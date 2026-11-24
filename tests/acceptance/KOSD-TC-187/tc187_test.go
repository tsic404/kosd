// Package kosdtc187 verifies the node target inventory of KOSD-AC-86,
// KOSD-TC-187: the Owner reads the target fields, execution identity and
// observation metadata, a Member learns nothing, a same-name replacement has a
// new UID and does not inherit the old session, node namespace and container
// stay empty, and an unavailable source is a 503 rather than an empty list.
package kosdtc187

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

func TestTC187NodeInventoryAndImmutableIdentity(t *testing.T) {
	cluster := harness.Start(t)
	source := &harness.ToggleSource{Source: controlplane.NewClusterSource(cluster.Dynamic)}
	term := cluster.StartTerminal(t, harness.TerminalOptions{
		Extensions: []string{harness.TerminalExtension},
		Source:     source,
	})
	owner := term.OwnerToken(t)
	alice := term.AccountToken(t, "alice", "Member")
	first := term.NodeTarget(t, "node-a")
	second := term.NodeTarget(t, "node-b")

	status, listing := term.Nodes(t, owner, harness.TerminalExtension)
	if status != http.StatusOK {
		t.Fatalf("Owner reads nodes = %d %v, want 200", status, listing)
	}
	items := itemsOf(t, listing)
	if len(items) != 2 {
		t.Fatalf("node items = %d, want 2", len(items))
	}
	seen := map[string]map[string]any{}
	for _, item := range items {
		node := item.(map[string]any)
		seen[stringField(t, node, "targetId")] = node
	}
	for _, target := range []controlplane.TerminalTarget{first, second} {
		node, found := seen[target.ID]
		if !found {
			t.Fatalf("node inventory omits %s: %v", target.ID, listing)
		}
		if got := node["targetType"]; got != controlplane.TerminalTargetTypeNode {
			t.Fatalf("%s targetType = %v", target.ID, got)
		}
		if got := node["targetUid"]; got != target.UID {
			t.Fatalf("%s targetUid = %v, want %s", target.ID, got, target.UID)
		}
		if got := node["namespace"]; got != "" {
			t.Fatalf("%s namespace = %v, want empty", target.ID, got)
		}
		if got := node["container"]; got != "" {
			t.Fatalf("%s container = %v, want empty", target.ID, got)
		}
		identities, _ := node["executionIdentities"].([]any)
		if len(identities) != 1 || identities[0] != controlplane.TerminalNodeIdentity {
			t.Fatalf("%s execution identities = %v", target.ID, node["executionIdentities"])
		}
		observed, _ := node["observedAt"].(string)
		if _, err := time.Parse(time.RFC3339Nano, observed); err != nil {
			t.Fatalf("%s observedAt = %q: %v", target.ID, observed, err)
		}
		if stale, found := node["stale"]; !found || stale != false {
			t.Fatalf("%s stale = %v, want a fresh false", target.ID, node["stale"])
		}
	}

	// A non-Owner gets no target fields and learns no count.
	status, refused := term.Nodes(t, alice, harness.TerminalExtension)
	if status != http.StatusForbidden {
		t.Fatalf("Member reads nodes = %d %v, want 403", status, refused)
	}
	if code := harness.FailureCode(t, refused); code != controlplane.CodeTerminalForbidden {
		t.Fatalf("Member nodes code = %s, want %s", code, controlplane.CodeTerminalForbidden)
	}
	encoded, err := json.Marshal(refused)
	if err != nil {
		t.Fatalf("encode refusal: %v", err)
	}
	for _, forbidden := range []string{"items", first.ID, first.UID, second.ID} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("the Member refusal leaks %q: %s", forbidden, encoded)
		}
	}

	// The session is bound to the old UID; a same-name replacement does not
	// inherit it.
	status, created := term.Create(t, owner, harness.TerminalExtension, first)
	if status != http.StatusCreated {
		t.Fatalf("create node session = %d %v, want 201", status, created)
	}
	session := stringField(t, created, "sessionId")
	token := stringField(t, created, "token")
	if status, body := term.Attach(t, token, session, "", nil); status != http.StatusOK {
		t.Fatalf("attach = %d %v, want 200", status, body)
	}

	cluster.DeleteNode(t, first.ID)
	replacement := term.NodeTarget(t, first.ID)
	if replacement.UID == first.UID {
		t.Fatalf("the replacement kept the old UID %s", first.UID)
	}
	status, body := term.Attach(t, token, session, "", nil)
	if status != http.StatusForbidden {
		t.Fatalf("old token on the replacement = %d %v, want 403", status, body)
	}
	if code := harness.FailureCode(t, body); code != controlplane.CodeTerminalTargetUnavailable {
		t.Fatalf("old token code = %s, want %s", code, controlplane.CodeTerminalTargetUnavailable)
	}
	if got := len(term.Executor.Batches(first.UID)); got != 0 {
		t.Fatalf("the old UID received %d batches", got)
	}
	status, read := term.Status(t, owner, session)
	if status != http.StatusOK {
		t.Fatalf("status after replacement = %d %v, want 200", status, read)
	}
	if got := read["state"]; got != controlplane.TerminalStateFailed {
		t.Fatalf("state after replacement = %v, want failed", got)
	}
	if status, body = term.Input(t, token, session, 1, "hostname\n"); status != http.StatusUnauthorized {
		t.Fatalf("input after replacement = %d %v, want 401", status, body)
	}

	status, listing = term.Nodes(t, owner, harness.TerminalExtension)
	if status != http.StatusOK {
		t.Fatalf("nodes after replacement = %d %v, want 200", status, listing)
	}
	replaced := map[string]map[string]any{}
	for _, item := range itemsOf(t, listing) {
		node := item.(map[string]any)
		replaced[stringField(t, node, "targetId")] = node
	}
	if got := replaced[first.ID]["targetUid"]; got != replacement.UID {
		t.Fatalf("inventory %s UID = %v, want %s", first.ID, got, replacement.UID)
	}

	// A source failure is an error, never a successful empty inventory.
	source.SetDown(true)
	status, broken := term.Nodes(t, owner, harness.TerminalExtension)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("nodes with a down source = %d %v, want 503", status, broken)
	}
	if code := harness.FailureCode(t, broken); code != controlplane.CodeTerminalTargetSourceUnavailable {
		t.Fatalf("down source code = %s, want %s", code, controlplane.CodeTerminalTargetSourceUnavailable)
	}
	if _, found := broken["items"]; found {
		t.Fatalf("a failed source returned an inventory: %v", broken)
	}
}

func itemsOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("answer carries no items: %v", body)
	}
	return items
}

func stringField(t *testing.T, body map[string]any, name string) string {
	t.Helper()
	value, _ := body[name].(string)
	if value == "" {
		t.Fatalf("%s is missing from %v", name, body)
	}
	return value
}
