// Package kosdtc176 verifies the eviction ordering of node maintenance: a node
// that is still schedulable keeps its pods, and the drain route refuses with a
// conflict that names the node without writing a drain record. Once the flow
// takes the same node out of scheduling, the same request is accepted, evicts
// the pod and leaves one drain record (KOSD-AC-78, KOSD-TC-176).
package kosdtc176

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/maintenance"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	node      = "node-a"
	pod       = "shop-abc-1"
	ownerName = "owner"
	ownerPass = "owner-password"
	drainPath = "/api/v1/node-maintenance/drain"
	startPath = "/api/v1/node-maintenance"
	nodeKind  = "Node"
)

// TestTC176DrainBeforeTheNodeIsTakenOut checks the order eviction depends on:
// a schedulable node is not drained, and the refusal leaves nothing behind.
func TestTC176DrainBeforeTheNodeIsTakenOut(t *testing.T) {
	cluster := harness.Start(t)
	createNode(t, cluster, node)
	createPod(t, cluster, pod, node)

	access := cluster.StartAccess(t, nil, func(config *controlplane.Config) {
		config.Maintenance = &maintenance.Service{
			Dynamic: cluster.Dynamic,
			Writer:  cluster.Writer(),
			Pods:    maintenance.Cluster{Kube: cluster.Kube},
			Auditor: config.Auditor,
		}
	})
	token := access.Owner(t, ownerName, ownerPass)

	// No cordon yet: the flow refuses the eviction and names the node.
	status, body := access.Call(t, token, http.MethodPost, drainPath, map[string]any{"node": node}, nil)
	if status != http.StatusConflict {
		t.Fatalf("drain of a schedulable node: status %d, body %v, want 409", status, body)
	}
	if code := failureCode(t, body); code != controlplane.CodeConflict {
		t.Fatalf("drain refusal code = %q, want %q", code, controlplane.CodeConflict)
	}
	if message := failureMessage(t, body); !strings.Contains(message, node) {
		t.Fatalf("the refusal does not name the node: %q", message)
	}
	if !schedulable(t, cluster, node) {
		t.Fatal("the refused drain took the node out of scheduling")
	}
	if !podExists(t, cluster, pod) {
		t.Fatal("the refused drain evicted the pod")
	}
	if records := drainRecords(t, access, node); len(records) != 0 {
		t.Fatalf("the refused drain left records on %q: %+v", node, records)
	}

	// The flow takes the node out; the same drain is now accepted.
	status, body = access.Call(t, token, http.MethodPost, startPath, map[string]any{
		"node": node, "downtime": "15m",
	}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("maintenance start of %q: status %d, body %v, want 202", node, status, body)
	}
	if schedulable(t, cluster, node) {
		t.Fatal("the accepted maintenance start did not take the node out of scheduling")
	}
	status, body = access.Call(t, token, http.MethodPost, drainPath, map[string]any{"node": node}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("drain after the removal: status %d, body %v, want 202", status, body)
	}
	evicted := drainedPods(t, body)
	if !contains(evicted, harness.Namespace+"/"+pod) {
		t.Fatalf("the accepted drain evicted %v, want [%s/%s]", evicted, harness.Namespace, pod)
	}
	if podExists(t, cluster, pod) {
		t.Fatal("the accepted drain left the pod on the node")
	}
	records := drainRecords(t, access, node)
	if len(records) != 1 {
		t.Fatalf("the accepted drain left %d records, want one: %+v", len(records), records)
	}
	if records[0].Result != controlplane.ResultAccepted || records[0].Actor != ownerName {
		t.Fatalf("drain record = %+v, want an accepted record by %q", records[0], ownerName)
	}
}

// createNode writes one schedulable node.
func createNode(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
	})
}

// createPod writes one pod the kernel scheduled to one node.
func createPod(t *testing.T, cluster *harness.Cluster, name, node string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "pods", harness.Namespace, map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec": map[string]any{
			"nodeName":                      node,
			"terminationGracePeriodSeconds": int64(0),
			"containers":                    []any{map[string]any{"name": "app", "image": "nginx:1.27"}},
		},
	})
}

// schedulable reports whether the kernel still schedules on one node.
func schedulable(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	node, err := cluster.Direct(t, "", "v1", "Node", "").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read node %q: %v", name, err)
	}
	cordoned, _, _ := unstructured.NestedBool(node.Object, "spec", "unschedulable")
	return !cordoned
}

// podExists reports whether the cluster still holds one pod. A read failure
// other than absence fails the case, so a broken cluster cannot pass for an
// eviction.
func podExists(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := cluster.Direct(t, "", "v1", "Pod", harness.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read pod %q: %v", name, err)
	}
	return true
}

// drainRecords reads the drain records the audit trail keeps for one node (C-14).
func drainRecords(t *testing.T, access *harness.Access, node string) []identity.AuditEntry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ref := controlplane.ObjectRef{Group: api.Group, Kind: nodeKind, Name: node}
	entries, err := access.Identity.Store.RecentAuditEntriesForObject(ctx, ref.String(),
		[]string{controlplane.VerbDrain}, nil, 10)
	if err != nil {
		t.Fatalf("read the drain records of %q: %v", node, err)
	}
	return entries
}

// drainedPods reads the pods one drain response names as evicted.
func drainedPods(t *testing.T, body map[string]any) []string {
	t.Helper()
	drain, _ := body["drain"].(map[string]any)
	evicted, _ := drain["evicted"].([]any)
	names := make([]string, 0, len(evicted))
	for _, item := range evicted {
		name, _ := item.(string)
		names = append(names, name)
	}
	return names
}

// failureCode reads the stable code of one error response.
func failureCode(t *testing.T, body map[string]any) string {
	t.Helper()
	failure, _ := body["error"].(map[string]any)
	code, _ := failure["code"].(string)
	return code
}

// failureMessage reads the message of one error response.
func failureMessage(t *testing.T, body map[string]any) string {
	t.Helper()
	failure, _ := body["error"].(map[string]any)
	message, _ := failure["message"].(string)
	return message
}

// contains reports whether values names want.
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
