// Package kosdtc177 verifies the writer boundary of node maintenance: a KOS
// subject outside the maintenance flow cannot write a node's unschedulable
// field in either direction, and a client outside the flow cannot drive any
// maintenance entry. The extension writer's refusal names the subject and the
// node; every refusal leaves the field and the pods in place and writes no
// maintenance or drain record. The Owner through the flow still takes a node
// out, and that write is recorded (KOSD-AC-78, KOSD-TC-177).
package kosdtc177

import (
	"context"
	"errors"
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
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	schedulableNode = "node-a"
	cordonedNode    = "node-b"
	schedulablePod  = "shop-abc-1"
	cordonedPod     = "cache-abc-1"
	ownerName       = "owner"
	ownerPass       = "owner-password"
	memberName      = "member"
	memberPass      = "member-password"
	startPath       = "/api/v1/node-maintenance"
	drainPath       = "/api/v1/node-maintenance/drain"
	restorePrefix   = "/api/v1/node-maintenance/"
)

// TestTC177WritesOutsideTheFlow checks the writer boundary of C-82: the node
// field and the eviction belong to the maintenance flow, and every other
// subject is refused with the object named and no side effect.
func TestTC177WritesOutsideTheFlow(t *testing.T) {
	cluster := harness.Start(t)
	createNode(t, cluster, schedulableNode)
	createNode(t, cluster, cordonedNode)
	createPod(t, cluster, schedulablePod, schedulableNode)
	createPod(t, cluster, cordonedPod, cordonedNode)
	// node-b is out of scheduling before the case starts, the way an outside
	// cordon leaves it (C-65).
	cordonExternally(t, cluster, cordonedNode)

	// A KOS subject outside the flow — an extension controller — is refused the
	// node field, and the refusal names the subject and the node (C-04, C-82).
	writer := cluster.Writer()
	_, err := writer.Apply(context.Background(), write.SubjectExtensionReconciler, write.Session{}, write.PartSpec,
		nodeDocument(schedulableNode, true))
	var denied *write.Denied
	if !errors.As(err, &denied) {
		t.Fatalf("the extension controller wrote the node field: %v", err)
	}
	if denied.Subject != write.SubjectExtensionReconciler || denied.Object.Kind != "Node" ||
		!strings.Contains(denied.Error(), schedulableNode) {
		t.Fatalf("the refusal does not name the subject and the node: %+v", denied)
	}
	if denied.Code() != http.StatusForbidden {
		t.Fatalf("refusal code = %d, want 403", denied.Code())
	}
	if !schedulable(t, cluster, schedulableNode) {
		t.Fatal("the refused write took the schedulable node out of scheduling")
	}
	if schedulable(t, cluster, cordonedNode) {
		t.Fatal("the refused write lifted the cordon of node-b")
	}

	// Clearing the field is a write to it too: an existing cordon survives the
	// same subject asking for the schedulable direction (C-82).
	_, err = writer.Apply(context.Background(), write.SubjectExtensionReconciler, write.Session{}, write.PartSpec,
		nodeDocument(cordonedNode, false))
	if !errors.As(err, &denied) {
		t.Fatalf("the extension controller cleared the node field: %v", err)
	}
	if denied.Subject != write.SubjectExtensionReconciler || denied.Object.Kind != "Node" ||
		!strings.Contains(denied.Error(), cordonedNode) {
		t.Fatalf("the clearing refusal does not name the subject and the node: %+v", denied)
	}
	if denied.Code() != http.StatusForbidden {
		t.Fatalf("clearing refusal code = %d, want 403", denied.Code())
	}
	if schedulable(t, cluster, cordonedNode) {
		t.Fatal("the refused clearing lifted the cordon of node-b")
	}

	access := cluster.StartAccess(t, nil, func(config *controlplane.Config) {
		config.Maintenance = &maintenance.Service{
			Dynamic: cluster.Dynamic,
			Writer:  cluster.Writer(),
			Pods:    maintenance.Cluster{Kube: cluster.Kube},
			Auditor: config.Auditor,
		}
	})
	ownerToken := access.Owner(t, ownerName, ownerPass)
	memberToken := access.Account(t, ownerToken, memberName, memberPass, write.RoleMember)

	// An ordinary client is refused both maintenance entries: the cordon write
	// and the eviction are the flow's own, not a client's (C-59, C-82).
	status, body := access.Call(t, memberToken, http.MethodPost, startPath, map[string]any{
		"node": schedulableNode, "downtime": "15m",
	}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("a member's maintenance start: status %d, body %v, want 403", status, body)
	}
	if code := failureCode(t, body); code != controlplane.CodePermissionDenied {
		t.Fatalf("start refusal code = %q, want %q", code, controlplane.CodePermissionDenied)
	}
	status, body = access.Call(t, memberToken, http.MethodPost, drainPath, map[string]any{"node": cordonedNode}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("a member's drain: status %d, body %v, want 403", status, body)
	}
	if code := failureCode(t, body); code != controlplane.CodePermissionDenied {
		t.Fatalf("drain refusal code = %q, want %q", code, controlplane.CodePermissionDenied)
	}
	status, body = access.Call(t, memberToken, http.MethodDelete, restorePrefix+cordonedNode, nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("a member's maintenance restore: status %d, body %v, want 403", status, body)
	}
	if code := failureCode(t, body); code != controlplane.CodePermissionDenied {
		t.Fatalf("restore refusal code = %q, want %q", code, controlplane.CodePermissionDenied)
	}

	// Neither node moved and both pods stayed; nothing was recorded as a
	// maintenance write or an eviction (C-14, C-82).
	if !schedulable(t, cluster, schedulableNode) {
		t.Fatal("the refused client write took the schedulable node out of scheduling")
	}
	if schedulable(t, cluster, cordonedNode) {
		t.Fatal("the refused client write lifted the cordon of node-b")
	}
	for _, name := range []string{schedulablePod, cordonedPod} {
		if !podExists(t, cluster, name) {
			t.Fatalf("the refused client requests evicted pod %q", name)
		}
	}
	for _, node := range []string{schedulableNode, cordonedNode} {
		if records := maintenanceRecords(t, access, node); len(records) != 0 {
			t.Fatalf("the refused requests left records on %q: %+v", node, records)
		}
	}

	// The Owner through the flow still writes the field, and that write leaves
	// one maintenance record naming the acting identity.
	status, body = access.Call(t, ownerToken, http.MethodPost, startPath, map[string]any{
		"node": schedulableNode, "downtime": "15m",
	}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("the owner's maintenance start: status %d, body %v, want 202", status, body)
	}
	if schedulable(t, cluster, schedulableNode) {
		t.Fatal("the accepted maintenance start did not take the node out of scheduling")
	}
	records := maintenanceRecords(t, access, schedulableNode)
	if len(records) != 1 {
		t.Fatalf("the accepted start left %d records, want one: %+v", len(records), records)
	}
	if records[0].Result != controlplane.ResultAccepted || records[0].Actor != ownerName {
		t.Fatalf("maintenance record = %+v, want an accepted record by %q", records[0], ownerName)
	}
}

// nodeDocument is the node spec an outside writer sends for the field.
func nodeDocument(name string, cordoned bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"unschedulable": cordoned},
	}}
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

// cordonExternally takes a node out of scheduling the way the cluster interface
// does: no KOS flow, no KOS record (C-65).
func cordonExternally(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes := cluster.Direct(t, "", "v1", "Node", "")
	node, err := nodes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read node %q: %v", name, err)
	}
	if err := unstructured.SetNestedField(node.Object, true, "spec", "unschedulable"); err != nil {
		t.Fatalf("set the unschedulable field of %q: %v", name, err)
	}
	if _, err := nodes.Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("cordon node %q: %v", name, err)
	}
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
// other than absence fails the case, so a broken cluster cannot pass for a
// surviving pod.
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

// maintenanceRecords reads the maintenance and drain records the audit trail
// keeps for one node (C-14, C-82).
func maintenanceRecords(t *testing.T, access *harness.Access, node string) []identity.AuditEntry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ref := controlplane.ObjectRef{Group: api.Group, Kind: "Node", Name: node}
	entries, err := access.Identity.Store.RecentAuditEntriesForObject(ctx, ref.String(),
		[]string{controlplane.VerbMaintain, controlplane.VerbRestore, controlplane.VerbDrain}, nil, 10)
	if err != nil {
		t.Fatalf("read the maintenance records of %q: %v", node, err)
	}
	return entries
}

// failureCode reads the stable code of one error response.
func failureCode(t *testing.T, body map[string]any) string {
	t.Helper()
	failure, _ := body["error"].(map[string]any)
	code, _ := failure["code"].(string)
	return code
}
