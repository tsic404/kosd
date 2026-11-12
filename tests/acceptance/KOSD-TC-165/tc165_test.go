// Package kosdtc165 verifies the single Pod delete's effect on expected state
// (KOSD-AC-75, KOSD-TC-165): deleting one Pod of a running multi-replica
// application removes exactly that Pod, leaves the application spec, replica
// count and volumes field for field unchanged, leaves no drift record, lets the
// runtime replace the instance, and a stopped application projects no Pods.
package kosdtc165

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// TestTC165DeleteOnlyRemovesThatPod pins the delete against the expected state.
func TestTC165DeleteOnlyRemovesThatPod(t *testing.T) {
	cluster := harness.Start(t)
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: harness.RuntimeApplication("shop", map[string]any{
		"owner":    "alice",
		"volumes":  []any{"data"},
		"replicas": int64(3),
	}, "shop")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, harness.RuntimeDeployment("shop", 3))
	for _, name := range []string{"shop-0", "shop-1", "shop-2"} {
		cluster.CreatePod(t, harness.PodFixture{Name: name, Labels: map[string]string{"app": "shop"}, Ready: true})
	}

	gateway := cluster.StartControlPlane(t, nil)
	projectionPath := "/api/v1/pods/" + harness.Namespace + "/shop"

	specBefore := applicationSpec(t, cluster)
	workloadVersion := workloadResourceVersion(t, cluster)
	status, body := gateway.Call(t, http.MethodDelete, projectionPath+"/shop-0", nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("delete shop-0 = %d %v, want 202", status, body)
	}
	ref, _ := body["ref"].(map[string]any)
	if harness.String(ref, "kind") != "Pod" || harness.String(ref, "name") != "shop-0" {
		t.Fatalf("delete answer names %v, want the deleted Pod", ref)
	}

	// Exactly that Pod is gone, and the expected state is untouched.
	if podStillStored(t, cluster, "shop-0") {
		t.Fatal("the delete left the named Pod in place")
	}
	if specAfter := applicationSpec(t, cluster); !reflect.DeepEqual(specBefore, specAfter) {
		t.Fatalf("the delete changed the application spec: %v -> %v", specBefore, specAfter)
	}
	if version := workloadResourceVersion(t, cluster); version != workloadVersion {
		t.Fatalf("the delete changed the workload (resourceVersion %s -> %s)", workloadVersion, version)
	}
	assertNoDrift(t, cluster)

	// The runtime's workload controller creates the replacement; KOS creates
	// none, so the projection holds one Pod fewer until it appears.
	assertPodNames(t, gateway, projectionPath, []string{"shop-1", "shop-2"})
	cluster.CreatePod(t, harness.PodFixture{Name: "shop-3", Labels: map[string]string{"app": "shop"}, Ready: true})
	assertPodNames(t, gateway, projectionPath, []string{"shop-1", "shop-2", "shop-3"})

	// Stopping the application leaves no Pods to project.
	stopApplication(t, gateway)
	status, body = gateway.Call(t, http.MethodGet, projectionPath, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the stopped application's projection = %d %v", status, body)
	}
	if items := harness.Items(t, body); len(items) != 0 {
		t.Fatalf("a stopped application projected %v, want an empty list", items)
	}
}

// assertPodNames reads the projection and compares its Pod names.
func assertPodNames(t *testing.T, gateway *harness.ControlPlane, path string, want []string) {
	t.Helper()
	status, body := gateway.Call(t, http.MethodGet, path, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the projection = %d %v", status, body)
	}
	items := harness.Items(t, body)
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, harness.String(item, "name"))
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("projection names = %v, want %v", names, want)
	}
}

// stopApplication submits the stop action.
func stopApplication(t *testing.T, gateway *harness.ControlPlane) {
	t.Helper()
	status, body := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind": "Application", "namespace": harness.Namespace, "name": "shop", "action": "stop",
	}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("stop the application = %d %v, want 202", status, body)
	}
}

// applicationSpec reads the stored application's spec.
func applicationSpec(t *testing.T, cluster *harness.Cluster) map[string]any {
	t.Helper()
	spec, _, err := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", "shop").Object, "spec")
	if err != nil {
		t.Fatalf("read the application spec: %v", err)
	}
	return spec
}

// workloadResourceVersion reads the rendered Deployment's version, so an
// untouched workload is observable.
func workloadResourceVersion(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	return cluster.Workload(t, "Deployment", harness.Namespace, "shop").GetResourceVersion()
}

// assertNoDrift checks that neither the application nor its status carries a
// drift record after a Pod delete (C-35).
func assertNoDrift(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	application := cluster.Get(t, "v1beta1", "Application", "shop")
	if drift, found, _ := unstructured.NestedSlice(application.Object, "status", "drift"); found && len(drift) > 0 {
		t.Fatalf("the delete left a drift record: %v", drift)
	}
	conditions, _, _ := unstructured.NestedSlice(application.Object, "status", "conditions")
	for _, entry := range conditions {
		condition, _ := entry.(map[string]any)
		if harness.String(condition, "type") == "Drifted" {
			t.Fatalf("the delete left a Drifted condition: %v", condition)
		}
	}
}

// podStillStored reports whether one Pod is still in the cluster.
func podStillStored(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := cluster.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).
		Namespace(harness.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read pod %s: %v", name, err)
	}
	return true
}
