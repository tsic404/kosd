// Package kosdtc188 verifies the terminal session view and state machine of
// KOSD-AC-82, KOSD-TC-188: create and status carry every KOSD-C-90 field, a
// node uses empty namespace and container, only connected sessions accept
// input, repeated creates take no new slot or token, and ending or failing a
// session is observable.
package kosdtc188

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

func TestTC188CreateFieldsAndConnectionState(t *testing.T) {
	cluster := harness.Start(t)
	term := cluster.StartTerminal(t, harness.TerminalOptions{
		Extensions: []string{harness.TerminalExtension},
	})
	owner := term.OwnerToken(t)
	term.Executor.SetOutput("hello")

	pod := term.PodTarget(t, "shop", "root")
	node := term.NodeTarget(t, "node-a")

	status, createdNode := term.Create(t, owner, harness.TerminalExtension, node)
	if status != http.StatusCreated {
		t.Fatalf("create node = %d %v, want 201", status, createdNode)
	}
	assertViewFields(t, createdNode)
	nodeSession := stringField(t, createdNode, "sessionId")
	nodeToken := stringField(t, createdNode, "token")
	assertField(t, createdNode, "targetType", controlplane.TerminalTargetTypeNode)
	assertField(t, createdNode, "targetId", node.ID)
	assertField(t, createdNode, "targetUid", node.UID)
	assertField(t, createdNode, "namespace", "")
	assertField(t, createdNode, "container", "")
	assertField(t, createdNode, "executionIdentity", controlplane.TerminalNodeIdentity)
	assertField(t, createdNode, "state", controlplane.TerminalStateConnecting)
	assertLimits(t, createdNode, 1, 1)
	if got := createdNode["idleTimeoutMinutes"].(float64); got != controlplane.TerminalDefaultIdleMinutes {
		t.Fatalf("idleTimeoutMinutes = %v, want %d", got, controlplane.TerminalDefaultIdleMinutes)
	}

	// The same create request reuses the session, takes no second slot and
	// delivers no second token.
	status, repeated := term.Create(t, owner, harness.TerminalExtension, node)
	if status != http.StatusOK {
		t.Fatalf("repeat create = %d %v, want 200", status, repeated)
	}
	if got := stringField(t, repeated, "sessionId"); got != nodeSession {
		t.Fatalf("repeat create session = %q, want %q", got, nodeSession)
	}
	if _, found := repeated["token"]; found {
		t.Fatalf("repeat create delivered another token: %v", repeated)
	}
	assertLimits(t, repeated, 1, 1)

	status, createdPod := term.Create(t, owner, harness.TerminalExtension, pod)
	if status != http.StatusCreated {
		t.Fatalf("create Pod = %d %v, want 201", status, createdPod)
	}
	podSession := stringField(t, createdPod, "sessionId")
	podToken := stringField(t, createdPod, "token")
	assertField(t, createdPod, "targetType", controlplane.TerminalTargetTypePod)
	assertField(t, createdPod, "targetId", pod.ID)
	assertField(t, createdPod, "targetUid", pod.UID)
	assertField(t, createdPod, "namespace", pod.Namespace)
	assertField(t, createdPod, "container", pod.Container)
	assertField(t, createdPod, "executionIdentity", "container:"+pod.Namespace+"/"+pod.ID+"/"+pod.Container)
	assertLimits(t, createdPod, 2, 1)

	// A connecting session accepts no input.
	status, body := term.Input(t, podToken, podSession, 1, "id\n")
	if status != http.StatusConflict {
		t.Fatalf("input while connecting = %d %v, want 409", status, body)
	}
	if got := len(term.Executor.Batches(pod.UID)); got != 0 {
		t.Fatalf("a connecting session delivered %d batches", got)
	}

	status, attached := term.Attach(t, podToken, podSession, "", nil)
	if status != http.StatusOK {
		t.Fatalf("attach = %d %v, want 200", status, attached)
	}
	assertField(t, attached, "state", controlplane.TerminalStateConnected)

	status, read := term.Status(t, owner, podSession)
	if status != http.StatusOK {
		t.Fatalf("status = %d %v, want 200", status, read)
	}
	assertField(t, read, "state", controlplane.TerminalStateConnected)
	assertLimits(t, read, 2, 1)

	status, receipt := term.Input(t, podToken, podSession, 1, "id\n")
	if status != http.StatusOK {
		t.Fatalf("connected input = %d %v, want 200", status, receipt)
	}
	if got := receipt["result"]; got != controlplane.TerminalReceiptAccepted {
		t.Fatalf("input result = %v, want accepted", got)
	}
	if got := receipt["output"]; got != base64.StdEncoding.EncodeToString([]byte("hello")) {
		t.Fatalf("input output = %v, want the executor output", got)
	}
	if got := len(term.Executor.Batches(pod.UID)); got != 1 {
		t.Fatalf("target received %d batches, want 1", got)
	}

	status, ended := term.End(t, owner, podSession)
	if status != http.StatusOK {
		t.Fatalf("end = %d %v, want 200", status, ended)
	}
	assertField(t, ended, "state", controlplane.TerminalStateClosed)
	assertField(t, ended, "reason", controlplane.TerminalReasonEnded)
	if got := len(term.Executor.Batches(pod.UID)); got != 1 {
		t.Fatalf("ending replayed input: %d batches, want 1", got)
	}
	if status, body = term.Input(t, podToken, podSession, 2, "id\n"); status != http.StatusUnauthorized {
		t.Fatalf("input after end = %d %v, want 401", status, body)
	}

	// A failed connection is a distinct observable state.
	failing := term.PodTarget(t, "broken", "root")
	status, createdFailing := term.Create(t, owner, harness.TerminalExtension, failing)
	if status != http.StatusCreated {
		t.Fatalf("create failing session = %d %v, want 201", status, createdFailing)
	}
	failingSession := stringField(t, createdFailing, "sessionId")
	failingToken := stringField(t, createdFailing, "token")
	term.Executor.FailOpen(failing.UID, controlplane.ErrTerminalExecutorUnavailable)
	status, body = term.Attach(t, failingToken, failingSession, "", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("failed attach = %d %v, want 503", status, body)
	}
	if code := harness.FailureCode(t, body); code != controlplane.CodeTerminalExecutorUnavailable {
		t.Fatalf("failed attach code = %s, want %s", code, controlplane.CodeTerminalExecutorUnavailable)
	}
	status, read = term.Status(t, owner, failingSession)
	if status != http.StatusOK {
		t.Fatalf("failed status = %d %v, want 200", status, read)
	}
	assertField(t, read, "state", controlplane.TerminalStateFailed)

	if nodeToken == "" {
		t.Fatal("the node create delivered no token")
	}
}

func assertViewFields(t *testing.T, body map[string]any) {
	t.Helper()
	for _, field := range []string{
		"sessionId", "targetType", "targetId", "targetUid", "namespace", "container",
		"executionIdentity", "state", "expiresAt", "idleTimeoutMinutes", "limits", "usage",
	} {
		if _, found := body[field]; !found {
			t.Fatalf("the view omits %s: %v", field, body)
		}
	}
}

func assertLimits(t *testing.T, body map[string]any, account, target int) {
	t.Helper()
	limits, _ := body["limits"].(map[string]any)
	usage, _ := body["usage"].(map[string]any)
	if got := limits["account"]; got != float64(controlplane.TerminalAccountLimit) {
		t.Fatalf("limits.account = %v, want %d", got, controlplane.TerminalAccountLimit)
	}
	if got := limits["target"]; got != float64(controlplane.TerminalTargetLimit) {
		t.Fatalf("limits.target = %v, want %d", got, controlplane.TerminalTargetLimit)
	}
	if got := usage["account"]; got != float64(account) {
		t.Fatalf("usage.account = %v, want %d", got, account)
	}
	if got := usage["target"]; got != float64(target) {
		t.Fatalf("usage.target = %v, want %d", got, target)
	}
}

func assertField(t *testing.T, body map[string]any, name, want string) {
	t.Helper()
	if got, _ := body[name].(string); got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
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
