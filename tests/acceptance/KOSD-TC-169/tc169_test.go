// Package kosdtc169 verifies the Pod-narrowed log read (KOSD-AC-75,
// KOSD-TC-169): a read naming one Pod returns only that Pod's container lines,
// another Pod of the same application contributes nothing, and the line bound
// still truncates and names the position it was cut at.
package kosdtc169

import (
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// readTime is the clock the case reads on, so the fixture lines sit inside the
// window.
var readTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// TestTC169PodScopedLogsHoldOnlyThatPodsLines pins the narrowing and the bound.
func TestTC169PodScopedLogsHoldOnlyThatPodsLines(t *testing.T) {
	cluster := harness.Start(t)
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: harness.RuntimeApplication("shop", map[string]any{
		"owner": "alice",
	}, "shop")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, harness.RuntimeDeployment("shop", 3))
	logs := harness.NewFixtureLogs()
	for _, pod := range []string{"shop-0", "shop-1", "shop-2"} {
		cluster.CreatePod(t, harness.PodFixture{Name: pod, Labels: map[string]string{"app": "shop"}, Ready: true})
		logs.Add(harness.Namespace, pod, "app", readTime.Add(-time.Minute), pod+" line")
	}
	for index := 1; index <= 4; index++ {
		logs.Add(harness.Namespace, "shop-1", "app", readTime.Add(-time.Duration(6-index)*time.Minute), "shop-1 line "+string(rune('0'+index)))
	}

	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Logs = logs
		config.Now = func() time.Time { return readTime }
	})
	logPath := "/api/v1/logs/" + harness.Namespace + "/shop?pod=shop-1"

	status, body := gateway.Call(t, http.MethodGet, logPath, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the Pod logs = %d %v", status, body)
	}
	lines := logLines(t, body)
	if len(lines) != 5 {
		t.Fatalf("lines = %d, want the five lines of the read Pod", len(lines))
	}
	for _, line := range lines {
		if harness.String(line, "pod") != "shop-1" {
			t.Fatalf("the read returned a line of another Pod: %v", line)
		}
		if harness.String(line, "container") != "app" {
			t.Fatalf("the read returned another container: %v", line)
		}
	}
	if body["truncated"] != false {
		t.Fatalf("truncated = %v, want false below the bound", body["truncated"])
	}
	if body["window"] != "15m0s" {
		t.Fatalf("window = %v, want the unchanged default window", body["window"])
	}

	// The line bound still applies to the narrowed read and names the cut.
	status, body = gateway.Call(t, http.MethodGet, logPath+"&limit=3", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("bounded read = %d %v", status, body)
	}
	lines = logLines(t, body)
	if len(lines) != 3 {
		t.Fatalf("bounded lines = %d, want 3", len(lines))
	}
	if body["truncated"] != true {
		t.Fatalf("truncated = %v, want true", body["truncated"])
	}
	wantCut := readTime.Add(-3 * time.Minute).Format(time.RFC3339Nano)
	if body["truncatedAt"] != wantCut {
		t.Fatalf("truncatedAt = %v, want %s", body["truncatedAt"], wantCut)
	}
	for _, line := range lines {
		if harness.String(line, "pod") != "shop-1" {
			t.Fatalf("the bounded read returned a line of another Pod: %v", line)
		}
	}
}

// logLines reads the lines of one log response.
func logLines(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["lines"].([]any)
	if !ok {
		t.Fatalf("response has no lines: %v", body)
	}
	lines := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		line, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("line is not an object: %v", entry)
		}
		lines = append(lines, line)
	}
	return lines
}
