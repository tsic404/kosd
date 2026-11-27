package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/api/v1beta1"
)

// referenceKinds serves one class whose objects declare one reference path.
func referenceKinds() extensionKinds {
	return extensionKinds{
		{Group: "desktop.example.test", Kind: "DesktopEntry"}: {
			Versions: []string{"v1"}, Resource: "desktopentries", Namespaced: true,
			References: []string{"spec.applicationRef"},
		},
	}
}

// referenceWorld builds a fake cluster holding the kernel Application and the
// extension class resource the guard reads and writes. A nil application
// leaves the cluster without one.
func referenceWorld(t *testing.T, application *unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(v1beta1.GroupVersion.WithKind("Application"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersion{Version: "v1"}.WithKind("ConfigMap"), &unstructured.Unstructured{})
	entryGV := schema.GroupVersion{Group: "desktop.example.test", Version: "v1"}
	scheme.AddKnownTypeWithName(entryGV.WithKind("DesktopEntry"), &unstructured.Unstructured{})
	objects := []runtime.Object{}
	if application != nil {
		objects = append(objects, application)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: api.Group, Version: v1beta1.GroupVersion.Version, Resource: "applications"}: "ApplicationList",
		{Group: "desktop.example.test", Version: "v1", Resource: "desktopentries"}:          "DesktopEntryList",
		{Version: "v1", Resource: "configmaps"}:                                             "ConfigMapList",
	}, objects...)
}

// referenceServer builds a control plane serving the reference class over one
// fake cluster.
func referenceServer(t *testing.T, client dynamic.Interface) *Server {
	t.Helper()
	server, err := New(Config{Dynamic: client, Extensions: referenceKinds()})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	t.Cleanup(server.Close)
	return server
}

// entryObject builds one desktop entry body naming one application.
func entryObject(namespace, name, applicationNamespace, applicationName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "desktop.example.test/v1",
		"kind":       "DesktopEntry",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{"applicationRef": map[string]any{
			"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
			"kind":       "Application",
			"namespace":  applicationNamespace,
			"name":       applicationName,
		}},
	}}
}

// applicationObject builds one kernel application, deleting or not.
func applicationObject(namespace, name string, deleting bool) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": namespace, "resourceVersion": "7"},
	}}
	if deleting {
		object.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	}
	return object
}

// applicationGVR is the resource identity the guard reads Applications from.
func applicationGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Resource: "applications",
	}
}

// TestGuardReferencesRefusesNewReferenceToRemovingApplication checks C-19's
// new-reference rule: a write that would add a declarative reference to an
// Application whose removal already started is refused.
func TestGuardReferencesRefusesNewReferenceToRemovingApplication(t *testing.T) {
	client := referenceWorld(t, applicationObject("default", "shop", true))
	server := referenceServer(t, client)
	failure := server.guardReferences(context.Background(),
		entryObject("desktop", "one", "default", "shop"), nil)
	if failure == nil || failure.Code != CodeReferenceRefused {
		t.Fatalf("failure = %v, want %s", failure, CodeReferenceRefused)
	}
	if failure.Stage != StageValidate {
		t.Fatalf("stage = %s, want %s", failure.Stage, StageValidate)
	}
}

// TestGuardReferencesAllowsReferenceToLiveApplication checks the other half: a
// reference to an Application that is not being removed passes and is tracked
// for the read after the write.
func TestGuardReferencesAllowsReferenceToLiveApplication(t *testing.T) {
	client := referenceWorld(t, applicationObject("default", "shop", false))
	server := referenceServer(t, client)
	failure := server.guardReferences(context.Background(),
		entryObject("desktop", "one", "default", "shop"), nil)
	if failure != nil {
		t.Fatalf("guard over a live application: %v", failure)
	}
}

// TestGuardReferencesAllowsReferenceToMissingApplication checks that a
// reference to an object that does not exist yet names nothing to protect.
func TestGuardReferencesAllowsReferenceToMissingApplication(t *testing.T) {
	client := referenceWorld(t, nil)
	server := referenceServer(t, client)
	failure := server.guardReferences(context.Background(),
		entryObject("desktop", "one", "default", "shop"), nil)
	if failure != nil {
		t.Fatalf("guard over a missing application: %v", failure)
	}
}

// TestGuardReferencesIgnoresStoredReferences checks the update path: a
// reference the object already carries is not new, so it keeps blocking the
// removal as before instead of being refused again.
func TestGuardReferencesIgnoresStoredReferences(t *testing.T) {
	client := referenceWorld(t, applicationObject("default", "shop", true))
	server := referenceServer(t, client)
	stored := entryObject("desktop", "one", "default", "shop")
	failure := server.guardReferences(context.Background(),
		entryObject("desktop", "one", "default", "shop"), stored)
	if failure != nil {
		t.Fatalf("a stored reference must not be refused: %v", failure)
	}
}

// TestGuardReferencesRefusesUpdateThatAddsReferenceToRemovingApplication checks
// the other update path: the object references a live application and the update
// moves that reference onto one whose removal started. The update adds the edge,
// so it is refused even though the object already carried a reference.
func TestGuardReferencesRefusesUpdateThatAddsReferenceToRemovingApplication(t *testing.T) {
	client := referenceWorld(t, applicationObject("default", "shop", true))
	server := referenceServer(t, client)
	stored := entryObject("desktop", "one", "default", "billing")
	failure := server.guardReferences(context.Background(),
		entryObject("desktop", "one", "default", "shop"), stored)
	if failure == nil || failure.Code != CodeReferenceRefused {
		t.Fatalf("failure = %v, want %s", failure, CodeReferenceRefused)
	}
}

// entryPath is the single-object route of one desktop entry.
const entryPath = "/api/v1/resources/DesktopEntry"

// call sends one request to the server's handler and decodes the answer.
func call(t *testing.T, server *Server, method, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the request body: %v", err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	decoded := map[string]any{}
	if len(bytes.TrimSpace(recorder.Body.Bytes())) > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("%s %s answered %q: %v", method, path, recorder.Body.String(), err)
		}
	}
	return recorder.Code, decoded
}

// entryRoute is the single-object route of one entry in one namespace.
func entryRoute(name, namespace string) string {
	return entryPath + "/" + name + "?apiVersion=desktop.example.test/v1&namespace=" + namespace
}

// startRemoval marks one application as being removed, as the API server does
// when it accepts a delete request with a finalizer in place.
func startRemoval(t *testing.T, client dynamic.Interface) {
	t.Helper()
	ctx := context.Background()
	application, err := client.Resource(applicationGVR()).Namespace("default").Get(ctx, "shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the application: %v", err)
	}
	application.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	if _, err := client.Resource(applicationGVR()).Namespace("default").Update(ctx, application, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("start the removal: %v", err)
	}
}

// TestRepeatCreateWithAStoredReferenceIsIdempotent covers C-12 on the C-19
// path: a POST that repeats an existing object introduces no reference, so it
// answers 202 without rewriting the object, while a new name is still refused.
func TestRepeatCreateWithAStoredReferenceIsIdempotent(t *testing.T) {
	fake := referenceWorld(t, applicationObject("default", "shop", false))
	client := &countingClient{Interface: fake}
	server := referenceServer(t, client)
	body := entryObject("desktop", "one", "default", "shop").Object
	if status, answer := call(t, server, http.MethodPost,
		entryPath+"?apiVersion=desktop.example.test/v1&namespace=desktop", body, nil); status != http.StatusAccepted {
		t.Fatalf("first create: status %d, body %v, want 202", status, answer)
	}
	writes := client.entryWrites

	startRemoval(t, fake)

	status, answer := call(t, server, http.MethodPost,
		entryPath+"?apiVersion=desktop.example.test/v1&namespace=desktop", body, nil)
	if status != http.StatusAccepted {
		t.Fatalf("repeat create: status %d, body %v, want 202", status, answer)
	}
	if answer["idempotent"] != true {
		t.Fatalf("repeat create: body %v, want idempotent", answer)
	}
	if client.entryWrites != writes {
		t.Fatalf("the repeat create wrote the entry: %d writes, want %d", client.entryWrites, writes)
	}

	newEntry := entryObject("desktop", "two", "default", "shop").Object
	status, answer = call(t, server, http.MethodPost,
		entryPath+"?apiVersion=desktop.example.test/v1&namespace=desktop", newEntry, nil)
	if status != http.StatusConflict {
		t.Fatalf("create entry two: status %d, body %v, want 409", status, answer)
	}
	if failure, _ := answer["error"].(map[string]any); failure["code"] != CodeReferenceRefused {
		t.Fatalf("create entry two: error = %v, want %s", answer["error"], CodeReferenceRefused)
	}
}

// TestRefusedUpdateKeepsAConcurrentWrite covers C-05/C-12: the refused update
// writes nothing, so a write another client already had accepted keeps its
// fields and the failure reports no side effects.
func TestRefusedUpdateKeepsAConcurrentWrite(t *testing.T) {
	fake := referenceWorld(t, applicationObject("default", "shop", false))
	client := &countingClient{Interface: fake}
	server := referenceServer(t, client)
	ctx := context.Background()
	stored := entryObject("desktop", "one", "default", "billing")
	stored.SetResourceVersion("3")
	if _, err := client.Resource(desktopEntryGVR()).Namespace("desktop").Create(ctx, stored, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the entry: %v", err)
	}
	// Another client's update lands first and is accepted with its own version.
	concurrent, err := client.Resource(desktopEntryGVR()).Namespace("desktop").Get(ctx, "one", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the entry: %v", err)
	}
	concurrent.SetLabels(map[string]string{"stage": "concurrent"})
	concurrent.SetResourceVersion("4")
	accepted, err := client.Resource(desktopEntryGVR()).Namespace("desktop").Update(ctx, concurrent, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("write the concurrent update: %v", err)
	}
	writes := client.entryWrites

	startRemoval(t, fake)

	moved := entryObject("desktop", "one", "default", "shop").Object
	moved["metadata"].(map[string]any)["labels"] = map[string]any{"stage": "concurrent"}
	status, answer := call(t, server, http.MethodPut, entryRoute("one", "desktop"), moved,
		map[string]string{"If-Match": accepted.GetResourceVersion()})
	if status != http.StatusConflict {
		t.Fatalf("update entry one: status %d, body %v, want 409", status, answer)
	}
	failure, _ := answer["error"].(map[string]any)
	if failure["code"] != CodeReferenceRefused {
		t.Fatalf("update entry one: error = %v, want %s", answer["error"], CodeReferenceRefused)
	}
	if failure["sideEffects"] != string(SideEffectsNone) {
		t.Fatalf("update entry one: sideEffects = %v, want %s", failure["sideEffects"], SideEffectsNone)
	}
	after, err := client.Resource(desktopEntryGVR()).Namespace("desktop").Get(ctx, "one", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reread the entry: %v", err)
	}
	if after.GetLabels()["stage"] != "concurrent" {
		t.Fatalf("the refused update lost the concurrent write: labels = %v", after.GetLabels())
	}
	if name, _, _ := unstructured.NestedString(after.Object, "spec", "applicationRef", "name"); name != "billing" {
		t.Fatalf("the refused update moved the reference to %q, want billing", name)
	}
	if after.GetResourceVersion() != accepted.GetResourceVersion() {
		t.Fatalf("the refused update rewrote the entry: resourceVersion %s -> %s",
			accepted.GetResourceVersion(), after.GetResourceVersion())
	}
	if client.entryWrites != writes {
		t.Fatalf("the refused update wrote the entry: %d writes, want %d", client.entryWrites, writes)
	}
}

// TestApplyRefusesReferenceToRemovingApplication covers C-19 on the set path:
// a batch create that names an Application already being removed is refused
// before the request is accepted.
func TestApplyRefusesReferenceToRemovingApplication(t *testing.T) {
	client := referenceWorld(t, applicationObject("default", "shop", true))
	server := referenceServer(t, client)
	request := SetRequest{Objects: []map[string]any{entryObject("desktop", "two", "default", "shop").Object}}
	_, _, _, failure := server.buildPlan(context.Background(), context.Background(), VerbApply, request)
	if failure == nil || failure.Code != CodeReferenceRefused {
		t.Fatalf("plan = %v, want %s", failure, CodeReferenceRefused)
	}
	if _, err := client.Resource(desktopEntryGVR()).Namespace("desktop").Get(
		context.Background(), "two", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the refused plan stored the entry: %v", err)
	}
}

// TestApplyDoesNotStoreAReferenceWrittenAfterAcceptance covers the other half:
// a removal that starts after the set was accepted still keeps the reference
// out of the cluster.
func TestApplyDoesNotStoreAReferenceWrittenAfterAcceptance(t *testing.T) {
	client := referenceWorld(t, applicationObject("default", "shop", false))
	server := referenceServer(t, client)
	ctx := context.Background()
	request := SetRequest{Objects: []map[string]any{entryObject("desktop", "two", "default", "shop").Object}}
	plan, desired, grants, failure := server.buildPlan(ctx, ctx, VerbApply, request)
	if failure != nil {
		t.Fatalf("plan: %v", failure)
	}

	startRemoval(t, client)

	server.execute(ctx, plan, desired, grants)
	if _, err := client.Resource(desktopEntryGVR()).Namespace("desktop").Get(ctx, "two", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the accepted set stored the entry after the removal started: %v", err)
	}
}

// desktopEntryGVR is the resource the test class is served under.
func desktopEntryGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "desktop.example.test", Version: "v1", Resource: "desktopentries"}
}

// countingClient counts the writes the server sends to the test class, so a
// case can tell a refusal that wrote nothing from one that left a trace.
type countingClient struct {
	dynamic.Interface
	entryWrites int
}

// Resource implements dynamic.Interface.
func (c *countingClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	resource := c.Interface.Resource(gvr)
	if gvr.Group != "desktop.example.test" {
		return resource
	}
	return &countingResource{NamespaceableResourceInterface: resource, counter: c}
}

// countingResource counts the writes of the wrapped resource.
type countingResource struct {
	dynamic.NamespaceableResourceInterface
	counter *countingClient
}

// Namespace implements dynamic.NamespaceableResourceInterface.
func (r *countingResource) Namespace(namespace string) dynamic.ResourceInterface {
	return &countingNamespaced{ResourceInterface: r.NamespaceableResourceInterface.Namespace(namespace), counter: r.counter}
}

// countingNamespaced counts the writes of one namespaced resource.
type countingNamespaced struct {
	dynamic.ResourceInterface
	counter *countingClient
}

// Create implements dynamic.ResourceInterface.
func (r *countingNamespaced) Create(ctx context.Context, object *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	r.counter.entryWrites++
	return r.ResourceInterface.Create(ctx, object, options, subresources...)
}

// Update implements dynamic.ResourceInterface.
func (r *countingNamespaced) Update(ctx context.Context, object *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	r.counter.entryWrites++
	return r.ResourceInterface.Update(ctx, object, options, subresources...)
}

// Delete implements dynamic.ResourceInterface.
func (r *countingNamespaced) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	r.counter.entryWrites++
	return r.ResourceInterface.Delete(ctx, name, options, subresources...)
}
