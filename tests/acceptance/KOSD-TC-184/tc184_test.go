// Package kosdtc184 verifies terminal authorization and the failure envelope
// of KOSD-AC-84, KOSD-TC-184: authorization runs before any runtime error,
// unauthorized and absent targets answer identically without target content,
// login failure is 401, an ended target is 409, a limit is 429, service and
// credential-boundary failures are 503, and a BFF-forwarded request answers
// like a direct one.
package kosdtc184

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/install"
	terminalpkg "github.com/tsic404/kosd/pkg/terminal"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

var forwarded = map[string]string{
	"X-Forwarded-Host": "console.example",
	"X-Forwarded-For":  "10.0.0.8",
}

const sensitiveDefinitionTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: "acceptance"
`

// TestTC184ProductionCredentialBoundary exercises the production judge rather
// than the fixture switch: the control-plane's own Pod and an application that
// declares a sensitive parameter are refused with the credential-boundary code,
// while a Pod carrying neither is admitted (ADR-043, KOSD-C-93).
func TestTC184ProductionCredentialBoundary(t *testing.T) {
	cluster := harness.Start(t)
	term := cluster.StartTerminal(t, harness.TerminalOptions{
		Extensions:    []string{harness.TerminalExtension},
		BoundaryJudge: terminalpkg.Boundary{},
	})
	owner := term.OwnerToken(t)

	controlPlane := cluster.CreatePod(t, harness.PodFixture{
		Name:           "control-plane-0",
		Labels:         map[string]string{"app": install.ControlPlaneName},
		ServiceAccount: install.ControlPlaneServiceAccount,
		Ready:          true,
	})
	expectBoundaryRefusal(t, term, owner, podTargetOf(controlPlane), "control-plane Pod")

	cluster.InstallDefinition(t, harness.DefinitionContent(t, "sensitive-def", "", sensitiveDefinitionTemplate, nil,
		[]v1beta1.ConfigField{{Name: "password", Type: "string", Sensitive: true}}))
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: harness.RuntimeApplication(
		"sensitive", map[string]any{"owner": "root", "definitionRef": "sensitive-def"}, "sensitive")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, harness.RuntimeDeployment("sensitive", 1))
	sensitive := cluster.CreatePod(t, harness.PodFixture{
		Name: "sensitive-0", Labels: map[string]string{"app": "sensitive"}, Ready: true,
	})
	expectBoundaryRefusal(t, term, owner, podTargetOf(sensitive), "sensitive application Pod")

	tokenPod := cluster.CreatePod(t, harness.PodFixture{
		Name: "projected-token-0", Ready: true, ServiceAccount: "default",
		Volumes: []any{map[string]any{
			"name": "kube-api-access",
			"projected": map[string]any{"sources": []any{
				map[string]any{"serviceAccountToken": map[string]any{"path": "token"}},
			}},
		}},
		Mounts: []any{map[string]any{
			"name":      "kube-api-access",
			"mountPath": "/var/run/secrets/kubernetes.io/serviceaccount",
			"readOnly":  true,
		}},
	})
	expectBoundaryRefusal(t, term, owner, podTargetOf(tokenPod), "service account token Pod")

	plain := cluster.CreatePod(t, harness.PodFixture{Name: "plain-0", Ready: true})
	status, created := term.Create(t, owner, harness.TerminalExtension, podTargetOf(plain))
	if status != http.StatusCreated {
		t.Fatalf("plain Pod create = %d %v, want 201", status, created)
	}
	if status, body := term.End(t, owner, stringField(t, created, "sessionId")); status != http.StatusOK {
		t.Fatalf("end plain session = %d %v, want 200", status, body)
	}

	scratch := cluster.CreatePod(t, harness.PodFixture{
		Name: "scratch-0", Ready: true,
		Volumes: []any{map[string]any{"name": "scratch", "emptyDir": map[string]any{}}},
		Mounts:  []any{map[string]any{"name": "scratch", "mountPath": "/scratch"}},
	})
	status, created = term.Create(t, owner, harness.TerminalExtension, podTargetOf(scratch))
	if status != http.StatusCreated {
		t.Fatalf("scratch volume Pod create = %d %v, want 201", status, created)
	}
	if status, body := term.End(t, owner, stringField(t, created, "sessionId")); status != http.StatusOK {
		t.Fatalf("end scratch volume session = %d %v, want 200", status, body)
	}
}

func expectBoundaryRefusal(t *testing.T, term *harness.Terminal, owner string, target controlplane.TerminalTarget, name string) {
	t.Helper()
	status, body := term.Create(t, owner, harness.TerminalExtension, target)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("%s create = %d %v, want 503", name, status, body)
	}
	if code := harness.FailureCode(t, body); code != controlplane.CodeTerminalCredentialBoundaryUnavailable {
		t.Fatalf("%s code = %s, want %s", name, code, controlplane.CodeTerminalCredentialBoundaryUnavailable)
	}
}

func podTargetOf(pod *unstructured.Unstructured) controlplane.TerminalTarget {
	return controlplane.TerminalTarget{
		Type: controlplane.TerminalTargetTypePod, ID: pod.GetName(), UID: string(pod.GetUID()),
		Namespace: pod.GetNamespace(), Container: "app",
	}
}

func TestTC184AuthorizationAndFailureClassification(t *testing.T) {
	cluster := harness.Start(t)
	term := cluster.StartTerminal(t, harness.TerminalOptions{
		Extensions: []string{
			harness.TerminalExtension, "ext-a", "ext-b", "ext-c", "ext-d",
		},
	})
	owner := term.OwnerToken(t)
	guest := term.AccountToken(t, "guest", "Guest")
	alice := term.AccountToken(t, "alice", "Member")
	bob := term.AccountToken(t, "bob", "Member")

	pod := term.PodTarget(t, "shop", "bob")

	// The account gate answers a missing or forged console session in the
	// terminal vocabulary (KOSD-C-91).
	expectFailure(t, term, "", pod, http.StatusUnauthorized, controlplane.CodeTerminalUnauthenticated, pod)
	expectFailure(t, term, "forged-token", pod, http.StatusUnauthorized, controlplane.CodeTerminalUnauthenticated, pod)

	// No target permission and an absent target share one answer.
	_, unauthorized := expectFailure(t, term, guest, pod, http.StatusForbidden, controlplane.CodeTerminalTargetUnavailable, pod)
	expectFailure(t, term, alice, pod, http.StatusForbidden, controlplane.CodeTerminalTargetUnavailable, pod)
	ghost := controlplane.TerminalTarget{
		Type: controlplane.TerminalTargetTypePod, ID: "ghost", UID: "ghost-uid",
		Namespace: pod.Namespace, Container: "app",
	}
	_, absent := expectFailure(t, term, owner, ghost, http.StatusForbidden, controlplane.CodeTerminalTargetUnavailable, pod)
	if !reflect.DeepEqual(unauthorized["error"], absent["error"]) {
		t.Fatalf("unauthorized %v and absent %v answers differ", unauthorized["error"], absent["error"])
	}

	// A Member owning the application enters its Pod; the Owner enters every
	// Pod and reaches a node as root@node.
	status, created := term.Create(t, bob, harness.TerminalExtension, pod)
	if status != http.StatusCreated {
		t.Fatalf("bob creates the Pod session = %d %v, want 201", status, created)
	}
	bobSession := stringField(t, created, "sessionId")
	bobToken := stringField(t, created, "token")
	if status, body := term.Attach(t, bobToken, bobSession, "", nil); status != http.StatusOK {
		t.Fatalf("bob attach = %d %v, want 200", status, body)
	}
	if status, body := term.Input(t, bobToken, bobSession, 1, "id\n"); status != http.StatusOK {
		t.Fatalf("bob input = %d %v, want 200", status, body)
	}
	if status, body := term.End(t, bob, bobSession); status != http.StatusOK {
		t.Fatalf("bob end = %d %v, want 200", status, body)
	}

	node := term.NodeTarget(t, "node-up")
	status, createdNode := term.Create(t, owner, harness.TerminalExtension, node)
	if status != http.StatusCreated {
		t.Fatalf("Owner creates the node session = %d %v, want 201", status, createdNode)
	}
	if got := createdNode["executionIdentity"]; got != controlplane.TerminalNodeIdentity {
		t.Fatalf("node execution identity = %v, want %s", got, controlplane.TerminalNodeIdentity)
	}
	ownerNodeSession := stringField(t, createdNode, "sessionId")
	if status, body := term.End(t, owner, ownerNodeSession); status != http.StatusOK {
		t.Fatalf("Owner end node = %d %v, want 200", status, body)
	}
	expectFailure(t, term, alice, node, http.StatusForbidden, controlplane.CodeTerminalTargetUnavailable, node)
	expectFailure(t, term, guest, node, http.StatusForbidden, controlplane.CodeTerminalTargetUnavailable, node)

	// Authorization runs first: an ended target stays target-unavailable for a
	// caller without permission and is a conflict only for an authorized one.
	ended := term.PodTargetPhase(t, "done", "bob", "Succeeded")
	expectFailure(t, term, alice, ended, http.StatusForbidden, controlplane.CodeTerminalTargetUnavailable, ended)
	expectFailure(t, term, bob, ended, http.StatusConflict, controlplane.CodeTerminalTargetEnded, ended)
	expectFailure(t, term, owner, ended, http.StatusConflict, controlplane.CodeTerminalTargetEnded, ended)

	// An authorized but unreachable node is a runtime failure.
	down := term.Cluster.CreateNodeWith(t, "node-down", false)
	downTarget := controlplane.TerminalTarget{
		Type: controlplane.TerminalTargetTypeNode, ID: down.GetName(), UID: string(down.GetUID()),
	}
	expectFailure(t, term, owner, downTarget, http.StatusServiceUnavailable, controlplane.CodeTerminalNodeUnreachable, downTarget)

	// An unavailable executor and an unisolable credential boundary are 503.
	dormant := term.PodTarget(t, "dormant", "root")
	term.Executor.SetAvailable(false)
	expectFailure(t, term, owner, dormant, http.StatusServiceUnavailable, controlplane.CodeTerminalExecutorUnavailable, dormant)
	term.Executor.SetAvailable(true)
	term.Boundary.RefusePod(true)
	expectFailure(t, term, owner, dormant, http.StatusServiceUnavailable, controlplane.CodeTerminalCredentialBoundaryUnavailable, dormant)
	term.Boundary.RefusePod(false)

	// Four live sessions fill both bounds; the fifth is 429 and never evicts
	// an existing session.
	sessions := make([]string, 0, 4)
	tokens := make([]string, 0, 4)
	for _, extension := range []string{harness.TerminalExtension, "ext-a", "ext-b", "ext-c"} {
		status, body := term.Create(t, owner, extension, pod)
		if status != http.StatusCreated {
			t.Fatalf("fill session %s = %d %v, want 201", extension, status, body)
		}
		sessions = append(sessions, stringField(t, body, "sessionId"))
		tokens = append(tokens, stringField(t, body, "token"))
	}
	expectFailureExtension(t, term, owner, pod, "ext-d", http.StatusTooManyRequests, controlplane.CodeTerminalSessionLimit, pod)
	status, read := term.Status(t, owner, sessions[0])
	if status != http.StatusOK {
		t.Fatalf("status of an old session = %d %v, want 200", status, read)
	}
	if status, body := term.Attach(t, tokens[0], sessions[0], "", nil); status != http.StatusOK {
		t.Fatalf("an over-limit create evicted an old session: %d %v", status, body)
	}
	if status, body := term.End(t, owner, sessions[0]); status != http.StatusOK {
		t.Fatalf("release = %d %v, want 200", status, body)
	}
	status, replaced := term.Create(t, owner, "ext-d", pod)
	if status != http.StatusCreated {
		t.Fatalf("create after release = %d %v, want 201", status, replaced)
	}
}

// expectFailure sends one create directly and once as a BFF-forwarded request,
// requiring the same status and code, no target field, and no delivery.
func expectFailure(t *testing.T, term *harness.Terminal, token string, target controlplane.TerminalTarget, wantStatus int, wantCode string, forbidden controlplane.TerminalTarget) (map[string]any, map[string]any) {
	t.Helper()
	return expectFailureExtension(t, term, token, target, harness.TerminalExtension, wantStatus, wantCode, forbidden)
}

func expectFailureExtension(t *testing.T, term *harness.Terminal, token string, target controlplane.TerminalTarget, extension string, wantStatus int, wantCode string, forbidden controlplane.TerminalTarget) (map[string]any, map[string]any) {
	t.Helper()
	before := len(term.Executor.Batches(target.UID))
	status, direct := term.Create(t, token, extension, target)
	if status != wantStatus {
		t.Fatalf("direct create %s = %d %v, want %d", target.ID, status, direct, wantStatus)
	}
	if code := harness.FailureCode(t, direct); code != wantCode {
		t.Fatalf("direct create %s code = %s, want %s", target.ID, code, wantCode)
	}
	status, bff := term.CreateWith(t, token, extension, target, forwarded)
	if status != wantStatus {
		t.Fatalf("BFF create %s = %d %v, want %d", target.ID, status, bff, wantStatus)
	}
	if code := harness.FailureCode(t, bff); code != wantCode {
		t.Fatalf("BFF create %s code = %s, want %s", target.ID, code, wantCode)
	}
	for name, body := range map[string]map[string]any{"direct": direct, "BFF": bff} {
		text := describe(body)
		for _, forbiddenText := range []string{forbidden.ID, forbidden.UID} {
			if forbiddenText != "" && strings.Contains(text, forbiddenText) {
				t.Fatalf("%s failure leaks target %q: %s", name, forbiddenText, text)
			}
		}
	}
	if after := len(term.Executor.Batches(target.UID)); after != before {
		t.Fatalf("a refused create delivered input: %d batches, want %d", after, before)
	}
	return direct, bff
}

func describe(body map[string]any) string {
	encoded, err := json.Marshal(body)
	if err != nil {
		return harness.Describe(body)
	}
	return string(encoded)
}

func stringField(t *testing.T, body map[string]any, name string) string {
	t.Helper()
	value, _ := body[name].(string)
	if value == "" {
		t.Fatalf("%s is missing from %v", name, body)
	}
	return value
}
