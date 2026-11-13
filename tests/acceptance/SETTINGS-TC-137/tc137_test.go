// Package settingstc137 verifies the maintenance window boundary a maintenance
// start runs against: an unwritten policy limits nothing, a period that
// excludes the judged instant refuses the start and names the next opening
// without touching the node's unschedulable field or the audit trail, and a
// period that includes it accepts the start (SETTINGS-AC-65, SETTINGS-TC-137).
package settingstc137

import (
	"context"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/maintenance"
	"github.com/tsic404/kosd/pkg/settings"
	"github.com/tsic404/kosd/pkg/settings/kossettings"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	nodeName = "node-a"
	downtime = "15m"
	owner    = "owner"
	password = "owner-password"

	startPath   = "/api/v1/node-maintenance"
	restorePath = "/api/v1/node-maintenance/" + nodeName
	windowPath  = "/api/v1/settings/" + kossettings.ClusterMaintenanceWindow
)

// TestTC137MaintenanceStartWindowBoundary drives one maintenance start before
// the policy is written, outside its window and inside it, judging the node's
// unschedulable field and the audit trail around every refusal.
func TestTC137MaintenanceStartWindowBoundary(t *testing.T) {
	cluster := harness.Start(t)
	createNode(t, cluster)

	// The access surface carries the account middleware in front of the
	// settings write and the maintenance flow, over one control-plane database.
	surface := harness.OpenIdentity(t, nil, nil)
	registry := &harness.RegistrySource{}
	installation := cluster.InstallPackage(t, "settings", kossettings.Declaration(), extension.MemoryArtifacts{})
	registry.Publish(installation.Registry())
	settingsService := settings.New(registry, surface.Store, settings.Config{Auditor: surface.Trail, Now: surface.Clock.Now})
	auditor := &harness.AuditRecorder{}
	maintenanceService := &maintenance.Service{
		Dynamic: cluster.Dynamic,
		Writer:  cluster.Writer(),
		Auditor: auditor,
		Now:     surface.Clock.Now,
		Window:  settingsService,
	}
	server, err := controlplane.New(controlplane.Config{
		Dynamic:     cluster.Dynamic,
		Authorizer:  identity.Authorizer{},
		Auditor:     surface.Trail,
		Maintenance: maintenanceService,
		Now:         surface.Clock.Now,
	})
	if err != nil {
		t.Fatalf("build the control plane: %v", err)
	}
	t.Cleanup(server.Close)
	settingsHandler := settings.NewAPI(settingsService).Handler()
	mux := http.NewServeMux()
	mux.Handle("/api/v1/settings", settingsHandler)
	mux.Handle("/api/v1/settings/", settingsHandler)
	mux.Handle("/", server.Handler())
	surface.Serve(t, mux)

	oneTime := surface.BootstrapOwner(t, owner)
	token, _ := surface.SignIn(t, owner, oneTime)
	surface.ChangePassword(t, token, oneTime, password)
	token, _ = surface.SignIn(t, owner, password)
	call := func(method, path string, body any) (int, map[string]any) {
		return surface.HTTP.Call(t, method, path, body, harness.Bearer(token))
	}

	// Without a written policy the start is not limited by it (C-72).
	status, body := call(http.MethodPost, startPath, map[string]any{"node": nodeName, "downtime": downtime})
	if status != http.StatusAccepted {
		t.Fatalf("start without a written window: status %d, body %v, want 202", status, body)
	}
	if !unschedulable(t, cluster) {
		t.Fatal("the accepted start did not take the node out of scheduling")
	}
	if verbs := auditor.Verbs(); len(verbs) != 1 || verbs[0] != controlplane.VerbMaintain {
		t.Fatalf("audit verbs after the accepted start = %v, want [%s]", verbs, controlplane.VerbMaintain)
	}
	status, body = call(http.MethodDelete, restorePath, nil)
	if status != http.StatusAccepted {
		t.Fatalf("restore: status %d, body %v, want 202", status, body)
	}
	if unschedulable(t, cluster) {
		t.Fatal("the restore left the node out of scheduling")
	}
	recordsBefore := len(auditor.Records())
	if recordsBefore != 2 {
		t.Fatalf("audit records after start and restore = %d, want 2", recordsBefore)
	}

	// The judged instant is Sunday 12:00 UTC; this period is Monday morning, so
	// the start is outside the window and must name the next opening.
	writeWindow(t, call, "Mon 02:00-04:00")
	status, body = call(http.MethodPost, startPath, map[string]any{"node": nodeName, "downtime": downtime})
	if status != http.StatusConflict {
		t.Fatalf("start outside the window: status %d, body %v, want 409", status, body)
	}
	if code := failureCode(t, body); code != controlplane.CodeConflict {
		t.Fatalf("refusal code = %q, want %q", code, controlplane.CodeConflict)
	}
	if message := failureMessage(t, body); !strings.Contains(message, "2026-03-02T02:00:00Z") {
		t.Fatalf("refusal reason = %q, want the next opening 2026-03-02T02:00:00Z", message)
	}
	if retry, _ := failure(body)["retryAfterSeconds"].(float64); retry <= 0 {
		t.Fatalf("refusal retryAfterSeconds = %v, want the wait until the next opening", failure(body)["retryAfterSeconds"])
	}
	if unschedulable(t, cluster) {
		t.Fatal("the refused start changed the node's unschedulable field")
	}
	if records := len(auditor.Records()); records != recordsBefore {
		t.Fatalf("audit records after the refusal = %d, want the %d before it", records, recordsBefore)
	}

	// A period that includes the judged instant accepts the start, so the
	// policy adds no refusal of its own.
	writeWindow(t, call, "Sun 11:00-13:00")
	status, body = call(http.MethodPost, startPath, map[string]any{"node": nodeName, "downtime": downtime})
	if status != http.StatusAccepted {
		t.Fatalf("start inside the window: status %d, body %v, want 202", status, body)
	}
	if !unschedulable(t, cluster) {
		t.Fatal("the accepted start did not take the node out of scheduling")
	}
	verbs := auditor.Verbs()
	if len(verbs) != recordsBefore+1 || verbs[len(verbs)-1] != controlplane.VerbMaintain {
		t.Fatalf("audit verbs after the accepted start = %v, want one more %s", verbs, controlplane.VerbMaintain)
	}
}

// writeWindow stores one weekly period through the settings write the declared
// item names (C-72).
func writeWindow(t *testing.T, call func(string, string, any) (int, map[string]any), value string) {
	t.Helper()
	status, body := call(http.MethodPut, windowPath, map[string]any{"value": value})
	if status != http.StatusOK {
		t.Fatalf("write the maintenance window %q: status %d, body %v", value, status, body)
	}
}

// unschedulable reports the node's unschedulable field as the kernel stores it.
func unschedulable(t *testing.T, cluster *harness.Cluster) bool {
	t.Helper()
	node, err := cluster.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}).
		Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read node %q: %v", nodeName, err)
	}
	cordoned, _, err := unstructured.NestedBool(node.Object, "spec", "unschedulable")
	if err != nil {
		t.Fatalf("read the unschedulable field of %q: %v", nodeName, err)
	}
	return cordoned
}

// createNode writes the one node every step of the case acts on.
func createNode(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": nodeName},
		"spec":       map[string]any{"unschedulable": false},
	})
}

// failure reads the error envelope of one refusal.
func failure(body map[string]any) map[string]any {
	envelope, _ := body["error"].(map[string]any)
	return envelope
}

// failureCode reads the stable code of one error response.
func failureCode(t *testing.T, body map[string]any) string {
	t.Helper()
	code, _ := failure(body)["code"].(string)
	return code
}

// failureMessage reads the user-facing message of one error response.
func failureMessage(t *testing.T, body map[string]any) string {
	t.Helper()
	message, _ := failure(body)["message"].(string)
	return message
}
