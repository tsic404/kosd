// Package kosdtc163 verifies the Pod projection's ownership and source
// (KOSD-AC-75, KOSD-TC-163): reading one application's projection returns only
// that application's Pods, each item carries the C-53 source type and the
// C-13 observation time, and the other application's Pods in the same
// namespace do not appear.
package kosdtc163

import (
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// startedAt is the start time both applications' Pods report.
var startedAt = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// TestTC163ProjectionOnlyContainsTheApplicationPods reads the first
// application's projection as the Owner and pins its items.
func TestTC163ProjectionOnlyContainsTheApplicationPods(t *testing.T) {
	cluster := harness.Start(t)
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: harness.RuntimeApplication("shop", nil, "shop")})
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: harness.RuntimeApplication("blog", nil, "blog")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, harness.RuntimeDeployment("shop", 2))
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, harness.RuntimeDeployment("blog", 2))
	cluster.CreatePod(t, harness.PodFixture{
		Name: "shop-0", Labels: map[string]string{"app": "shop"}, Node: "node-a",
		Ready: true, Restarts: 1, Image: "nginx:1.27", StartedAt: startedAt,
	})
	cluster.CreatePod(t, harness.PodFixture{
		Name: "shop-1", Labels: map[string]string{"app": "shop"}, Node: "node-b",
		Ready: true, StartedAt: startedAt,
	})
	cluster.CreatePod(t, harness.PodFixture{
		Name: "blog-0", Labels: map[string]string{"app": "blog"}, Node: "node-a",
		Ready: true, StartedAt: startedAt,
	})
	cluster.CreatePod(t, harness.PodFixture{
		Name: "blog-1", Labels: map[string]string{"app": "blog"}, Node: "node-b",
		Ready: true, StartedAt: startedAt,
	})

	system := cluster.StartAccess(t, nil, nil)
	owner := system.Owner(t, "owner", "owner-password")
	status, body := system.Call(t, owner, http.MethodGet, "/api/v1/pods/"+harness.Namespace+"/shop", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the pod projection = %d %v", status, body)
	}

	items := harness.Items(t, body)
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, harness.String(item, "name"))
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"shop-0", "shop-1"}) {
		t.Fatalf("projection names = %v, want only the application's own Pods", names)
	}

	for _, item := range items {
		if got := harness.String(item, "sourceType"); got != "object" {
			t.Fatalf("%s sourceType = %q, want the object source type", harness.String(item, "name"), got)
		}
		observedAt := harness.String(item, "observedAt")
		if observedAt == "" {
			t.Fatalf("%s carries no observation time", harness.String(item, "name"))
		}
		if _, err := time.Parse(time.RFC3339Nano, observedAt); err != nil {
			t.Fatalf("%s observedAt = %q is not a timestamp: %v", harness.String(item, "name"), observedAt, err)
		}
		if item["stale"] != false {
			t.Fatalf("%s stale = %v, want a fresh read", harness.String(item, "name"), item["stale"])
		}
		if got := harness.String(item, "phase"); got != "Running" {
			t.Fatalf("%s phase = %q, want Running", harness.String(item, "name"), got)
		}
		if item["ready"] != true {
			t.Fatalf("%s ready = %v, want true", harness.String(item, "name"), item["ready"])
		}
		if got := harness.String(item, "startedAt"); got != startedAt.Format(time.RFC3339Nano) {
			t.Fatalf("%s startedAt = %q, want the runtime start time", harness.String(item, "name"), got)
		}
		containers := containerItems(t, item)
		if len(containers) != 1 || harness.String(containers[0], "name") != "app" ||
			harness.String(containers[0], "image") != "nginx:1.27" {
			t.Fatalf("%s containers = %v, want the container and its image", harness.String(item, "name"), containers)
		}
		wantKeys := []string{"containers", "name", "node", "observedAt", "phase", "ready", "restarts", "sourceType", "stale", "startedAt"}
		if got := sortedKeys(item); !reflect.DeepEqual(got, wantKeys) {
			t.Fatalf("%s carries fields outside the projection: %v", harness.String(item, "name"), got)
		}
	}

	shopZero := itemNamed(t, items, "shop-0")
	if harness.String(shopZero, "node") != "node-a" {
		t.Fatalf("shop-0 node = %v, want node-a", shopZero["node"])
	}
	if shopZero["restarts"] != float64(1) {
		t.Fatalf("shop-0 restarts = %v, want 1", shopZero["restarts"])
	}
}

// itemNamed returns one projection item by Pod name.
func itemNamed(t *testing.T, items []map[string]any, name string) map[string]any {
	t.Helper()
	for _, item := range items {
		if harness.String(item, "name") == name {
			return item
		}
	}
	t.Fatalf("no item named %s in %v", name, items)
	return nil
}

// containerItems reads the containers of one projection item.
func containerItems(t *testing.T, item map[string]any) []map[string]any {
	t.Helper()
	raw, ok := item["containers"].([]any)
	if !ok {
		return nil
	}
	containers := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		container, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("container is not an object: %v", entry)
		}
		containers = append(containers, container)
	}
	return containers
}

// sortedKeys returns the keys of one item in sorted order.
func sortedKeys(item map[string]any) []string {
	keys := make([]string, 0, len(item))
	for key := range item {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
