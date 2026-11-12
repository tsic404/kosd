// Package kosdtc166 verifies the failure semantics of the runtime reads
// (KOSD-AC-75, KOSD-TC-166): a control plane cut off from the runtime kernel
// answers the Pod projection and the Pod-narrowed log read with a reason
// instead of an empty list or empty log, and both recover once the runtime is
// readable again.
package kosdtc166

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// readTime is the clock the case reads on, so the fixture log line sits inside
// the window.
var readTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// sourceConnected and the two failure modes the case switches between: the
// whole runtime unreachable, and only the Pod list failing while every object
// read still answers.
const (
	sourceConnected int32 = iota
	sourceUnreachable
	sourcePodsUnreadable
)

// switchSource is the runtime read seam with a switch the case throws: the
// control plane keeps its client while the runtime is made unreachable.
type switchSource struct {
	base controlplane.ObjectSource
	mode *atomic.Int32
}

// List implements controlplane.ObjectSource.
func (s switchSource) List(ctx context.Context, objectType controlplane.ObjectType, namespace string) ([]controlplane.ObservedObject, error) {
	if s.mode.Load() == sourcePodsUnreadable && objectType.Kind == "Pod" {
		return nil, &clusterUnreachableError{}
	}
	if err := s.failure(); err != nil {
		return nil, err
	}
	return s.base.List(ctx, objectType, namespace)
}

// Get implements controlplane.ObjectSource.
func (s switchSource) Get(ctx context.Context, objectType controlplane.ObjectType, namespace, name string) (controlplane.ObservedObject, error) {
	if err := s.failure(); err != nil {
		return controlplane.ObservedObject{}, err
	}
	return s.base.Get(ctx, objectType, namespace, name)
}

// failure is the reason the read fails in the current mode.
func (s switchSource) failure() error {
	if s.mode.Load() == sourceUnreachable {
		return &clusterUnreachableError{}
	}
	return nil
}

// clusterUnreachableError is the failure a disconnected client reports.
type clusterUnreachableError struct{}

// Error implements error.
func (*clusterUnreachableError) Error() string {
	return "dial tcp 10.43.0.1:443: connect: connection refused"
}

// TestTC166UnreachableRuntimeIsAReasonNotAnEmptyAnswer pins the failure and the
// recovery of both runtime reads.
func TestTC166UnreachableRuntimeIsAReasonNotAnEmptyAnswer(t *testing.T) {
	cluster := harness.Start(t)
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: harness.RuntimeApplication("shop", map[string]any{
		"owner": "alice",
	}, "shop")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, harness.RuntimeDeployment("shop", 1))
	cluster.CreatePod(t, harness.PodFixture{Name: "shop-0", Labels: map[string]string{"app": "shop"}, Ready: true})

	logs := harness.NewFixtureLogs()
	logs.Add(harness.Namespace, "shop-0", "app", readTime.Add(-time.Minute), "shop line")

	mode := &atomic.Int32{}
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Source = switchSource{base: controlplane.NewClusterSource(cluster.Dynamic), mode: mode}
		config.Logs = logs
		config.Now = func() time.Time { return readTime }
	})
	projectionPath := "/api/v1/pods/" + harness.Namespace + "/shop"
	logPath := "/api/v1/logs/" + harness.Namespace + "/shop?pod=shop-0"

	reads := []struct {
		name string
		path string
		key  string
	}{
		{name: "pod projection", path: projectionPath, key: "items"},
		{name: "pod logs", path: logPath, key: "lines"},
	}

	// An unreachable runtime is a failure with a reason, never an empty answer.
	mode.Store(sourceUnreachable)
	for _, read := range reads {
		t.Run("unreachable "+read.name, func(t *testing.T) {
			assertUnreadable(t, gateway, read.path, read.key)
		})
	}

	// The Pod list alone can fail while every object read still answers: the
	// projection and the Pod-narrowed log read must name that failure instead
	// of answering an empty result (C-76's "never an empty projection").
	mode.Store(sourcePodsUnreadable)
	for _, read := range reads {
		t.Run("pod list unreadable "+read.name, func(t *testing.T) {
			assertUnreadable(t, gateway, read.path, read.key)
		})
	}

	// Once the runtime answers, both reads return their content again.
	mode.Store(sourceConnected)
	status, body := gateway.Call(t, http.MethodGet, projectionPath, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("recovered projection = %d %v, want 200", status, body)
	}
	items := harness.Items(t, body)
	if len(items) != 1 || harness.String(items[0], "name") != "shop-0" {
		t.Fatalf("recovered projection = %v, want the application's Pod", items)
	}
	status, body = gateway.Call(t, http.MethodGet, logPath, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("recovered logs = %d %v, want 200", status, body)
	}
	lines, _ := body["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("recovered logs = %v, want the fixture line", body["lines"])
	}
}

// assertUnreadable checks one runtime read fails with the cluster's own reason
// and no result field.
func assertUnreadable(t *testing.T, gateway *harness.ControlPlane, path, resultKey string) {
	t.Helper()
	status, body := gateway.Call(t, http.MethodGet, path, nil, nil)
	if status != http.StatusBadGateway {
		t.Fatalf("read = %d %v, want 502", status, body)
	}
	errorBody, _ := body["error"].(map[string]any)
	if errorBody["code"] != controlplane.CodeClusterUnavailable {
		t.Fatalf("code = %v, want %s", errorBody["code"], controlplane.CodeClusterUnavailable)
	}
	if errorBody["retryable"] != true {
		t.Fatalf("retryable = %v, want true", errorBody["retryable"])
	}
	if message, _ := errorBody["message"].(string); message == "" {
		t.Fatalf("the failure carries no reason: %v", errorBody)
	}
	if _, answered := body[resultKey]; answered {
		t.Fatalf("an unreadable read answered with a result: %v", body)
	}
}
