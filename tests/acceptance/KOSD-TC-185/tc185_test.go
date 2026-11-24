// Package kosdtc185 verifies the terminal audit read of KOSD-AC-83,
// KOSD-TC-185: the operator reads its own session, the global Owner reads
// every session, another account and an absent session answer alike, the
// metadata carries input sequence and receipt, node container is empty, the
// trail survives ending, uninstall and permission changes, and a database
// failure is a 503 rather than an empty page.
package kosdtc185

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

func TestTC185AuditReadAuthorizationAndRetention(t *testing.T) {
	cluster := harness.Start(t)
	term := cluster.StartTerminal(t, harness.TerminalOptions{
		Extensions: []string{harness.TerminalExtension},
	})
	owner := term.OwnerToken(t)
	alice := term.AccountToken(t, "alice", "Member")
	bob := term.AccountToken(t, "bob", "Member")

	alicePod := term.PodTarget(t, "alice-shop", "alice")
	bobPod := term.PodTarget(t, "bob-shop", "bob")
	aliceSession, aliceToken := startSession(t, term, alice, alicePod)
	for sequence := uint64(1); sequence <= 3; sequence++ {
		if status, body := term.Input(t, aliceToken, aliceSession, sequence, "echo alice\n"); status != http.StatusOK {
			t.Fatalf("alice input %d = %d %v, want 200", sequence, status, body)
		}
	}
	bobSession, _ := startSession(t, term, bob, bobPod)
	node := term.NodeTarget(t, "node-a")
	nodeSession, nodeToken := createAttached(t, term, owner, node)
	if status, body := term.Input(t, nodeToken, nodeSession, 1, "hostname\n"); status != http.StatusOK {
		t.Fatalf("node input = %d %v, want 200", status, body)
	}

	// The operator reads its own events with the full KOSD-C-92 metadata.
	status, own := term.AuditEvents(t, alice, aliceSession, "")
	if status != http.StatusOK {
		t.Fatalf("alice reads own audit = %d %v, want 200", status, own)
	}
	for _, item := range itemsOf(t, own) {
		event := item.(map[string]any)
		if got := event["actor"]; got != "alice" {
			t.Fatalf("alice audit actor = %v", got)
		}
		for _, field := range []string{
			"eventId", "actor", "extension", "sessionId", "targetType", "targetId",
			"targetUid", "namespace", "container", "executionIdentity", "event",
			"result", "occurredAt",
		} {
			if _, found := event[field]; !found {
				t.Fatalf("audit event omits %s: %v", field, event)
			}
		}
	}
	firstPage := inputEvents(t, itemsOf(t, own))
	if len(firstPage) != 3 {
		t.Fatalf("alice input events = %d, want 3", len(firstPage))
	}
	for index, event := range firstPage {
		if got, _ := event["sequence"].(float64); got != float64(index+1) {
			t.Fatalf("input sequence = %v, want %d", event["sequence"], index+1)
		}
		if got := event["receipt"]; got != controlplane.TerminalReceiptAccepted {
			t.Fatalf("input receipt = %v, want accepted", got)
		}
	}

	// The global Owner reads any session, including a non-Owner's.
	if status, body := term.AuditEvents(t, owner, aliceSession, ""); status != http.StatusOK {
		t.Fatalf("Owner reads alice audit = %d %v, want 200", status, body)
	}
	if status, body := term.AuditEvents(t, owner, bobSession, ""); status != http.StatusOK {
		t.Fatalf("Owner reads bob audit = %d %v, want 200", status, body)
	}

	// Another non-Owner learns nothing, and an absent session answers the same.
	status, foreign := term.AuditEvents(t, alice, bobSession, "")
	if status != http.StatusForbidden {
		t.Fatalf("alice reads bob audit = %d %v, want 403", status, foreign)
	}
	if code := harness.FailureCode(t, foreign); code != controlplane.CodeTerminalForbidden {
		t.Fatalf("foreign audit code = %s, want %s", code, controlplane.CodeTerminalForbidden)
	}
	status, absent := term.AuditEvents(t, alice, "ts-absent", "")
	if status != http.StatusForbidden {
		t.Fatalf("absent audit = %d %v, want 403", status, absent)
	}
	if !reflect.DeepEqual(foreign["error"], absent["error"]) {
		t.Fatalf("foreign %v and absent %v answers differ", foreign["error"], absent["error"])
	}
	encoded, err := json.Marshal(foreign)
	if err != nil {
		t.Fatalf("encode refusal: %v", err)
	}
	for _, forbidden := range []string{bobPod.ID, bobPod.UID, "items"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("the refusal leaks %q: %s", forbidden, encoded)
		}
	}

	// Pagination returns items and a cursor for the next page.
	status, page := term.AuditEvents(t, alice, aliceSession, "limit=2")
	if status != http.StatusOK {
		t.Fatalf("first page = %d %v, want 200", status, page)
	}
	if got := len(itemsOf(t, page)); got != 2 {
		t.Fatalf("first page items = %d, want 2", got)
	}
	cursor, _ := page["nextCursor"].(string)
	if cursor == "" {
		t.Fatalf("first page carries no cursor: %v", page)
	}
	if _, err := strconv.ParseInt(cursor, 10, 64); err != nil {
		t.Fatalf("cursor %q is not a number: %v", cursor, err)
	}
	status, rest := term.AuditEvents(t, alice, aliceSession, "limit=2&cursor="+cursor)
	if status != http.StatusOK {
		t.Fatalf("second page = %d %v, want 200", status, rest)
	}
	seen := map[float64]bool{}
	for _, item := range append(itemsOf(t, page), itemsOf(t, rest)...) {
		id := item.(map[string]any)["eventId"].(float64)
		if seen[id] {
			t.Fatalf("event %v appears on both pages", id)
		}
		seen[id] = true
	}
	if len(seen) != len(itemsOf(t, own)) {
		t.Fatalf("paged events = %d, want %d", len(seen), len(itemsOf(t, own)))
	}

	// A node session's audit carries empty namespace and container.
	status, nodeAudit := term.AuditEvents(t, owner, nodeSession, "")
	if status != http.StatusOK {
		t.Fatalf("node audit = %d %v, want 200", status, nodeAudit)
	}
	for _, item := range itemsOf(t, nodeAudit) {
		event := item.(map[string]any)
		if got := event["namespace"]; got != "" {
			t.Fatalf("node audit namespace = %v, want empty", got)
		}
		if got := event["container"]; got != "" {
			t.Fatalf("node audit container = %v, want empty", got)
		}
		if got := event["executionIdentity"]; got != controlplane.TerminalNodeIdentity {
			t.Fatalf("node audit identity = %v, want %s", got, controlplane.TerminalNodeIdentity)
		}
	}

	// Ending, uninstalling and moving target ownership delete no history.
	if status, body := term.End(t, alice, aliceSession); status != http.StatusOK {
		t.Fatalf("end alice session = %d %v, want 200", status, body)
	}
	term.Extensions.Set(harness.TerminalExtension, false)
	term.TransferApplication(t, "alice-shop", "bob")
	status, retained := term.AuditEvents(t, alice, aliceSession, "")
	if status != http.StatusOK {
		t.Fatalf("retained audit = %d %v, want 200", status, retained)
	}
	if len(itemsOf(t, retained)) < len(itemsOf(t, own)) {
		t.Fatalf("retained events = %d, want at least %d", len(itemsOf(t, retained)), len(itemsOf(t, own)))
	}

	// A database failure is a 503, never an empty page.
	term.Audit.SetDown(true)
	status, broken := term.AuditEvents(t, alice, aliceSession, "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("audit with a down database = %d %v, want 503", status, broken)
	}
	if code := harness.FailureCode(t, broken); code != controlplane.CodeTerminalAuditUnavailable {
		t.Fatalf("down database code = %s, want %s", code, controlplane.CodeTerminalAuditUnavailable)
	}
	if _, found := broken["items"]; found {
		t.Fatalf("a failed read returned an empty list: %v", broken)
	}
}

func startSession(t *testing.T, term *harness.Terminal, token string, target controlplane.TerminalTarget) (string, string) {
	t.Helper()
	status, created := term.Create(t, token, harness.TerminalExtension, target)
	if status != http.StatusCreated {
		t.Fatalf("create %s session = %d %v, want 201", target.ID, status, created)
	}
	session := stringField(t, created, "sessionId")
	createdToken := stringField(t, created, "token")
	if status, body := term.Attach(t, createdToken, session, "", nil); status != http.StatusOK {
		t.Fatalf("attach %s = %d %v, want 200", target.ID, status, body)
	}
	return session, createdToken
}

func createAttached(t *testing.T, term *harness.Terminal, token string, target controlplane.TerminalTarget) (string, string) {
	t.Helper()
	status, created := term.Create(t, token, harness.TerminalExtension, target)
	if status != http.StatusCreated {
		t.Fatalf("create %s session = %d %v, want 201", target.ID, status, created)
	}
	session := stringField(t, created, "sessionId")
	createdToken := stringField(t, created, "token")
	if status, body := term.Attach(t, createdToken, session, "", nil); status != http.StatusOK {
		t.Fatalf("attach %s = %d %v, want 200", target.ID, status, body)
	}
	return session, createdToken
}

func itemsOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("audit answer carries no items: %v", body)
	}
	return items
}

func inputEvents(t *testing.T, items []any) []map[string]any {
	t.Helper()
	inputs := make([]map[string]any, 0)
	for _, item := range items {
		event := item.(map[string]any)
		if event["event"] == controlplane.TerminalEventInput {
			inputs = append(inputs, event)
		}
	}
	return inputs
}

func stringField(t *testing.T, body map[string]any, name string) string {
	t.Helper()
	value, _ := body[name].(string)
	if value == "" {
		t.Fatalf("%s is missing from %v", name, body)
	}
	return value
}
