// Package kosdtc168 verifies the forbidden runtime writes (KOSD-F-20,
// KOSD-AC-75, KOSD-TC-168): there is no way to create a Pod, to rewrite a
// Pod's spec, to open a terminal or to forward a port; the attempts are
// rejected or have no route, and the Pod and workload stay as they were.
package kosdtc168

import (
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// TestTC168NoPodWriteSurface enumerates the runtime write surface a Pod page
// must not offer.
func TestTC168NoPodWriteSurface(t *testing.T) {
	cluster := harness.Start(t)
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: harness.RuntimeApplication("shop", map[string]any{
		"owner": "alice",
	}, "shop")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, harness.RuntimeDeployment("shop", 1))
	cluster.CreatePod(t, harness.PodFixture{Name: "shop-0", Labels: map[string]string{"app": "shop"}, Ready: true})

	gateway := cluster.StartControlPlane(t, nil)
	applicationPath := "/api/v1/pods/" + harness.Namespace + "/shop"
	pod := map[string]any{
		"kind":     "Pod",
		"metadata": map[string]any{"name": "shop-9", "namespace": harness.Namespace},
		"spec": map[string]any{"containers": []any{map[string]any{
			"name": "app", "image": "alpine:3.20",
		}}},
	}

	// Creating a Pod has no route: the kind is outside the closed kernel model.
	status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Pod?namespace="+harness.Namespace, pod, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("create a Pod = %d %v, want 400", status, body)
	}
	if failureCode(t, body) != controlplane.CodeInvalidField {
		t.Fatalf("create a Pod code = %v, want %s", failureCode(t, body), controlplane.CodeInvalidField)
	}

	// Rewriting a Pod's spec, including its image, has no route either.
	updated := map[string]any{
		"kind":     "Pod",
		"metadata": map[string]any{"name": "shop-0", "namespace": harness.Namespace},
		"spec": map[string]any{"containers": []any{map[string]any{
			"name": "app", "image": "alpine:3.20",
		}}},
	}
	status, body = gateway.Call(t, http.MethodPut, "/api/v1/resources/Pod/shop-0?namespace="+harness.Namespace, updated, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("rewrite a Pod = %d %v, want 400", status, body)
	}
	if failureCode(t, body) != controlplane.CodeInvalidField {
		t.Fatalf("rewrite a Pod code = %v, want %s", failureCode(t, body), controlplane.CodeInvalidField)
	}

	// Terminal and port forwarding do not exist.
	for _, path := range []string{
		applicationPath + "/shop-0/exec",
		applicationPath + "/shop-0/port-forward",
		"/api/v1/resources/Pod/" + harness.Namespace + "/shop-0/exec",
	} {
		status, body = gateway.Call(t, http.MethodPost, path, nil, nil)
		if status != http.StatusNotFound {
			t.Fatalf("%s = %d %v, want 404", path, status, body)
		}
	}

	// The Pod surface exposes only the projection read and the single delete;
	// every other method on it falls through to the API's not-found answer.
	for _, attempt := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: applicationPath},
		{method: http.MethodPut, path: applicationPath + "/shop-0"},
	} {
		status, body = gateway.Call(t, attempt.method, attempt.path, nil, nil)
		if status != http.StatusNotFound {
			t.Fatalf("%s %s = %d %v, want 404", attempt.method, attempt.path, status, body)
		}
		if failureCode(t, body) != controlplane.CodeNotFound {
			t.Fatalf("%s %s code = %v, want %s", attempt.method, attempt.path, failureCode(t, body), controlplane.CodeNotFound)
		}
	}

	// The Pod and the workload are unchanged, and no Pod was created.
	items := cluster.Pods(t, harness.Namespace)
	if len(items) != 1 {
		t.Fatalf("namespace holds %d Pods, want only the fixture", len(items))
	}
	containers, _, _ := unstructured.NestedSlice(items[0].Object, "spec", "containers")
	first, _ := containers[0].(map[string]any)
	if image := harness.String(first, "image"); image != "nginx:1.27" {
		t.Fatalf("the Pod image = %q, want the untouched fixture image", image)
	}
	if replicas := cluster.WorkloadReplicas(t, "Deployment", harness.Namespace, "shop"); replicas != 1 {
		t.Fatalf("the workload replicas = %d, want the untouched fixture count", replicas)
	}
}

// failureCode reads the stable code of one failure envelope.
func failureCode(t *testing.T, body map[string]any) string {
	t.Helper()
	errorBody, _ := body["error"].(map[string]any)
	code, _ := errorBody["code"].(string)
	return code
}
