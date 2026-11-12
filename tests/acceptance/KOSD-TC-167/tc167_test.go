// Package kosdtc167 verifies the repeat-delete and audit rules of the single
// Pod delete (KOSD-AC-75, KOSD-TC-167): two submissions for the same Pod
// remove one Pod and leave one replacement, both leave a delete record aimed at
// that Pod, and the delete records stay distinguishable from the application's
// stop and restart records.
package kosdtc167

import (
	"net/http"
	"reflect"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/app"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// TestTC167RepeatedDeleteHasOneEffectAndTwoRecords pins idempotency and the
// audit trail.
func TestTC167RepeatedDeleteHasOneEffectAndTwoRecords(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	stored := harness.RuntimeApplication("shop", map[string]any{
		"owner":    "alice",
		"replicas": int64(2),
	}, "shop")
	stored["status"].(map[string]any)["phase"] = app.PhaseReady
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: stored})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, harness.RuntimeDeployment("shop", 2))
	cluster.CreatePod(t, harness.PodFixture{Name: "shop-0", Labels: map[string]string{"app": "shop"}, Ready: true})
	cluster.CreatePod(t, harness.PodFixture{Name: "shop-1", Labels: map[string]string{"app": "shop"}, Ready: true})

	audit := &harness.AuditRecorder{}
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Auditor = audit
	})
	podPath := "/api/v1/pods/" + harness.Namespace + "/shop/shop-0"

	// The first submission removes the Pod; the second one repeats the request
	// after the runtime controller created the replacement instance.
	if status, body := gateway.Call(t, http.MethodDelete, podPath, nil, nil); status != http.StatusAccepted {
		t.Fatalf("first delete = %d %v, want 202", status, body)
	}
	cluster.CreatePod(t, harness.PodFixture{Name: "shop-2", Labels: map[string]string{"app": "shop"}, Ready: true})
	status, body := gateway.Call(t, http.MethodDelete, podPath, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("repeat delete = %d %v, want 202", status, body)
	}
	if body["idempotent"] != true {
		t.Fatalf("repeat delete idempotent = %v, want true", body["idempotent"])
	}
	status, body = gateway.Call(t, http.MethodGet, "/api/v1/pods/"+harness.Namespace+"/shop", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the projection = %d %v", status, body)
	}
	names := make([]string, 0)
	for _, item := range harness.Items(t, body) {
		names = append(names, harness.String(item, "name"))
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"shop-1", "shop-2"}) {
		t.Fatalf("projection names = %v, want exactly one replacement", names)
	}

	// The application's stop and restart actions leave their own records.
	action(t, gateway, "stop")
	action(t, gateway, "restart")

	deletes := recordsWithVerb(audit.Records(), string(controlplane.VerbDeletePod))
	if len(deletes) != 2 {
		t.Fatalf("delete records = %d, want one per submission", len(deletes))
	}
	for _, record := range deletes {
		if record.Object.Kind != "Pod" || record.Object.Name != "shop-0" {
			t.Fatalf("delete record names %v, want the Pod the request names", record.Object)
		}
		if record.Result != controlplane.ResultAccepted {
			t.Fatalf("delete record result = %q, want accepted", record.Result)
		}
		if record.Actor == "" {
			t.Fatal("the delete record carries no actor")
		}
	}
	stopRecords := recordsWithVerb(audit.Records(), "stop")
	if len(stopRecords) != 1 || stopRecords[0].Object.Kind != "Application" {
		t.Fatalf("stop records = %v, want one application record", stopRecords)
	}
	restartRecords := recordsWithVerb(audit.Records(), "restart")
	if len(restartRecords) != 1 || restartRecords[0].Object.Kind != "Application" {
		t.Fatalf("restart records = %v, want one application record", restartRecords)
	}
	if string(controlplane.VerbDeletePod) == "stop" || string(controlplane.VerbDeletePod) == "start" ||
		string(controlplane.VerbDeletePod) == "restart" || string(controlplane.VerbDeletePod) == "scale" {
		t.Fatalf("the pod delete shares a verb with an application lifecycle action: %q", controlplane.VerbDeletePod)
	}
}

// action submits one application action.
func action(t *testing.T, gateway *harness.ControlPlane, verb string) {
	t.Helper()
	status, body := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind": "Application", "namespace": harness.Namespace, "name": "shop", "action": verb,
	}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("action %s = %d %v, want 202", verb, status, body)
	}
}

// recordsWithVerb selects the audit records carrying one verb.
func recordsWithVerb(records []controlplane.AuditRecord, verb string) []controlplane.AuditRecord {
	matched := make([]controlplane.AuditRecord, 0)
	for _, record := range records {
		if record.Verb == verb {
			matched = append(matched, record)
		}
	}
	return matched
}
