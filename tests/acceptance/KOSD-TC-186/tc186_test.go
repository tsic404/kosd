// Package kosdtc186 verifies the terminal session bounds and idle setting of
// KOSD-AC-85, KOSD-TC-186: four account and four target slots, concurrent
// creation cannot pass them, a repeated create takes no slot, releasing frees
// one, and the cluster idle setting only shapes new sessions while renewal,
// duplicate input and output leave the idle clock alone.
package kosdtc186

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

var extensions = []string{
	harness.TerminalExtension, "ext-a", "ext-b", "ext-c", "ext-d",
	"ext-e", "ext-f", "ext-g", "ext-h",
}

func TestTC186SessionBoundsAndIdleSetting(t *testing.T) {
	cluster := harness.Start(t)
	term := cluster.StartTerminal(t, harness.TerminalOptions{Extensions: extensions})
	owner := term.OwnerToken(t)

	// Four live sessions fill both bounds; the fifth is refused and does not
	// evict an existing one.
	pod := term.PodTarget(t, "bounded", "root")
	first := create(t, term, owner, harness.TerminalExtension, pod)
	created := []session{
		first,
		create(t, term, owner, "ext-a", pod),
		create(t, term, owner, "ext-b", pod),
		create(t, term, owner, "ext-c", pod),
	}
	assertUsage(t, term, owner, first.id, 4, 4)
	status, body := term.Create(t, owner, "ext-d", pod)
	if status != http.StatusTooManyRequests {
		t.Fatalf("fifth session = %d %v, want 429", status, body)
	}
	if code := harness.FailureCode(t, body); code != controlplane.CodeTerminalSessionLimit {
		t.Fatalf("fifth session code = %s, want %s", code, controlplane.CodeTerminalSessionLimit)
	}
	status, repeated := term.Create(t, owner, "ext-a", pod)
	if status != http.StatusOK {
		t.Fatalf("repeat create = %d %v, want 200", status, repeated)
	}
	if got := stringField(t, repeated, "sessionId"); got != created[1].id {
		t.Fatalf("repeat create session = %s, want %s", got, created[1].id)
	}
	if _, found := repeated["token"]; found {
		t.Fatalf("repeat create delivered another token: %v", repeated)
	}
	assertUsage(t, term, owner, first.id, 4, 4)
	if status, body := term.Status(t, owner, first.id); status != http.StatusOK {
		t.Fatalf("an old session disappeared: %d %v", status, body)
	}
	if status, body := term.End(t, owner, first.id); status != http.StatusOK {
		t.Fatalf("release = %d %v, want 200", status, body)
	}
	replacement := create(t, term, owner, "ext-d", pod)
	assertUsage(t, term, owner, replacement.id, 4, 4)
	endAll(t, term, owner, append(created[1:], replacement))

	// Concurrent creates cannot pass the same bounds.
	results := concurrentCreates(t, term, owner, pod, extensions)
	if results.counts[201] != 4 || results.counts[429] != 5 {
		t.Fatalf("concurrent creates = %v, want four 201 and five 429", results.counts)
	}
	if len(results.sessions) != 4 {
		t.Fatalf("concurrent sessions = %d, want 4", len(results.sessions))
	}
	assertUsage(t, term, owner, results.sessions[0], 4, 4)
	endAll(t, term, owner, nil, results.sessions...)

	// The per-target bound counts one Pod across containers, and a second
	// account cannot spend a fifth target slot.
	targets := term.PodTargets(t, "multi", "root", "app", "sidecar", "helper", "debug")
	members := make([]string, 0, 5)
	for index := 1; index <= 5; index++ {
		members = append(members, term.AccountToken(t, "member-"+string(rune('a'+index-1)), "Member"))
	}
	memberSessions := make([]session, 0, 4)
	for index := 0; index < 4; index++ {
		term.TransferApplication(t, "multi", "member-"+string(rune('a'+index)))
		memberSessions = append(memberSessions, create(t, term, members[index], harness.TerminalExtension, targets[index]))
		assertUsage(t, term, members[index], memberSessions[index].id, 1, index+1)
	}
	term.TransferApplication(t, "multi", "member-e")
	status, body = term.Create(t, members[4], harness.TerminalExtension, targets[0])
	if status != http.StatusTooManyRequests {
		t.Fatalf("fifth target session = %d %v, want 429", status, body)
	}
	if status, body := term.Status(t, members[0], memberSessions[0].id); status != http.StatusOK {
		t.Fatalf("the target limit evicted an old session: %d %v", status, body)
	}
	if status, body := term.End(t, members[0], memberSessions[0].id); status != http.StatusOK {
		t.Fatalf("target release = %d %v, want 200", status, body)
	}
	released := create(t, term, members[4], harness.TerminalExtension, targets[0])
	_ = released
	for index := 1; index < len(memberSessions); index++ {
		if status, body := term.End(t, members[index], memberSessions[index].id); status != http.StatusOK {
			t.Fatalf("end member session = %d %v, want 200", status, body)
		}
	}
	if status, body := term.End(t, members[4], released.id); status != http.StatusOK {
		t.Fatalf("end released session = %d %v, want 200", status, body)
	}

	// The idle setting defaults to ten, changes only new sessions, rejects
	// out-of-range and non-integer values, and only the global Owner writes it.
	idlePod := term.PodTarget(t, "idle", "root")
	old := createSession(t, term, owner, harness.TerminalExtension, idlePod)
	status, body = term.SetIdle(t, owner, "1")
	if status != http.StatusOK {
		t.Fatalf("set idle = %d %v, want 200", status, body)
	}
	fresh := createSession(t, term, owner, "ext-a", idlePod)
	status, oldView := term.Status(t, owner, old.id)
	if status != http.StatusOK {
		t.Fatalf("old status = %d %v, want 200", status, oldView)
	}
	if got := oldView["idleTimeoutMinutes"]; got != float64(controlplane.TerminalDefaultIdleMinutes) {
		t.Fatalf("old idle = %v, want %d", got, controlplane.TerminalDefaultIdleMinutes)
	}
	if got := oldView["state"]; got != controlplane.TerminalStateConnected {
		t.Fatalf("old state = %v, want connected", got)
	}
	status, freshView := term.Status(t, owner, fresh.id)
	if status != http.StatusOK {
		t.Fatalf("new status = %d %v, want 200", status, freshView)
	}
	if got := freshView["idleTimeoutMinutes"]; got != float64(1) {
		t.Fatalf("new idle = %v, want 1", got)
	}

	// Renewal and a duplicate sequence do not restart the idle clock: the last
	// accepted batch is what counts.
	term.Clock.Advance(10 * time.Second)
	status, renewed := term.Renew(t, owner, fresh.id)
	if status != http.StatusOK {
		t.Fatalf("renew = %d %v, want 200", status, renewed)
	}
	freshToken := stringField(t, renewed, "token")
	term.Clock.Advance(20 * time.Second)
	if status, body := term.Input(t, freshToken, fresh.id, 1, "echo still here\n"); status != http.StatusOK {
		t.Fatalf("input = %d %v, want 200", status, body)
	}
	term.Clock.Advance(20 * time.Second)
	status, duplicate := term.Input(t, freshToken, fresh.id, 1, "echo still here\n")
	if status != http.StatusOK {
		t.Fatalf("duplicate input = %d %v, want 200", status, duplicate)
	}
	if got := duplicate["duplicate"]; got != true {
		t.Fatalf("duplicate answer = %v, want duplicate", duplicate)
	}
	if status, body := term.Status(t, owner, fresh.id); status != http.StatusOK || body["state"] != controlplane.TerminalStateConnected {
		t.Fatalf("before the idle bound = %d %v, want connected", status, body)
	}
	term.Clock.Advance(41 * time.Second)
	status, freshView = term.Status(t, owner, fresh.id)
	if status != http.StatusOK {
		t.Fatalf("idle status = %d %v, want 200", status, freshView)
	}
	if got := freshView["state"]; got != controlplane.TerminalStateClosed {
		t.Fatalf("idle state = %v, want closed", got)
	}
	if got := freshView["reason"]; got != controlplane.TerminalReasonIdle {
		t.Fatalf("idle reason = %v, want %s", got, controlplane.TerminalReasonIdle)
	}
	if status, body := term.Input(t, freshToken, fresh.id, 2, "echo late\n"); status != http.StatusUnauthorized {
		t.Fatalf("input after idle = %d %v, want 401", status, body)
	}
	status, oldView = term.Status(t, owner, old.id)
	if status != http.StatusOK || oldView["state"] != controlplane.TerminalStateConnected {
		t.Fatalf("the old session did not keep its ten-minute bound: %d %v", status, oldView)
	}

	member := members[0]
	for _, value := range []string{"0", "61", "1.5", "abc"} {
		status, body = term.SetIdle(t, owner, value)
		if status != http.StatusBadRequest {
			t.Fatalf("set idle %q = %d %v, want 400", value, status, body)
		}
	}
	if status, body = term.SetIdle(t, member, "5"); status != http.StatusForbidden {
		t.Fatalf("member set idle = %d %v, want 403", status, body)
	}
	if status, body = term.SetIdle(t, owner, "1"); status != http.StatusOK {
		t.Fatalf("idle value after refused writes = %d %v, want 200", status, body)
	}
}

type session struct {
	id    string
	token string
}

func create(t *testing.T, term *harness.Terminal, token, extension string, target controlplane.TerminalTarget) session {
	t.Helper()
	status, body := term.Create(t, token, extension, target)
	if status != http.StatusCreated {
		t.Fatalf("create %s = %d %v, want 201", extension, status, body)
	}
	return session{id: stringField(t, body, "sessionId"), token: stringField(t, body, "token")}
}

func createSession(t *testing.T, term *harness.Terminal, token, extension string, target controlplane.TerminalTarget) session {
	t.Helper()
	created := create(t, term, token, extension, target)
	if status, body := term.Attach(t, created.token, created.id, "", nil); status != http.StatusOK {
		t.Fatalf("attach %s = %d %v, want 200", extension, status, body)
	}
	return created
}

func endAll(t *testing.T, term *harness.Terminal, token string, extra []session, ids ...string) {
	t.Helper()
	for _, item := range extra {
		ids = append(ids, item.id)
	}
	for _, id := range ids {
		if status, body := term.End(t, token, id); status != http.StatusOK {
			t.Fatalf("end %s = %d %v, want 200", id, status, body)
		}
	}
}

func assertUsage(t *testing.T, term *harness.Terminal, token, session string, account, target int) {
	t.Helper()
	status, body := term.Status(t, token, session)
	if status != http.StatusOK {
		t.Fatalf("usage status = %d %v, want 200", status, body)
	}
	usage, _ := body["usage"].(map[string]any)
	if got := usage["account"]; got != float64(account) {
		t.Fatalf("usage.account = %v, want %d", got, account)
	}
	if got := usage["target"]; got != float64(target) {
		t.Fatalf("usage.target = %v, want %d", got, target)
	}
}

type createResults struct {
	mu       sync.Mutex
	counts   map[int]int
	sessions []string
}

func concurrentCreates(t *testing.T, term *harness.Terminal, token string, target controlplane.TerminalTarget, extensions []string) *createResults {
	t.Helper()
	results := &createResults{counts: map[int]int{}}
	var wait sync.WaitGroup
	for _, extension := range extensions {
		wait.Add(1)
		go func(extension string) {
			defer wait.Done()
			body := map[string]any{
				"extension":  extension,
				"targetType": target.Type,
				"targetId":   target.ID,
				"targetUid":  target.UID,
				"namespace":  target.Namespace,
				"container":  target.Container,
			}
			status, payload, err := term.HTTP.Raw(http.MethodPost, "/api/v1/terminal/sessions", body, map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"})
			results.mu.Lock()
			defer results.mu.Unlock()
			if err != nil {
				return
			}
			results.counts[status]++
			if status == http.StatusCreated {
				var decoded map[string]any
				if json.Unmarshal(payload, &decoded) == nil {
					if id, _ := decoded["sessionId"].(string); id != "" {
						results.sessions = append(results.sessions, id)
					}
				}
			}
		}(extension)
	}
	wait.Wait()
	return results
}

func stringField(t *testing.T, body map[string]any, name string) string {
	t.Helper()
	value, _ := body[name].(string)
	if value == "" {
		t.Fatalf("%s is missing from %v", name, body)
	}
	return value
}
