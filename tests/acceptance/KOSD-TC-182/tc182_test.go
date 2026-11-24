// Package kosdtc182 verifies the terminal token binding and session exit of
// KOSD-AC-82, KOSD-TC-182: a token is bound to its account, session, target,
// container and entry origin; it is delivered once, lives five minutes, dies
// with renewal, end or expiry, and no exit replays input or adds an object.
package kosdtc182

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const origin = "https://console.example"

func TestTC182TokenBindingAndSessionExit(t *testing.T) {
	cluster := harness.Start(t)
	term := cluster.StartTerminal(t, harness.TerminalOptions{
		Extensions: []string{harness.TerminalExtension},
	})
	owner := term.OwnerToken(t)
	pod := term.PodTarget(t, "shop", "root")
	node := term.NodeTarget(t, "node-a")
	second := term.PodTarget(t, "expiring", "root")
	alice := term.AccountToken(t, "alice", "Member")
	memberPod := term.PodTarget(t, "member-shop", "alice")

	beforeNodes := nodeIdentities(t, cluster)
	beforePods := podIdentities(t, cluster)

	status, createdPod := term.CreateWith(t, owner, harness.TerminalExtension, pod, map[string]string{"Origin": origin})
	if status != http.StatusCreated {
		t.Fatalf("create Pod session = %d %v, want 201", status, createdPod)
	}
	podSession := stringField(t, createdPod, "sessionId")
	podToken := stringField(t, createdPod, "token")
	if podToken == "" {
		t.Fatal("the create response delivered no token")
	}
	if got := createdPod["state"]; got != controlplane.TerminalStateConnecting {
		t.Fatalf("new state = %v, want connecting", got)
	}
	expires := timeField(t, createdPod, "expiresAt")
	if want := term.Clock.Now().Add(controlplane.TerminalTokenLifetime); !expires.Equal(want) {
		t.Fatalf("expiresAt = %s, want %s", expires, want)
	}

	status, createdNode := term.CreateWith(t, owner, harness.TerminalExtension, node, map[string]string{"Origin": origin})
	if status != http.StatusCreated {
		t.Fatalf("create Node session = %d %v, want 201", status, createdNode)
	}
	nodeSession := stringField(t, createdNode, "sessionId")
	nodeToken := stringField(t, createdNode, "token")
	if got := createdNode["namespace"]; got != "" {
		t.Fatalf("node namespace = %v, want empty", got)
	}
	if got := createdNode["container"]; got != "" {
		t.Fatalf("node container = %v, want empty", got)
	}
	if got := createdNode["executionIdentity"]; got != controlplane.TerminalNodeIdentity {
		t.Fatalf("node execution identity = %v, want %s", got, controlplane.TerminalNodeIdentity)
	}

	// The status read carries the metadata but never the plaintext token.
	status, read := term.Status(t, owner, podSession)
	if status != http.StatusOK {
		t.Fatalf("status = %d %v, want 200", status, read)
	}
	if _, found := read["token"]; found {
		t.Fatalf("status returned a token: %v", read)
	}
	encoded, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("encode status: %v", err)
	}
	if strings.Contains(string(encoded), podToken) || strings.Contains(string(encoded), nodeToken) {
		t.Fatal("status metadata contains a plaintext token")
	}

	// Any mismatch of account, session, target or entry origin is forbidden and
	// never falls back to the cookie identity (KOSD-C-91).
	swaps := []struct {
		name    string
		session string
		token   string
		query   string
		origin  string
	}{
		{name: "another session", session: nodeSession, token: podToken, origin: origin},
		{name: "another target", session: podSession, token: podToken, query: "targetId=" + url.QueryEscape(node.ID), origin: origin},
		{name: "another account", session: podSession, token: podToken, query: "account=alice", origin: origin},
		{name: "another origin", session: podSession, token: podToken, origin: "https://other.example"},
		{name: "no origin", session: podSession, token: podToken},
		{name: "node token on the Pod path", session: podSession, token: nodeToken, origin: origin},
	}
	for _, attempt := range swaps {
		headers := map[string]string{}
		if attempt.origin != "" {
			headers["Origin"] = attempt.origin
		}
		status, body := term.Attach(t, attempt.token, attempt.session, attempt.query, headers)
		if status != http.StatusForbidden {
			t.Fatalf("%s = %d %v, want 403", attempt.name, status, body)
		}
		if code := harness.FailureCode(t, body); code != controlplane.CodeTerminalForbidden {
			t.Fatalf("%s code = %s, want %s", attempt.name, code, controlplane.CodeTerminalForbidden)
		}
	}
	if got := len(term.Executor.Batches(pod.UID)); got != 0 {
		t.Fatalf("a bound-token mismatch delivered %d batches", got)
	}

	status, attached := term.Attach(t, podToken, podSession, "", map[string]string{"Origin": origin})
	if status != http.StatusOK {
		t.Fatalf("attach = %d %v, want 200", status, attached)
	}
	if got := attached["state"]; got != controlplane.TerminalStateConnected {
		t.Fatalf("attached state = %v, want connected", got)
	}
	status, receipt := term.InputWith(t, podToken, podSession, 1, "echo first\n", map[string]string{"Origin": origin})
	if status != http.StatusOK {
		t.Fatalf("input = %d %v, want 200", status, receipt)
	}
	if got := len(term.Executor.Batches(pod.UID)); got != 1 {
		t.Fatalf("target received %d batches, want 1", got)
	}

	// Renewal replaces the token; the old one is dead and the idle clock stays.
	status, renewed := term.Renew(t, owner, podSession)
	if status != http.StatusOK {
		t.Fatalf("renew = %d %v, want 200", status, renewed)
	}
	renewedToken := stringField(t, renewed, "token")
	if renewedToken == "" || renewedToken == podToken {
		t.Fatalf("renewal token = %q, want a new delivered token", renewedToken)
	}
	if got := timeField(t, renewed, "expiresAt"); !got.Equal(term.Clock.Now().Add(controlplane.TerminalTokenLifetime)) {
		t.Fatalf("renewed expiresAt = %s, want five minutes from now", got)
	}
	status, body := term.Attach(t, podToken, podSession, "", map[string]string{"Origin": origin})
	if status != http.StatusUnauthorized {
		t.Fatalf("old token after renewal = %d %v, want 401", status, body)
	}
	if code := harness.FailureCode(t, body); code != controlplane.CodeTerminalUnauthenticated {
		t.Fatalf("old token code = %s, want %s", code, controlplane.CodeTerminalUnauthenticated)
	}
	if status, attached = term.Attach(t, renewedToken, podSession, "", map[string]string{"Origin": origin}); status != http.StatusOK {
		t.Fatalf("renewed token attach = %d %v, want 200", status, attached)
	}

	// Ending the session kills its token and replays nothing.
	status, ended := term.End(t, owner, podSession)
	if status != http.StatusOK {
		t.Fatalf("end = %d %v, want 200", status, ended)
	}
	if got := ended["state"]; got != controlplane.TerminalStateClosed {
		t.Fatalf("ended state = %v, want closed", got)
	}
	if status, body = term.Attach(t, renewedToken, podSession, "", map[string]string{"Origin": origin}); status != http.StatusUnauthorized {
		t.Fatalf("ended session attach = %d %v, want 401", status, body)
	}
	if status, body = term.Input(t, renewedToken, podSession, 2, "echo second\n"); status != http.StatusUnauthorized {
		t.Fatalf("ended session input = %d %v, want 401", status, body)
	}
	if got := len(term.Executor.Batches(pod.UID)); got != 1 {
		t.Fatalf("ending replayed input: %d batches, want 1", got)
	}

	// Only the session's own account may renew it. An Owner renewal would
	// otherwise deliver a token bound to the member account, misattribute the
	// Owner's later input and revoke the member's token (KOSD-C-91, KOSD-C-92).
	status, memberCreated := term.Create(t, alice, harness.TerminalExtension, memberPod)
	if status != http.StatusCreated {
		t.Fatalf("member create = %d %v, want 201", status, memberCreated)
	}
	memberSession := stringField(t, memberCreated, "sessionId")
	memberToken := stringField(t, memberCreated, "token")
	if status, attached = term.Attach(t, memberToken, memberSession, "", nil); status != http.StatusOK {
		t.Fatalf("member attach = %d %v, want 200", status, attached)
	}
	if status, receipt = term.Input(t, memberToken, memberSession, 1, "echo member\n"); status != http.StatusOK {
		t.Fatalf("member input = %d %v, want 200", status, receipt)
	}
	status, refusedRenew := term.Renew(t, owner, memberSession)
	if status != http.StatusForbidden {
		t.Fatalf("Owner renew of a member session = %d %v, want 403", status, refusedRenew)
	}
	if code := harness.FailureCode(t, refusedRenew); code != controlplane.CodeTerminalForbidden {
		t.Fatalf("Owner renew code = %s, want %s", code, controlplane.CodeTerminalForbidden)
	}
	if _, found := refusedRenew["token"]; found {
		t.Fatalf("a refused renewal delivered a token: %v", refusedRenew)
	}
	if status, receipt = term.Input(t, memberToken, memberSession, 2, "echo still\n"); status != http.StatusOK {
		t.Fatalf("member token after the refused renewal = %d %v, want 200", status, receipt)
	}
	if got := len(term.Executor.Batches(memberPod.UID)); got != 2 {
		t.Fatalf("member target received %d batches, want 2", got)
	}
	if status, body = term.End(t, alice, memberSession); status != http.StatusOK {
		t.Fatalf("member end = %d %v, want 200", status, body)
	}

	// An unrenewed token expires after five minutes and the session closes.
	status, expiring := term.CreateWith(t, owner, harness.TerminalExtension, second, map[string]string{"Origin": origin})
	if status != http.StatusCreated {
		t.Fatalf("create expiring session = %d %v, want 201", status, expiring)
	}
	expiringSession := stringField(t, expiring, "sessionId")
	expiringToken := stringField(t, expiring, "token")
	term.Clock.Advance(controlplane.TerminalTokenLifetime + time.Second)
	status, read = term.Status(t, owner, expiringSession)
	if status != http.StatusOK {
		t.Fatalf("expired status = %d %v, want 200", status, read)
	}
	if got := read["state"]; got != controlplane.TerminalStateClosed {
		t.Fatalf("expired state = %v, want closed", got)
	}
	if got := read["reason"]; got != controlplane.TerminalReasonTokenExpired {
		t.Fatalf("expired reason = %v, want %s", got, controlplane.TerminalReasonTokenExpired)
	}
	if status, body = term.Attach(t, expiringToken, expiringSession, "", map[string]string{"Origin": origin}); status != http.StatusUnauthorized {
		t.Fatalf("expired token attach = %d %v, want 401", status, body)
	}

	// A terminal session is temporary state: no kernel object appeared.
	if got, want := nodeIdentities(t, cluster), beforeNodes; !sameIdentities(got, want) {
		t.Fatalf("node set changed: %v, want %v", got, want)
	}
	if got, want := podIdentities(t, cluster), beforePods; !sameIdentities(got, want) {
		t.Fatalf("Pod set changed: %v, want %v", got, want)
	}
}

func stringField(t *testing.T, body map[string]any, name string) string {
	t.Helper()
	value, _ := body[name].(string)
	if value == "" {
		t.Fatalf("%s is missing from %v", name, body)
	}
	return value
}

func timeField(t *testing.T, body map[string]any, name string) time.Time {
	t.Helper()
	value := stringField(t, body, name)
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("%s = %q is not a timestamp: %v", name, value, err)
	}
	return parsed
}

func nodeIdentities(t *testing.T, cluster *harness.Cluster) map[string]string {
	t.Helper()
	identities := map[string]string{}
	for _, node := range cluster.Nodes(t) {
		identities[node.GetName()] = string(node.GetUID())
	}
	return identities
}

func podIdentities(t *testing.T, cluster *harness.Cluster) map[string]string {
	t.Helper()
	identities := map[string]string{}
	for _, pod := range cluster.Pods(t, harness.Namespace) {
		identities[pod.GetName()] = string(pod.GetUID())
	}
	return identities
}

func sameIdentities(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, uid := range left {
		if right[name] != uid {
			return false
		}
	}
	return true
}
