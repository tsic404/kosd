// Package kosdtc183 verifies the unified terminal audit of KOSD-AC-83,
// KOSD-TC-183: establish, input, duplicate, renewal and end are associated with
// one session and its target; no raw stream or token enters the trail; a
// duplicate sequence is delivered once; and an unavailable trail refuses new
// work and ends the live sessions.
package kosdtc183

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const secret = "audit-secret-183"

func TestTC183SessionAuditAndRefusal(t *testing.T) {
	cluster := harness.Start(t)
	term := cluster.StartTerminal(t, harness.TerminalOptions{
		Extensions: []string{harness.TerminalExtension},
	})
	owner := term.OwnerToken(t)
	term.Executor.SetOutput("audit-output-secret")

	pod := term.PodTarget(t, "shop", "root")
	status, created := term.Create(t, owner, harness.TerminalExtension, pod)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %v, want 201", status, created)
	}
	session := stringField(t, created, "sessionId")
	token := stringField(t, created, "token")
	if status, body := term.Attach(t, token, session, "", nil); status != http.StatusOK {
		t.Fatalf("attach = %d %v, want 200", status, body)
	}
	if status, body := term.Input(t, token, session, 1, "echo "+secret+"\n"); status != http.StatusOK {
		t.Fatalf("input = %d %v, want 200", status, body)
	}
	status, duplicate := term.Input(t, token, session, 1, "echo "+secret+"\n")
	if status != http.StatusOK {
		t.Fatalf("duplicate input = %d %v, want 200", status, duplicate)
	}
	if got := duplicate["duplicate"]; got != true {
		t.Fatalf("duplicate input answer = %v, want duplicate", duplicate)
	}
	if got := len(term.Executor.Batches(pod.UID)); got != 1 {
		t.Fatalf("target received %d batches, want one delivery", got)
	}
	status, renewed := term.Renew(t, owner, session)
	if status != http.StatusOK {
		t.Fatalf("renew = %d %v, want 200", status, renewed)
	}
	renewedToken := stringField(t, renewed, "token")
	if status, body := term.End(t, owner, session); status != http.StatusOK {
		t.Fatalf("end = %d %v, want 200", status, body)
	}

	status, audit := term.AuditEvents(t, owner, session, "")
	if status != http.StatusOK {
		t.Fatalf("audit read = %d %v, want 200", status, audit)
	}
	items := itemsOf(t, audit)
	events := map[string]map[string]any{}
	for _, item := range items {
		event := item.(map[string]any)
		if got := event["sessionId"]; got != session {
			t.Fatalf("event session = %v, want %s", got, session)
		}
		if got := event["extension"]; got != harness.TerminalExtension {
			t.Fatalf("event extension = %v, want %s", got, harness.TerminalExtension)
		}
		if got := event["actor"]; got != "root" {
			t.Fatalf("event actor = %v, want root", got)
		}
		if got := event["targetId"]; got != pod.ID {
			t.Fatalf("event targetId = %v, want %s", got, pod.ID)
		}
		if got := event["targetUid"]; got != pod.UID {
			t.Fatalf("event targetUid = %v, want %s", got, pod.UID)
		}
		if got := event["container"]; got != pod.Container {
			t.Fatalf("event container = %v, want %s", got, pod.Container)
		}
		if got := event["executionIdentity"]; got != "container:"+pod.Namespace+"/"+pod.ID+"/"+pod.Container {
			t.Fatalf("event execution identity = %v", got)
		}
		name, _ := event["event"].(string)
		events[name+"/"+resultOf(event)] = event
	}
	for _, want := range []string{
		controlplane.TerminalEventEstablish + "/" + controlplane.ResultAccepted,
		controlplane.TerminalEventInput + "/" + controlplane.TerminalReceiptAccepted,
		controlplane.TerminalEventInput + "/" + controlplane.TerminalReceiptDuplicate,
		controlplane.TerminalEventRenew + "/" + controlplane.ResultAccepted,
		controlplane.TerminalEventEnd + "/" + controlplane.ResultAccepted,
	} {
		if _, found := events[want]; !found {
			t.Fatalf("audit omits %s: %v", want, audit)
		}
	}
	input := events[controlplane.TerminalEventInput+"/"+controlplane.TerminalReceiptAccepted]
	sequence, _ := input["sequence"].(float64)
	if sequence != 1 {
		t.Fatalf("input sequence = %v, want 1", input["sequence"])
	}
	if got := input["receipt"]; got != controlplane.TerminalReceiptAccepted {
		t.Fatalf("input receipt = %v, want accepted", got)
	}
	duplicateEvent := events[controlplane.TerminalEventInput+"/"+controlplane.TerminalReceiptDuplicate]
	if got, _ := duplicateEvent["sequence"].(float64); got != 1 {
		t.Fatalf("duplicate sequence = %v, want 1", duplicateEvent["sequence"])
	}

	encoded, err := json.Marshal(audit)
	if err != nil {
		t.Fatalf("encode audit: %v", err)
	}
	for name, forbidden := range map[string]string{
		"raw input": secret,
		"output":    "audit-output-secret",
		"old token": token,
		"new token": renewedToken,
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("the audit trail contains the %s", name)
		}
	}
	if strings.Contains(string(encoded), `"data"`) {
		t.Fatalf("the audit trail carries raw batch data: %s", encoded)
	}

	// Two concurrent submissions of one sequence must deliver once: the second
	// sees the sequence claimed while the first is still in Send (KOSD-C-92).
	concurrent := term.PodTarget(t, "concurrent", "root")
	status, created = term.Create(t, owner, harness.TerminalExtension, concurrent)
	if status != http.StatusCreated {
		t.Fatalf("create concurrent = %d %v, want 201", status, created)
	}
	concurrentSession := stringField(t, created, "sessionId")
	concurrentToken := stringField(t, created, "token")
	if status, body := term.Attach(t, concurrentToken, concurrentSession, "", nil); status != http.StatusOK {
		t.Fatalf("attach concurrent = %d %v, want 200", status, body)
	}
	type inputAnswer struct {
		status int
		body   map[string]any
		err    error
	}
	submit := func(sessionID, token string, sequence uint64) inputAnswer {
		status, payload, err := term.HTTP.Raw(http.MethodPost, "/api/v1/terminal/sessions/"+sessionID+"/input",
			map[string]any{
				"sequence": sequence,
				"data":     base64.StdEncoding.EncodeToString([]byte("echo same\n")),
			}, map[string]string{"Authorization": "Bearer " + token})
		if err != nil {
			return inputAnswer{err: err}
		}
		body := map[string]any{}
		if err := json.Unmarshal(payload, &body); err != nil {
			return inputAnswer{err: err}
		}
		return inputAnswer{status: status, body: body}
	}
	release := term.Executor.HoldSend()
	answers := make(chan inputAnswer, 2)
	go func() { answers <- submit(concurrentSession, concurrentToken, 7) }()
	go func() { answers <- submit(concurrentSession, concurrentToken, 7) }()
	var duplicateAnswer inputAnswer
	select {
	case duplicateAnswer = <-answers:
	case <-time.After(10 * time.Second):
		release()
		t.Fatal("the duplicate input waited for the in-flight delivery")
	}
	if duplicateAnswer.err != nil {
		release()
		t.Fatalf("duplicate input: %v", duplicateAnswer.err)
	}
	if duplicateAnswer.status != http.StatusOK || duplicateAnswer.body["duplicate"] != true {
		release()
		t.Fatalf("duplicate input = %d %v, want 200 duplicate", duplicateAnswer.status, duplicateAnswer.body)
	}
	release()
	var first inputAnswer
	select {
	case first = <-answers:
	case <-time.After(10 * time.Second):
		t.Fatal("the first input did not return after the delivery was released")
	}
	if first.err != nil {
		t.Fatalf("first input: %v", first.err)
	}
	if first.status != http.StatusOK || first.body["duplicate"] == true {
		t.Fatalf("first input = %d %v, want one accepted delivery", first.status, first.body)
	}
	if got := len(term.Executor.Batches(concurrent.UID)); got != 1 {
		t.Fatalf("concurrent duplicate delivered %d batches, want 1", got)
	}
	if status, body := term.End(t, owner, concurrentSession); status != http.StatusOK {
		t.Fatalf("end concurrent = %d %v, want 200", status, body)
	}

	// A failed delivery must keep its sequence claimed until the session leaves
	// the connected state, so a duplicate submitted in that window cannot reach
	// Send again (KOSD-C-92).
	failing := term.PodTarget(t, "failing", "root")
	status, created = term.Create(t, owner, harness.TerminalExtension, failing)
	if status != http.StatusCreated {
		t.Fatalf("create failing = %d %v, want 201", status, created)
	}
	failingSession := stringField(t, created, "sessionId")
	failingToken := stringField(t, created, "token")
	if status, body := term.Attach(t, failingToken, failingSession, "", nil); status != http.StatusOK {
		t.Fatalf("attach failing = %d %v, want 200", status, body)
	}
	var windowAnswer inputAnswer
	failure := &failWindowError{err: controlplane.ErrTerminalExecutorUnavailable}
	failure.onIs = func() {
		windowAnswer = submit(failingSession, failingToken, 9)
	}
	term.Executor.FailSend(failing.UID, failure)
	failedAnswer := make(chan inputAnswer, 1)
	go func() { failedAnswer <- submit(failingSession, failingToken, 9) }()
	var firstFailure inputAnswer
	select {
	case firstFailure = <-failedAnswer:
	case <-time.After(10 * time.Second):
		t.Fatal("the failing input did not return")
	}
	if firstFailure.err != nil {
		t.Fatalf("failing input: %v", firstFailure.err)
	}
	if firstFailure.status != http.StatusServiceUnavailable {
		t.Fatalf("failing input = %d %v, want 503", firstFailure.status, firstFailure.body)
	}
	if windowAnswer.err != nil {
		t.Fatalf("duplicate in the failure window: %v", windowAnswer.err)
	}
	if windowAnswer.status != http.StatusOK || windowAnswer.body["duplicate"] != true {
		t.Fatalf("duplicate in the failure window = %d %v, want 200 duplicate", windowAnswer.status, windowAnswer.body)
	}
	if got := term.Executor.Sends(failing.UID); got != 1 {
		t.Fatalf("the failure window delivered the sequence %d times, want 1", got)
	}
	status, read := term.Status(t, owner, failingSession)
	if status != http.StatusOK {
		t.Fatalf("status after the failed delivery = %d %v, want 200", status, read)
	}
	if got := read["state"]; got != controlplane.TerminalStateFailed {
		t.Fatalf("state after the failed delivery = %v, want failed", got)
	}
	if usage, ok := read["usage"].(map[string]any); !ok || usage["account"] != float64(0) || usage["target"] != float64(0) {
		t.Fatalf("usage after the failed delivery = %v, want 0/0", read["usage"])
	}

	// An unavailable trail refuses new input before delivery and ends the live
	// sessions (KOSD-C-92).
	second := term.PodTarget(t, "second", "root")
	status, created = term.Create(t, owner, harness.TerminalExtension, second)
	if status != http.StatusCreated {
		t.Fatalf("create second = %d %v, want 201", status, created)
	}
	secondSession := stringField(t, created, "sessionId")
	secondToken := stringField(t, created, "token")
	if status, body := term.Attach(t, secondToken, secondSession, "", nil); status != http.StatusOK {
		t.Fatalf("attach second = %d %v, want 200", status, body)
	}
	term.Audit.SetDown(true)
	status, body := term.Input(t, secondToken, secondSession, 1, "echo blocked\n")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("input with a down trail = %d %v, want 503", status, body)
	}
	if code := harness.FailureCode(t, body); code != controlplane.CodeTerminalAuditUnavailable {
		t.Fatalf("input with a down trail code = %s, want %s", code, controlplane.CodeTerminalAuditUnavailable)
	}
	if got := len(term.Executor.Batches(second.UID)); got != 0 {
		t.Fatalf("a down trail delivered %d batches, want none", got)
	}
	status, read = term.Status(t, owner, secondSession)
	if status != http.StatusOK {
		t.Fatalf("status after trail failure = %d %v, want 200", status, read)
	}
	if got := read["state"]; got != controlplane.TerminalStateFailed {
		t.Fatalf("state after trail failure = %v, want failed", got)
	}
	if status, body = term.Input(t, secondToken, secondSession, 2, "echo blocked\n"); status != http.StatusUnauthorized {
		t.Fatalf("input after the session ended = %d %v, want 401", status, body)
	}

	third := term.PodTarget(t, "third", "root")
	status, body = term.Create(t, owner, harness.TerminalExtension, third)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("create with a down trail = %d %v, want 503", status, body)
	}
	term.Audit.SetDown(false)
	status, created = term.Create(t, owner, harness.TerminalExtension, third)
	if status != http.StatusCreated {
		t.Fatalf("create after the trail returned = %d %v, want 201 instead of reusing a session", status, created)
	}
}

// failWindowError blocks inside errors.Is, after Send has failed and before the
// handler closes the session, so a case can submit the same sequence exactly in
// that failure window.
type failWindowError struct {
	err   error
	fired atomic.Bool
	onIs  func()
}

func (e *failWindowError) Error() string { return "scripted send failure" }

func (e *failWindowError) Is(target error) bool {
	if e.fired.CompareAndSwap(false, true) {
		e.onIs()
	}
	return errors.Is(e.err, target)
}

func itemsOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("audit answer carries no items: %v", body)
	}
	return items
}

func resultOf(event map[string]any) string {
	result, _ := event["result"].(string)
	return result
}

func stringField(t *testing.T, body map[string]any, name string) string {
	t.Helper()
	value, _ := body[name].(string)
	if value == "" {
		t.Fatalf("%s is missing from %v", name, body)
	}
	return value
}
