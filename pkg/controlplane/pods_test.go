package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// podReadTime is the clock every pod projection case reads on.
var podReadTime = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// podObjectSource serves the runtime fixtures of the projection cases. Pods
// are not kernel objects, so it matches on kind and namespace alone.
type podObjectSource struct {
	objects []ObservedObject
}

// List implements ObjectSource.
func (s podObjectSource) List(_ context.Context, objectType ObjectType, namespace string) ([]ObservedObject, error) {
	matched := make([]ObservedObject, 0, len(s.objects))
	for _, object := range s.objects {
		if text(object.Object["kind"]) != objectType.Kind {
			continue
		}
		if objectType.Namespaced && namespace != "" && nestedText(object.Object, "metadata", "namespace") != namespace {
			continue
		}
		matched = append(matched, object)
	}
	return matched, nil
}

// Get implements ObjectSource.
func (s podObjectSource) Get(_ context.Context, objectType ObjectType, namespace, name string) (ObservedObject, error) {
	for _, object := range s.objects {
		if text(object.Object["kind"]) != objectType.Kind || nestedText(object.Object, "metadata", "name") != name {
			continue
		}
		if objectType.Namespaced && namespace != "" && nestedText(object.Object, "metadata", "namespace") != namespace {
			continue
		}
		return object, nil
	}
	return ObservedObject{}, apierrors.NewNotFound(schema.GroupResource{Resource: "objects"}, name)
}

// runtimeApplication is a stored application whose render wrote one Deployment
// of the same name.
func runtimeApplication(name string, paused bool) map[string]any {
	return map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec":       map[string]any{"namespace": "default", "paused": paused},
		"status": map[string]any{"rendered": []any{map[string]any{
			"group": "apps", "version": "v1", "kind": "Deployment",
			"namespace": "default", "name": name,
		}}},
	}
}

// runtimeDeployment is the workload an application renders.
func runtimeDeployment(name string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
		},
	}
}

// runtimePod is one Pod with the status the projection reads.
func runtimePod(name, label string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": "default", "uid": name + "-uid",
			"labels": map[string]any{"app": label},
		},
		"spec": map[string]any{
			"nodeName": "node-a",
			"containers": []any{
				map[string]any{"name": "app", "image": "nginx:1.27"},
				map[string]any{"name": "sidecar", "image": "busybox:1.36"},
			},
		},
		"status": map[string]any{
			"phase":     "Running",
			"startTime": podReadTime.Add(-time.Hour).Format(time.RFC3339Nano),
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "True",
			}},
			"containerStatuses": []any{
				map[string]any{"name": "app", "restartCount": int64(2)},
				map[string]any{"name": "sidecar", "restartCount": int64(1)},
			},
		},
	}
}

// podProjectionServer serves one API over the given runtime source.
func podProjectionServer(t *testing.T, source ObjectSource) *httptest.Server {
	t.Helper()
	server, err := New(Config{
		Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		Source:  source,
		Now:     func() time.Time { return podReadTime },
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

// readPodProjection calls the projection route and decodes the answer.
func readPodProjection(t *testing.T, server *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	response, err := http.Get(server.URL + path)
	if err != nil {
		t.Fatalf("read the pod projection: %v", err)
	}
	defer response.Body.Close()
	body := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode the pod projection: %v", err)
	}
	return response.StatusCode, body
}

// TestPodProjectionCarriesTheListedFactsAndTheStaleMarker checks C-76's item
// fields and C-13's one freshness bound across the exact threshold.
func TestPodProjectionCarriesTheListedFactsAndTheStaleMarker(t *testing.T) {
	otherPod := runtimePod("blog-0", "blog")
	fresh := runtimePod("shop-0", "shop")
	boundary := runtimePod("shop-1", "shop")
	stale := runtimePod("shop-2", "shop")
	source := podObjectSource{objects: []ObservedObject{
		{Object: runtimeApplication("shop", false), ObservedAt: podReadTime},
		{Object: runtimeDeployment("shop"), ObservedAt: podReadTime},
		{Object: fresh, ObservedAt: podReadTime.Add(-30 * time.Second)},
		{Object: boundary, ObservedAt: podReadTime.Add(-StaleThreshold)},
		{Object: stale, ObservedAt: podReadTime.Add(-StaleThreshold - time.Nanosecond)},
		{Object: otherPod, ObservedAt: podReadTime},
	}}
	server := podProjectionServer(t, source)
	status, body := readPodProjection(t, server, "/api/v1/pods/default/shop")
	if status != http.StatusOK {
		t.Fatalf("read = %d %v", status, body)
	}
	raw, _ := body["items"].([]any)
	items := make(map[string]map[string]any, len(raw))
	for _, entry := range raw {
		item, _ := entry.(map[string]any)
		items[text(item["name"])] = item
	}
	if len(items) != 3 {
		t.Fatalf("items = %v, want the application's three Pods", raw)
	}
	if _, carried := items["blog-0"]; carried {
		t.Fatalf("the projection carried another application's Pod: %v", raw)
	}
	for name, wantStale := range map[string]bool{"shop-0": false, "shop-1": false, "shop-2": true} {
		item := items[name]
		if item == nil {
			t.Fatalf("missing item %s", name)
		}
		if item["stale"] != wantStale {
			t.Fatalf("%s stale = %v, want %v", name, item["stale"], wantStale)
		}
		if item["sourceType"] != PodSourceType {
			t.Fatalf("%s sourceType = %v, want %s", name, item["sourceType"], PodSourceType)
		}
		if got := text(item["node"]); got != "node-a" {
			t.Fatalf("%s node = %q, want node-a", name, got)
		}
		if item["restarts"] != float64(3) {
			t.Fatalf("%s restarts = %v, want the sum over containers", name, item["restarts"])
		}
		if item["ready"] != true {
			t.Fatalf("%s ready = %v, want true", name, item["ready"])
		}
		if item["startedAt"] != podReadTime.Add(-time.Hour).Format(time.RFC3339Nano) {
			t.Fatalf("%s startedAt = %v, want the runtime start time", name, item["startedAt"])
		}
		containers, _ := item["containers"].([]any)
		if len(containers) != 2 {
			t.Fatalf("%s containers = %v, want the spec containers", name, containers)
		}
		first, _ := containers[0].(map[string]any)
		if text(first["name"]) != "app" || text(first["image"]) != "nginx:1.27" {
			t.Fatalf("%s first container = %v, want app with its image", name, first)
		}
	}
	if observed, ok := items["shop-0"]["observedAt"].(string); !ok || observed != podReadTime.Add(-30*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("shop-0 observedAt = %v, want the source's observation time", items["shop-0"]["observedAt"])
	}
}

// TestStoppedApplicationProjectsNoPods checks C-76's stopped-application rule:
// the stored paused marker empties the projection even while the runtime still
// holds Pods.
func TestStoppedApplicationProjectsNoPods(t *testing.T) {
	source := podObjectSource{objects: []ObservedObject{
		{Object: runtimeApplication("shop", true), ObservedAt: podReadTime},
		{Object: runtimeDeployment("shop"), ObservedAt: podReadTime},
		{Object: runtimePod("shop-0", "shop"), ObservedAt: podReadTime},
	}}
	server := podProjectionServer(t, source)
	status, body := readPodProjection(t, server, "/api/v1/pods/default/shop")
	if status != http.StatusOK {
		t.Fatalf("read = %d %v", status, body)
	}
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("the response carries no items list: %v", body)
	}
	if len(raw) != 0 {
		t.Fatalf("a stopped application projected %v, want an empty list", raw)
	}
}
