package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/reconcile"
	"github.com/tsic404/kosd/pkg/state"
	"github.com/tsic404/kosd/pkg/write"
)

// orphanStore serves the objects an uninstall round touches and records what
// the round writes.
type orphanStore struct {
	Store
	objects    map[string]*unstructured.Unstructured
	statuses   []*unstructured.Unstructured
	records    []*unstructured.Unstructured
	finalizers map[string][]string
	deleted    []string
}

// Get returns the stored object of one identity.
func (s *orphanStore) Get(_ context.Context, ref write.Ref) (*unstructured.Unstructured, error) {
	object, ok := s.objects[ref.Kind+"|"+ref.Namespace+"|"+ref.Name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: ref.Kind}, ref.Name)
	}
	return object.DeepCopy(), nil
}

// ApplyStatus records one status write.
func (s *orphanStore) ApplyStatus(_ context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	stored := document.DeepCopy()
	s.statuses = append(s.statuses, stored)
	return stored, nil
}

// CreateRecord records one immutable record write.
func (s *orphanStore) CreateRecord(_ context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	stored := document.DeepCopy()
	s.records = append(s.records, stored)
	return stored, nil
}

// Delete records one record consumption.
func (s *orphanStore) Delete(_ context.Context, _ string, ref write.Ref) error {
	s.deleted = append(s.deleted, ref.Kind+"|"+ref.Namespace+"|"+ref.Name)
	return nil
}

// SetFinalizers records one finalizer release.
func (s *orphanStore) SetFinalizers(_ context.Context, ref write.Ref, finalizers []string) (*unstructured.Unstructured, error) {
	key := ref.Kind + "|" + ref.Namespace + "|" + ref.Name
	s.finalizers[key] = append([]string(nil), finalizers...)
	if object, ok := s.objects[key]; ok {
		object.SetFinalizers(finalizers)
	}
	return s.objects[key], nil
}

// fixedGate answers every reference read with a fixed referrer set.
type fixedGate struct {
	referrers []v1beta1.ObjectRef
}

// Referrers implements ReferenceGate.
func (g fixedGate) Referrers(context.Context, write.Ref) ([]v1beta1.ObjectRef, error) {
	return g.referrers, nil
}

// orphanApplication builds one stored application carrying the uninstall gate.
func orphanApplication(name, resourceVersion string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name": name, "namespace": "default", "resourceVersion": resourceVersion,
		},
	}}
}

// orphanDownstream builds one downstream object the cluster still holds.
func orphanDownstream(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
	}}
}

// orphanRecord builds the one-shot action record an orphan round consumes.
func orphanRecord(uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": ActionRecordName(uid, ActionOrphan), "namespace": api.SystemNamespace,
		},
	}}
}

// decodeStatus reads the application status a status write carried.
func decodeStatus(t *testing.T, document *unstructured.Unstructured) v1beta1.ApplicationStatus {
	t.Helper()
	raw, found, err := unstructured.NestedMap(document.Object, "status")
	if err != nil || !found {
		t.Fatalf("status document carries no status: %v", document.Object)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode the status: %v", err)
	}
	var status v1beta1.ApplicationStatus
	if err := json.Unmarshal(encoded, &status); err != nil {
		t.Fatalf("decode the status: %v", err)
	}
	return status
}

// TestOrphanUninstallStaysBlockedByServedReference covers C-19/C-33: an orphan
// gives up the blocked downstream objects, but a served extension reference
// keeps the uninstall gate exactly as an ordinary uninstall does.
func TestOrphanUninstallStaysBlockedByServedReference(t *testing.T) {
	entry := v1beta1.ObjectRef{
		Group: "desktop.example.test", Version: "v1", Kind: "DesktopEntry",
		Namespace: "desktop", Name: "shop",
	}
	store := &orphanStore{
		objects:    map[string]*unstructured.Unstructured{"Application|default|shop": orphanApplication("shop", "7")},
		finalizers: map[string][]string{},
	}
	application := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name: "shop", Namespace: "default", UID: "uid-1", Generation: 1,
			Finalizers: []string{Finalizer}, ResourceVersion: "7",
		},
	}
	controller := NewController(store, Options{References: fixedGate{referrers: []v1beta1.ObjectRef{entry}}})

	result, err := controller.uninstall(context.Background(), application, ActionOrphan, orphanRecord("uid-1"), time.Now())
	if err != nil {
		t.Fatalf("orphan uninstall: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("a blocked uninstall must requeue")
	}
	if len(store.statuses) != 1 {
		t.Fatalf("statuses = %d, want the blocked status", len(store.statuses))
	}
	status := decodeStatus(t, store.statuses[0])
	if status.Phase != PhaseUninstalling {
		t.Fatalf("phase = %q, want %q", status.Phase, PhaseUninstalling)
	}
	condition, found := state.Find(status.Conditions, ConditionUninstallBlocked)
	if !found || condition.Status != metav1.ConditionTrue {
		t.Fatalf("UninstallBlocked = %+v, want True", condition)
	}
	if len(status.Uninstall.Blocked) != 1 || status.Uninstall.Blocked[0] != entry {
		t.Fatalf("blocked = %v, want the desktop entry", status.Uninstall.Blocked)
	}
	if len(store.records) != 0 {
		t.Fatal("a blocked orphan wrote an abandonment manifest")
	}
	if len(store.finalizers) != 0 {
		t.Fatal("a blocked orphan released the gate")
	}
	if len(store.deleted) != 0 {
		t.Fatal("a blocked orphan consumed the action record")
	}
}

// TestOrphanAbandonmentListsTheDownstreamStillInTheCluster covers C-33's
// manifest rule on the path a reference block leaves: the earlier blocked list
// names only the referrers, so the entries come from the cluster as it stands.
// The downstream object that is still there is recorded, one that is already
// gone is not, and the referrer never enters the manifest.
func TestOrphanAbandonmentListsTheDownstreamStillInTheCluster(t *testing.T) {
	referrer := v1beta1.ObjectRef{
		Group: "desktop.example.test", Version: "v1", Kind: "DesktopEntry",
		Namespace: "desktop", Name: "held",
	}
	deployment := v1beta1.ObjectRef{
		Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "default", Name: "shop",
	}
	gone := v1beta1.ObjectRef{
		Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "default", Name: "shop-old",
	}
	store := &orphanStore{
		objects: map[string]*unstructured.Unstructured{
			"Application|default|shop": orphanApplication("shop", "7"),
			"Deployment|default|shop":  orphanDownstream("shop"),
		},
		finalizers: map[string][]string{},
	}
	application := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name: "shop", Namespace: "default", UID: "uid-1", Generation: 1,
			Finalizers: []string{Finalizer}, ResourceVersion: "7",
		},
		Spec: v1beta1.ApplicationSpec{Namespace: "default", Volumes: []string{"data"}},
		Status: v1beta1.ApplicationStatus{
			Rendered:  []v1beta1.ObjectRef{deployment, gone},
			Uninstall: &v1beta1.UninstallStatus{Blocked: []v1beta1.ObjectRef{referrer}},
		},
	}
	controller := NewController(store, Options{References: fixedGate{}})

	result, err := controller.uninstall(context.Background(), application, ActionOrphan, orphanRecord("uid-1"), time.Now())
	if err != nil {
		t.Fatalf("orphan uninstall: %v", err)
	}
	if result != (reconcile.Result{}) {
		t.Fatalf("a completed orphan must not requeue: %+v", result)
	}
	if len(store.records) != 1 {
		t.Fatalf("records = %d, want the abandonment manifest", len(store.records))
	}
	text, _, err := unstructured.NestedString(store.records[0].Object, "data", AbandonmentKey)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	entries, err := DecodeAbandonment(text)
	if err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want the entry abandoned and the volume retained", entries)
	}
	if entries[0].Object != deployment || entries[0].Disposition != DispositionAbandoned {
		t.Fatalf("entry = %+v, want the surviving downstream abandoned", entries[0])
	}
	wantVolume := v1beta1.ObjectRef{
		Group: api.Group, Version: "v1beta1", Kind: "Volume",
		Namespace: "default", Name: "data",
	}
	if entries[1].Object != wantVolume || entries[1].Disposition != DispositionRetained {
		t.Fatalf("volume = %+v, want the declared volume retained", entries[1])
	}
	for _, entry := range entries {
		if entry.Object == referrer || entry.Object == gone {
			t.Fatalf("the manifest lists an object the orphan does not abandon: %+v", entry)
		}
	}
	if got := store.finalizers["Application|default|shop"]; len(got) != 0 {
		t.Fatalf("finalizers = %v, want the gate released", got)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "ConfigMap|"+api.SystemNamespace+"|"+ActionRecordName("uid-1", ActionOrphan) {
		t.Fatalf("deleted = %v, want the orphan record consumed", store.deleted)
	}
}

// TestReleaseFinalizerHoldsTheGateWhenAReferenceAppeared covers C-19's release
// end: the Application's version is not the proof, because storing a reference
// never changes it. A reference that is already in the cluster when the release
// scans again keeps the gate, and the round reports the block.
func TestReleaseFinalizerHoldsTheGateWhenAReferenceAppeared(t *testing.T) {
	entry := v1beta1.ObjectRef{
		Group: "desktop.example.test", Version: "v1", Kind: "DesktopEntry",
		Namespace: "desktop", Name: "alpha",
	}
	store := &orphanStore{
		objects:    map[string]*unstructured.Unstructured{"Application|default|shop": orphanApplication("shop", "7")},
		finalizers: map[string][]string{},
	}
	controller := NewController(store, Options{References: fixedGate{referrers: []v1beta1.ObjectRef{entry}}})
	scanned := &v1beta1.Application{ObjectMeta: metav1.ObjectMeta{
		Name: "shop", Namespace: "default", ResourceVersion: "7",
		Finalizers: []string{Finalizer},
	}}

	result, err := controller.releaseFinalizer(context.Background(), scanned, nil)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("a release behind a stored reference must requeue")
	}
	if len(store.finalizers) != 0 {
		t.Fatalf("finalizers = %v, want the gate left for the next round", store.finalizers)
	}
	if len(store.statuses) != 1 {
		t.Fatalf("statuses = %d, want the blocked status", len(store.statuses))
	}
	status := decodeStatus(t, store.statuses[0])
	if status.Uninstall == nil || len(status.Uninstall.Blocked) != 1 || status.Uninstall.Blocked[0] != entry {
		t.Fatalf("blocked = %+v, want the stored reference", status.Uninstall)
	}
}

// TestReleaseFinalizerReleasesWithoutReferences covers the pass-through: with
// no served reference the gate opens.
func TestReleaseFinalizerReleasesWithoutReferences(t *testing.T) {
	store := &orphanStore{
		objects:    map[string]*unstructured.Unstructured{"Application|default|shop": orphanApplication("shop", "7")},
		finalizers: map[string][]string{},
	}
	controller := NewController(store, Options{References: fixedGate{}})
	scanned := &v1beta1.Application{ObjectMeta: metav1.ObjectMeta{
		Name: "shop", Namespace: "default", ResourceVersion: "7",
		Finalizers: []string{Finalizer, "other"},
	}}

	result, err := controller.releaseFinalizer(context.Background(), scanned, nil)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("requeue = %s, want an immediate release", result.RequeueAfter)
	}
	if got := store.finalizers["Application|default|shop"]; len(got) != 1 || got[0] != "other" {
		t.Fatalf("finalizers = %v, want only the uninstall gate removed", got)
	}
}
