// Package kosdtc179 verifies the upgrade preflight record: the Owner's
// preflight write stores the request with its result, the read path reports it
// with an observation time, a repeated request keeps one record, a later
// preflight marks the earlier record stale without deleting it, the lifecycle
// CLI preflight writes no record, and the upgrade write references a stored
// record while Member and Guest writes are refused (KOSD-AC-79, KOSD-TC-179).
package kosdtc179

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/maintenance"
	"github.com/tsic404/kosd/pkg/upgrade"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	appsNamespace   = "apps"
	nodeName        = "node-a"
	volumeName      = "data"
	applicationName = "shop"
	targetVersion   = "v1.38.0"
	nextVersion     = "v1.39.0"
	downtime        = "15m"
	ownerName       = "owner"
	ownerPassword   = "owner-password"
	memberName      = "member"
	memberPassword  = "member-password"
	guestName       = "guest"
	guestPassword   = "guest-password"
	preflightPath   = "/api/v1/upgrade-preflights"
	upgradePath     = "/api/v1/upgrade-requests"
	nodeKubelet     = "v1.36.0"
)

// TestTC179PreflightRecordAndWriterRole drives the case the frozen entry
// describes: one Owner preflight, the same request again, a second preflight,
// the lifecycle CLI preflight, the upgrade write, and the two refused roles.
func TestTC179PreflightRecordAndWriterRole(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, appsNamespace)
	createNode(t, cluster, nodeName, nodeKubelet)
	volume := harness.Volume(appsNamespace, volumeName, map[string]any{
		"owner":        ownerName,
		"mobility":     "node-local",
		"capacity":     "1Gi",
		"storageClass": "local",
	})
	volume.Object["status"] = map[string]any{"node": nodeName}
	cluster.CreateIn(t, appsNamespace, volume)
	application := harness.KernelApplication(appsNamespace, applicationName, applicationName, ownerName, "shop")
	application.Object["spec"].(map[string]any)["volumes"] = []any{volumeName}
	cluster.CreateIn(t, appsNamespace, application)

	surface := harness.OpenIdentity(t, nil, nil)
	flow := &upgrade.Flow{
		Maintenance: &maintenance.Service{
			Dynamic: cluster.Dynamic,
			Writer:  cluster.Writer(),
			Pods:    maintenance.Cluster{Kube: cluster.Kube},
			Auditor: surface.Trail,
		},
		Applications: upgrade.ClusterApplications{Dynamic: cluster.Dynamic},
		Nodes:        upgrade.ClusterNodes{Dynamic: cluster.Dynamic},
	}
	records := upgrade.New(upgrade.Config{
		Store:    surface.Store,
		Platform: flow,
		Auditor:  surface.Trail,
		Now:      surface.Clock.Now,
	})
	api := upgrade.NewAPI(records)
	mux := http.NewServeMux()
	mux.Handle(preflightPath, api.Handler())
	mux.Handle(preflightPath+"/", api.Handler())
	mux.Handle(upgradePath, api.Handler())
	surface.Serve(t, mux)

	ownerToken := signInOwner(t, surface)
	memberToken := createAccount(t, surface, ownerToken, memberName, memberPassword, write.RoleMember)
	guestToken := createAccount(t, surface, ownerToken, guestName, guestPassword, write.RoleGuest)

	// One Owner preflight stores the request with the result the flow computed,
	// and the record reads back with the same content and its observation time.
	request := map[string]any{
		"kind":          "platform",
		"targetVersion": targetVersion,
		"downtime":      downtime,
		"nodes":         []string{nodeName},
	}
	status, body := surface.HTTP.Call(t, http.MethodPost, preflightPath, request, bearer(ownerToken))
	if status != http.StatusCreated {
		t.Fatalf("the owner's preflight write = %d %v, want 201", status, body)
	}
	first := recordOf(t, body)
	firstID := harness.String(first, "id")
	if firstID == "" {
		t.Fatalf("the preflight answer carries no record identity: %v", first)
	}
	result := resultOf(t, first)
	if harness.String(result, "targetVersion") != targetVersion {
		t.Fatalf("record targetVersion = %q, want %q", harness.String(result, "targetVersion"), targetVersion)
	}
	if harness.String(result, "declaredDowntime") != downtime || harness.String(result, "estimatedDowntime") != downtime {
		t.Fatalf("record downtime = %v/%v, want the declared %q",
			result["declaredDowntime"], result["estimatedDowntime"], downtime)
	}
	if got := stringsOf(t, result["affectedApplications"]); !reflect.DeepEqual(got, []string{appsNamespace + "/" + applicationName}) {
		t.Fatalf("affected applications = %v, want [%s/%s]", got, appsNamespace, applicationName)
	}
	if harness.String(first, "observedAt") == "" {
		t.Fatalf("the record carries no observation time: %v", first)
	}
	if first["stale"] != false || first["isSuperseded"] != false {
		t.Fatalf("a fresh newest record reports stale=%v superseded=%v", first["stale"], first["isSuperseded"])
	}

	status, body = surface.HTTP.Call(t, http.MethodGet, preflightPath+"/"+firstID, nil, bearer(ownerToken))
	if status != http.StatusOK {
		t.Fatalf("read the record = %d %v", status, body)
	}
	if !sameRecord(first, body) {
		t.Fatalf("the stored record differs from the preflight answer:\n preflight %v\n stored    %v", first, body)
	}

	// The same request submitted again keeps one record: no second record is
	// produced (C-12, C-88).
	status, body = surface.HTTP.Call(t, http.MethodPost, preflightPath, request, bearer(ownerToken))
	if status != http.StatusCreated {
		t.Fatalf("the repeated preflight write = %d %v", status, body)
	}
	if repeated := harness.String(recordOf(t, body), "id"); repeated != firstID {
		t.Fatalf("a repeated request produced record %q, want the stored %q", repeated, firstID)
	}
	if items := itemsOf(t, surface, ownerToken); len(items) != 1 {
		t.Fatalf("the control plane holds %d records, want one", len(items))
	}

	// A later preflight becomes the current record and marks the first one stale
	// without deleting it (C-88).
	status, body = surface.HTTP.Call(t, http.MethodPost, preflightPath, map[string]any{
		"kind": "platform", "targetVersion": nextVersion, "downtime": downtime, "nodes": []string{nodeName},
	}, bearer(ownerToken))
	if status != http.StatusCreated {
		t.Fatalf("the second preflight write = %d %v", status, body)
	}
	second := recordOf(t, body)
	secondID := harness.String(second, "id")
	if secondID == "" || secondID == firstID {
		t.Fatalf("the second preflight produced %q, want a new record", secondID)
	}
	items := itemsOf(t, surface, ownerToken)
	if len(items) != 2 {
		t.Fatalf("the control plane holds %d records, want two", len(items))
	}
	if newest := harness.String(items[0], "id"); newest != secondID {
		t.Fatalf("newest record = %q, want the second preflight %q", newest, secondID)
	}
	status, body = surface.HTTP.Call(t, http.MethodGet, preflightPath+"/"+firstID, nil, bearer(ownerToken))
	if status != http.StatusOK {
		t.Fatalf("reading the superseded record = %d %v, want it readable", status, body)
	}
	superseded := body
	if superseded["isSuperseded"] != true {
		t.Fatalf("the earlier record is not marked stale after the later preflight: %v", superseded)
	}
	if !sameRecord(first, superseded) {
		t.Fatalf("the superseded record changed:\n before %v\n after  %v", first, superseded)
	}

	// The lifecycle CLI preflight prints its result and writes no record
	// (ADM-C-69): the records, their count and their content stay as they were.
	before := snapshot(t, surface, ownerToken)
	report, failure := flow.Preflight(context.Background(), upgrade.Request{
		Nodes: []string{nodeName}, Version: nextVersion, Downtime: downtime,
	})
	if failure != nil {
		t.Fatalf("the lifecycle CLI preflight: %v", failure)
	}
	if len(report.Nodes) != 1 || !reflect.DeepEqual(report.Nodes[0].Applications, []string{appsNamespace + "/" + applicationName}) {
		t.Fatalf("the CLI preflight reported %+v, want the node-local application", report.Nodes)
	}
	if after := snapshot(t, surface, ownerToken); !reflect.DeepEqual(before, after) {
		t.Fatalf("the CLI preflight changed the control-plane records:\n before %v\n after  %v", before, after)
	}

	// The upgrade write references the newest record, which stays readable and
	// is the source of that upgrade (C-88).
	status, body = surface.HTTP.Call(t, http.MethodPost, upgradePath, map[string]any{
		"recordId": secondID,
		"nodes": []map[string]any{
			{"node": nodeName, "version": nextVersion, "result": "restored"},
			{"node": "node-b", "version": nextVersion, "result": "restored"},
		},
	}, bearer(ownerToken))
	if status != http.StatusAccepted {
		t.Fatalf("the upgrade write = %d %v, want 202", status, body)
	}
	upgraded := recordOf(t, body)
	run, _ := upgraded["upgrade"].(map[string]any)
	if run == nil || harness.String(run, "recordId") != secondID {
		t.Fatalf("the upgrade write does not reference the record: %v", upgraded["upgrade"])
	}
	results, _ := run["nodes"].([]any)
	if len(results) != 2 {
		t.Fatalf("the upgrade write recorded %d node results, want both nodes in one record: %v", len(results), run["nodes"])
	}
	status, body = surface.HTTP.Call(t, http.MethodGet, preflightPath+"/"+secondID, nil, bearer(ownerToken))
	if status != http.StatusOK {
		t.Fatalf("reading the upgraded record = %d %v", status, body)
	}
	readBack := body
	readRun, _ := readBack["upgrade"].(map[string]any)
	if readRun == nil || harness.String(readRun, "recordId") != secondID {
		t.Fatalf("the stored record does not carry its upgrade source: %v", readBack["upgrade"])
	}
	if items := itemsOf(t, surface, ownerToken); len(items) != 2 {
		t.Fatalf("the upgrade write produced %d records, want the two preflights", len(items))
	}

	// A record that does not exist refuses the upgrade write and states why
	// (C-88).
	status, body = surface.HTTP.Call(t, http.MethodPost, upgradePath, map[string]any{"recordId": "preflight-99"}, bearer(ownerToken))
	if status != http.StatusNotFound {
		t.Fatalf("an upgrade from a missing record = %d %v, want 404", status, body)
	}
	if code := failureCode(t, body); code != controlplane.CodeNotFound {
		t.Fatalf("missing-record refusal code = %q, want %q", code, controlplane.CodeNotFound)
	}
	if message := failureMessage(t, body); !strings.Contains(message, "preflight-99") {
		t.Fatalf("the refusal does not name the missing record: %q", message)
	}

	// Member and Guest submit both writes and are refused with 403; no record
	// appears and the stored records keep their content (KOSD-C-59).
	unchanged := snapshot(t, surface, ownerToken)
	for _, token := range []string{memberToken, guestToken} {
		status, body = surface.HTTP.Call(t, http.MethodPost, preflightPath, request, bearer(token))
		if status != http.StatusForbidden {
			t.Fatalf("a non-owner preflight write = %d %v, want 403", status, body)
		}
		if code := failureCode(t, body); code != controlplane.CodePermissionDenied {
			t.Fatalf("non-owner preflight refusal code = %q, want %q", code, controlplane.CodePermissionDenied)
		}
		status, body = surface.HTTP.Call(t, http.MethodPost, upgradePath, map[string]any{
			"recordId": secondID,
			"nodes":    []map[string]any{{"node": nodeName, "version": nextVersion, "result": "restored"}},
		}, bearer(token))
		if status != http.StatusForbidden {
			t.Fatalf("a non-owner upgrade write = %d %v, want 403", status, body)
		}
		if code := failureCode(t, body); code != controlplane.CodePermissionDenied {
			t.Fatalf("non-owner upgrade refusal code = %q, want %q", code, controlplane.CodePermissionDenied)
		}
	}
	if after := snapshot(t, surface, ownerToken); !reflect.DeepEqual(unchanged, after) {
		t.Fatalf("the refused writes changed the records:\n before %v\n after  %v", unchanged, after)
	}

	// C-13's one freshness bound applies to the record too: an observation older
	// than the threshold reads as stale.
	surface.Clock.Advance(controlplane.StaleThreshold + time.Minute)
	status, body = surface.HTTP.Call(t, http.MethodGet, preflightPath+"/"+secondID, nil, bearer(ownerToken))
	if status != http.StatusOK {
		t.Fatalf("reading the aged record = %d %v", status, body)
	}
	if aged := body; aged["stale"] != true {
		t.Fatalf("a record observed beyond the threshold reads stale=%v, want true", aged["stale"])
	}
}

// createNode writes one schedulable node that reports its kubelet version.
func createNode(t *testing.T, cluster *harness.Cluster, name, version string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
		"status":     map[string]any{"nodeInfo": map[string]any{"kubeletVersion": version}},
	})
}

// signInOwner bootstraps the first Owner, replaces its one-time password and
// returns a usable token.
func signInOwner(t *testing.T, surface *harness.Identity) string {
	t.Helper()
	oneTime := surface.BootstrapOwner(t, ownerName)
	token, _ := surface.SignIn(t, ownerName, oneTime)
	surface.ChangePassword(t, token, oneTime, ownerPassword)
	token, _ = surface.SignIn(t, ownerName, ownerPassword)
	return token
}

// createAccount creates one account with a role and returns a usable token.
func createAccount(t *testing.T, surface *harness.Identity, ownerToken, username, password string, role write.Role) string {
	t.Helper()
	status, body := surface.HTTP.Call(t, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": username, "role": string(role),
	}, bearer(ownerToken))
	if status != http.StatusCreated {
		t.Fatalf("create account %s: status %d, body %v", username, status, body)
	}
	oneTime, _ := body["oneTimePassword"].(string)
	if oneTime == "" {
		t.Fatalf("create account %s returned no one-time password: %v", username, body)
	}
	token, _ := surface.SignIn(t, username, oneTime)
	surface.ChangePassword(t, token, oneTime, password)
	token, _ = surface.SignIn(t, username, password)
	return token
}

// bearer is the header carrying one token.
func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// itemsOf reads every stored record, newest first.
func itemsOf(t *testing.T, surface *harness.Identity, token string) []map[string]any {
	t.Helper()
	status, body := surface.HTTP.Call(t, http.MethodGet, preflightPath, nil, bearer(token))
	if status != http.StatusOK {
		t.Fatalf("list the records = %d %v", status, body)
	}
	return itemsOfBody(t, body)
}

// itemsOfBody reads the record list of one answer.
func itemsOfBody(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("the answer carries no record list: %v", body)
	}
	items := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		record, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a record entry is not an object: %v", entry)
		}
		items = append(items, record)
	}
	return items
}

// snapshot reads the record list as the case compares it before and after an
// operation that must not write.
func snapshot(t *testing.T, surface *harness.Identity, token string) []any {
	t.Helper()
	status, body := surface.HTTP.Call(t, http.MethodGet, preflightPath, nil, bearer(token))
	if status != http.StatusOK {
		t.Fatalf("list the records = %d %v", status, body)
	}
	items, _ := body["items"].([]any)
	return items
}

// recordOf reads the record one write answer carries.
func recordOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	record, ok := body["record"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no record: %v", body)
	}
	return record
}

// resultOf reads one record's stored result.
func resultOf(t *testing.T, record map[string]any) map[string]any {
	t.Helper()
	result, ok := record["result"].(map[string]any)
	if !ok {
		t.Fatalf("the record carries no result: %v", record)
	}
	return result
}

// sameRecord compares the parts a read must return unchanged: the request, the
// result and the observation time.
func sameRecord(left, right map[string]any) bool {
	return reflect.DeepEqual(left["request"], right["request"]) &&
		reflect.DeepEqual(left["result"], right["result"]) &&
		left["observedAt"] == right["observedAt"]
}

// stringsOf reads one string list from a decoded body.
func stringsOf(t *testing.T, value any) []string {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("value is not a list: %v", value)
	}
	items := make([]string, 0, len(raw))
	for _, entry := range raw {
		text, ok := entry.(string)
		if !ok {
			t.Fatalf("list entry is not a string: %v", entry)
		}
		items = append(items, text)
	}
	return items
}

// failureCode reads the stable code of one error answer.
func failureCode(t *testing.T, body map[string]any) string {
	t.Helper()
	failure, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no error envelope: %v", body)
	}
	return harness.String(failure, "code")
}

// failureMessage reads the user-facing message of one error answer.
func failureMessage(t *testing.T, body map[string]any) string {
	t.Helper()
	failure, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no error envelope: %v", body)
	}
	return harness.String(failure, "message")
}
