package kernel

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/reconcile"
	"github.com/tsic404/kosd/pkg/state"
)

// endpointResource is the class the deletion test sweeps.
var endpointResource = schema.GroupVersionResource{Group: api.Group, Version: "v1beta1", Resource: "endpoints"}

// recordingReconciler records the requests it received.
type recordingReconciler struct {
	requests chan reconcile.Request
}

// Reconcile records one request.
func (r *recordingReconciler) Reconcile(_ context.Context, request reconcile.Request) (reconcile.Result, error) {
	r.requests <- request
	return reconcile.Result{}, nil
}

// awaitRequest reads one request within the deadline.
func awaitRequest(t *testing.T, channel <-chan reconcile.Request) reconcile.Request {
	t.Helper()
	select {
	case request := <-channel:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a delivered round")
		return reconcile.Request{}
	}
}

// expectNoRequest requires the channel to stay empty for a short while.
func expectNoRequest(t *testing.T, channel <-chan reconcile.Request) {
	t.Helper()
	select {
	case request := <-channel:
		t.Fatalf("unexpected round delivered for %s/%s", request.Namespace, request.Name)
	case <-time.After(300 * time.Millisecond):
	}
}

// newSweepingLoops builds loops whose only observer is one recording reconciler
// over the endpoint class, with a fake cluster holding the given objects.
func newSweepingLoops(t *testing.T, objects ...*unstructured.Unstructured) (*kernelLoops, *recordingReconciler) {
	t.Helper()
	items := make([]runtime.Object, 0, len(objects))
	for _, object := range objects {
		items = append(items, object)
	}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{endpointResource: "EndpointList"}, items...)
	recorder := &recordingReconciler{requests: make(chan reconcile.Request, 32)}
	controller, err := reconcile.NewController(recorder, reconcile.Options{Workers: 1})
	if err != nil {
		t.Fatalf("build the recording controller: %v", err)
	}
	loops := &kernelLoops{
		config: loopConfig{
			Dynamic:    client,
			Projection: NewProjection(),
			Logger:     slog.New(slog.DiscardHandler),
		},
		hosted: map[string]string{},
		seen:   map[string]map[string]struct{}{},
	}
	loops.observers = append(loops.observers, observer{kind: "Endpoint", controller: controller})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return loops, recorder
}

// endpointObject builds one stored endpoint. Its content is irrelevant: the
// sweep delivers identifiers, not payloads.
func endpointObject(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Endpoint",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
	}}
}

// TestSweepDeliversDeletedObjectIdentifiers pins KOSD-C-79: the kernel has no
// watch source, so an object that disappeared must be delivered by the next
// sweep for its cleanup round to run at all. An implementation that only lists
// what exists never reaches that round.
func TestSweepDeliversDeletedObjectIdentifiers(t *testing.T) {
	loops, recorder := newSweepingLoops(t, endpointObject("web-route"))
	ctx := context.Background()

	loops.sweep(ctx)
	if request := awaitRequest(t, recorder.requests); request.Name != "web-route" {
		t.Fatalf("first sweep delivered %s, want web-route", request.Name)
	}

	if err := loops.config.Dynamic.Resource(endpointResource).Namespace("default").Delete(ctx, "web-route", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the endpoint: %v", err)
	}
	loops.sweep(ctx)
	if request := awaitRequest(t, recorder.requests); request.Name != "web-route" || request.Namespace != "default" {
		t.Fatalf("the sweep delivered %s/%s, want default/web-route", request.Namespace, request.Name)
	}

	// A deletion is delivered once: the next sweep has nothing left to report.
	loops.sweep(ctx)
	expectNoRequest(t, recorder.requests)
}

// TestSweepKeepsItsSnapshotWhenTheListFails pins KOSD-C-56 with C-79: a failed
// list is not a deletion. Treating it as one would tell every reconciler that
// its objects are gone and tear down the downstream objects of a healthy set.
func TestSweepKeepsItsSnapshotWhenTheListFails(t *testing.T) {
	loops, recorder := newSweepingLoops(t, endpointObject("web-route"))
	ctx := context.Background()

	loops.sweep(ctx)
	awaitRequest(t, recorder.requests)

	failing := loops.config.Dynamic.(*fake.FakeDynamicClient)
	failing.PrependReactor("list", "endpoints", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server is unreachable")
	})
	loops.sweep(ctx)
	expectNoRequest(t, recorder.requests)

	// The snapshot survives the failure, so the deletion is still delivered
	// once the list works again.
	failing.ReactionChain = failing.ReactionChain[1:]
	if err := loops.config.Dynamic.Resource(endpointResource).Namespace("default").Delete(ctx, "web-route", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the endpoint: %v", err)
	}
	loops.sweep(ctx)
	if request := awaitRequest(t, recorder.requests); request.Name != "web-route" {
		t.Fatalf("the recovered sweep delivered %s, want web-route", request.Name)
	}
}

// TestSyncControllersRegistersAndWithdrawsComponents pins C-20, C-28 and C-29:
// an active component's declared controller enters the set, a component that
// left the active set is withdrawn, and a declaration this process cannot
// execute is served but unavailable (its objects project Unknown) instead of
// being silently dropped.
func TestSyncControllersRegistersAndWithdrawsComponents(t *testing.T) {
	loops := &kernelLoops{
		config: loopConfig{
			Controllers: extension.NewControllerSet(1),
			Projection:  NewProjection(),
			Logger:      slog.New(slog.DiscardHandler),
		},
		hosted: map[string]string{},
		seen:   map[string]map[string]struct{}{},
	}
	widgets := extension.Controller{ID: "widget", Manages: []extension.GroupKind{{Group: "widgets.example.test", Kind: "Widget"}}}
	contribution := extension.ComponentResult{
		Name:       "builtin",
		Identifier: "demo.widgets",
		Phase:      extension.PhaseReady,
		Contributions: extension.Contributions{
			Controllers: []extension.Controller{widgets},
		},
	}

	loops.syncControllers(nil)
	if _, _, served := loops.config.Controllers.Serving("widgets.example.test", "Widget"); served {
		t.Fatal("an installation with no component served a class")
	}

	loops.syncControllers([]extension.ComponentResult{contribution})
	if controller, _, served := loops.config.Controllers.Serving("widgets.example.test", "Widget"); !served || controller != "builtin" {
		t.Fatalf("the active component's class is served by %q, want builtin", controller)
	}
	// This process links no implementation, so the declared controller is
	// unavailable and its objects project Unknown (C-20).
	projected := loops.config.Controllers.ProjectedStatus("widgets.example.test", "Widget",
		v1beta1.ObjectStatus{
			Health:     v1beta1.Health{State: v1beta1.HealthStateHealthy},
			Conditions: []metav1.Condition{state.Applied(1, "healthy")},
		})
	if projected.Health.State != "Unknown" {
		t.Fatalf("an unavailable controller's object health = %s, want Unknown", projected.Health.State)
	}

	contribution.Phase = extension.PhaseDisabled
	loops.syncControllers([]extension.ComponentResult{contribution})
	if _, _, served := loops.config.Controllers.Serving("widgets.example.test", "Widget"); served {
		t.Fatal("a withdrawn component still serves its class")
	}
}
