package harness

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/pkg/access"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/settings"
)

// TerminalExtension is the component identifier the acceptance cases act for.
const TerminalExtension = "kos-console"

// errTerminalAuditDown is the failure the audit switch reports.
var errTerminalAuditDown = errors.New("terminal audit is down")

// TerminalOptions shapes the composed terminal surface.
type TerminalOptions struct {
	// Extensions are the component identifiers reported active.
	Extensions []string
	// Source replaces the live cluster source, so a case can make the target
	// source stop answering (KOSD-C-95).
	Source controlplane.ObjectSource
	// Executor replaces the scripted executor when set.
	Executor *ScriptedExecutor
	// Boundary replaces the default accepting boundary when set.
	Boundary *TerminalBoundary
	// BoundaryJudge replaces the boundary with any judge; it wins over Boundary
	// when both are set, so a case can exercise the production judge.
	BoundaryJudge controlplane.CredentialBoundary
	// Audit replaces the database-backed audit when set.
	Audit *TerminalAudit
	// WithoutOwner skips the bootstrap; the case logs in itself.
	WithoutOwner bool
}

// Terminal is a running control plane with the account, settings and terminal
// surfaces composed in the deployment's order: the token-only routes ahead of
// the console gate and everything else behind it.
type Terminal struct {
	Cluster    *Cluster
	Identity   *Identity
	Server     *controlplane.Server
	HTTP       *HTTP
	Clock      *Clock
	Audit      *TerminalAudit
	Executor   *ScriptedExecutor
	Boundary   *TerminalBoundary
	Extensions *ActiveExtensions

	ownerUsername string
	ownerToken    string
}

// StartTerminal boots the composed surface over the acceptance cluster.
func (c *Cluster) StartTerminal(t *testing.T, options TerminalOptions) *Terminal {
	t.Helper()
	accounts := OpenIdentity(t, nil, nil)
	clock := accounts.Clock
	executor := options.Executor
	if executor == nil {
		executor = NewScriptedExecutor()
	}
	boundary := options.Boundary
	if boundary == nil {
		boundary = &TerminalBoundary{}
	}
	var judge controlplane.CredentialBoundary = boundary
	if options.BoundaryJudge != nil {
		judge = options.BoundaryJudge
	}
	extensions := NewActiveExtensions(options.Extensions)
	audit := options.Audit
	if audit == nil {
		audit = &TerminalAudit{Trail: identity.TerminalAuditTrail{Store: accounts.Store}}
	}
	server, err := controlplane.New(controlplane.Config{
		Dynamic: c.Dynamic,
		Source:  options.Source,
		Now:     clock.Now,
		Authorizer: identity.Authorizer{Policy: &access.Policy{
			Objects: controlplane.NewClusterSource(c.Dynamic),
		}},
		Terminal: controlplane.TerminalConfig{
			Console:     identity.TerminalSessions{Service: accounts.Service, Store: accounts.Store},
			Audit:       audit,
			Extensions:  extensions,
			Executor:    executor,
			Boundary:    judge,
			IdleTimeout: terminalIdleReader(accounts.Store),
		},
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	t.Cleanup(server.Close)

	settingsAPI := settings.NewAPI(settings.New(TerminalDeclarations{}, accounts.Store, settings.Config{
		Auditor: accounts.Trail,
	})).Handler()
	public := server.TerminalPublicHandler()
	gated := http.NewServeMux()
	gated.Handle("/api/v1/settings", settingsAPI)
	gated.Handle("/api/v1/settings/", settingsAPI)
	gated.Handle("/", server.Handler())
	root := http.NewServeMux()
	root.Handle("POST /api/v1/terminal/sessions/{sessionId}/attach", public)
	root.Handle("POST /api/v1/terminal/sessions/{sessionId}/input", public)
	root.Handle("/", accounts.API.Handler(gated))
	httpServer := httptest.NewServer(root)
	t.Cleanup(httpServer.Close)
	accounts.HTTP = &HTTP{URL: httpServer.URL}

	terminal := &Terminal{
		Cluster:    c,
		Identity:   accounts,
		Server:     server,
		HTTP:       accounts.HTTP,
		Clock:      clock,
		Audit:      audit,
		Executor:   executor,
		Boundary:   boundary,
		Extensions: extensions,
	}
	if !options.WithoutOwner {
		terminal.ownerUsername, terminal.ownerToken = terminal.StartOwner(t, "root")
	}
	return terminal
}

// terminalIdleReader reads the cluster-scoped terminal idle setting.
func terminalIdleReader(store *identity.Store) func(context.Context) (int, *controlplane.Error) {
	return func(ctx context.Context) (int, *controlplane.Error) {
		values, err := store.SettingValues(ctx, extension.SettingScopeCluster, "")
		if err != nil {
			return 0, &controlplane.Error{
				Code:        controlplane.CodeDatabaseUnavailable,
				Message:     "控制面数据库不可用",
				Stage:       controlplane.StageRead,
				Retryable:   true,
				SideEffects: controlplane.SideEffectsNone,
			}
		}
		minutes, ok := controlplane.TerminalIdleMinutes(values[controlplane.TerminalIdleSettingID])
		if !ok {
			return 0, &controlplane.Error{
				Code:        controlplane.CodeInternal,
				Message:     "已存的终端空闲配置无效",
				Stage:       controlplane.StageRead,
				SideEffects: controlplane.SideEffectsNone,
			}
		}
		return minutes, nil
	}
}

// TerminalDeclarations contributes the terminal setting the way the terminal
// component's package does (KOSD-C-94).
type TerminalDeclarations struct{}

// Settings implements settings.Declarations.
func (TerminalDeclarations) Settings() []extension.Setting {
	return []extension.Setting{{
		ID:       controlplane.TerminalIdleSettingID,
		Scope:    extension.SettingScopeCluster,
		Writer:   extension.SettingWriterUser,
		Editable: true,
	}}
}

// Setting implements settings.Declarations.
func (d TerminalDeclarations) Setting(id string) (extension.Setting, bool) {
	for _, setting := range d.Settings() {
		if setting.ID == id {
			return setting, true
		}
	}
	return extension.Setting{}, false
}

// ActiveExtensions is the terminal surface's activity projection.
type ActiveExtensions struct {
	mu     sync.RWMutex
	active map[string]bool
}

// NewActiveExtensions starts with the named identifiers active.
func NewActiveExtensions(identifiers []string) *ActiveExtensions {
	extensions := &ActiveExtensions{active: map[string]bool{}}
	for _, identifier := range identifiers {
		extensions.active[identifier] = true
	}
	return extensions
}

// IsActive implements controlplane.TerminalExtensions.
func (e *ActiveExtensions) IsActive(identifier string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.active[identifier]
}

// Set changes one identifier's activity.
func (e *ActiveExtensions) Set(identifier string, active bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if active {
		e.active[identifier] = true
		return
	}
	delete(e.active, identifier)
}

// TerminalAudit is the database trail with a failure switch, so a case can
// make the trail stop answering (KOSD-C-92).
type TerminalAudit struct {
	// Trail is the real database-backed store.
	Trail identity.TerminalAuditTrail
	// IsDown makes every append and read fail.
	IsDown bool
	mu     sync.Mutex
}

// Append implements controlplane.TerminalAudit.
func (a *TerminalAudit) Append(ctx context.Context, event controlplane.TerminalEvent) error {
	a.mu.Lock()
	down := a.IsDown
	a.mu.Unlock()
	if down {
		return errTerminalAuditDown
	}
	return a.Trail.Append(ctx, event)
}

// Read implements controlplane.TerminalAudit.
func (a *TerminalAudit) Read(ctx context.Context, sessionID string, after int64, limit int) ([]controlplane.TerminalEvent, int64, error) {
	a.mu.Lock()
	down := a.IsDown
	a.mu.Unlock()
	if down {
		return nil, 0, errTerminalAuditDown
	}
	return a.Trail.Read(ctx, sessionID, after, limit)
}

// Available implements controlplane.TerminalAudit.
func (a *TerminalAudit) Available(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.IsDown {
		return errTerminalAuditDown
	}
	return nil
}

// SetDown changes whether the trail answers.
func (a *TerminalAudit) SetDown(down bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.IsDown = down
}

// ToggleSource wraps an object source with a failure switch, so a case can
// make target reads stop answering without stopping the cluster (KOSD-C-95).
type ToggleSource struct {
	// Source is the source that answers while the switch is up.
	Source controlplane.ObjectSource
	mu     sync.Mutex
	down   bool
}

// SetDown changes whether the source answers.
func (s *ToggleSource) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// List implements controlplane.ObjectSource.
func (s *ToggleSource) List(ctx context.Context, objectType controlplane.ObjectType, namespace string) ([]controlplane.ObservedObject, error) {
	if s.isDown() {
		return nil, errTerminalAuditDown
	}
	return s.Source.List(ctx, objectType, namespace)
}

// Get implements controlplane.ObjectSource.
func (s *ToggleSource) Get(ctx context.Context, objectType controlplane.ObjectType, namespace, name string) (controlplane.ObservedObject, error) {
	if s.isDown() {
		return controlplane.ObservedObject{}, errTerminalAuditDown
	}
	return s.Source.Get(ctx, objectType, namespace, name)
}

func (s *ToggleSource) isDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.down
}

// TerminalBoundary is the credential-isolation judge of the cases.
type TerminalBoundary struct {
	mu         sync.Mutex
	refusePod  bool
	refuseNode bool
}

// RefusePod changes whether Pod sessions are refused.
func (b *TerminalBoundary) RefusePod(refuse bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refusePod = refuse
}

// RefuseNode changes whether node sessions are refused.
func (b *TerminalBoundary) RefuseNode(refuse bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refuseNode = refuse
}

// Judge implements controlplane.CredentialBoundary.
func (b *TerminalBoundary) Judge(_ context.Context, observed controlplane.TerminalBoundaryTarget) *controlplane.Error {
	b.mu.Lock()
	refuse := b.refusePod
	if observed.Target.Type == controlplane.TerminalTargetTypeNode {
		refuse = b.refuseNode
	}
	b.mu.Unlock()
	if !refuse {
		return nil
	}
	return &controlplane.Error{
		Code:        controlplane.CodeTerminalCredentialBoundaryUnavailable,
		Message:     "无法保证凭据隔离",
		Stage:       controlplane.StageConnect,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// ScriptedExecutor records what each target received and answers with a
// declared output. It never executes a process, so a case measures delivery
// rather than a shell.
type ScriptedExecutor struct {
	mu          sync.Mutex
	isAvailable bool
	batches     map[string][][]byte
	opens       map[string]int
	closes      map[string]int
	failOpen    map[string]error
	sends       map[string]int
	failSend    map[string]error
	output      string
	gate        chan struct{}
}

// NewScriptedExecutor starts available with no inputs.
func NewScriptedExecutor() *ScriptedExecutor {
	return &ScriptedExecutor{
		isAvailable: true,
		batches:     map[string][][]byte{},
		opens:       map[string]int{},
		closes:      map[string]int{},
		failOpen:    map[string]error{},
		sends:       map[string]int{},
		failSend:    map[string]error{},
	}
}

// Available implements controlplane.TerminalExecutor.
func (e *ScriptedExecutor) Available(context.Context) *controlplane.Error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.isAvailable {
		return nil
	}
	return &controlplane.Error{
		Code:        controlplane.CodeTerminalExecutorUnavailable,
		Message:     "执行服务不可用",
		Stage:       controlplane.StageConnect,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// Open implements controlplane.TerminalExecutor.
func (e *ScriptedExecutor) Open(_ context.Context, target controlplane.TerminalTarget, _ string) (controlplane.TerminalChannel, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.failOpen[target.UID]; err != nil {
		return nil, err
	}
	if !e.isAvailable {
		return nil, controlplane.ErrTerminalExecutorUnavailable
	}
	e.opens[target.UID]++
	return &scriptedChannel{executor: e, target: target.UID}, nil
}

// SetAvailable changes whether a create may start.
func (e *ScriptedExecutor) SetAvailable(available bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.isAvailable = available
}

// FailOpen makes the channel of one target fail to open.
func (e *ScriptedExecutor) FailOpen(targetUID string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failOpen[targetUID] = err
}

// FailSend makes every delivery of one target fail.
func (e *ScriptedExecutor) FailSend(targetUID string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failSend[targetUID] = err
}

// SetOutput declares what every batch answers as output.
func (e *ScriptedExecutor) SetOutput(output string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.output = output
}

// HoldSend blocks the next delivery until the returned function is called, so
// a case can submit one sequence twice while the first is still in flight.
func (e *ScriptedExecutor) HoldSend() func() {
	e.mu.Lock()
	gate := make(chan struct{})
	e.gate = gate
	e.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			if e.gate == gate {
				e.gate = nil
			}
			e.mu.Unlock()
			close(gate)
		})
	}
}

// Batches returns the input batches one target received.
func (e *ScriptedExecutor) Batches(targetUID string) [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	batches := e.batches[targetUID]
	copied := make([][]byte, len(batches))
	for index := range batches {
		copied[index] = append([]byte(nil), batches[index]...)
	}
	return copied
}

// Opens returns how often one target's channel opened.
func (e *ScriptedExecutor) Opens(targetUID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.opens[targetUID]
}

// Sends returns how often one target's Send was called, including failures.
func (e *ScriptedExecutor) Sends(targetUID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sends[targetUID]
}

// Closes returns how often one target's channel closed.
func (e *ScriptedExecutor) Closes(targetUID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closes[targetUID]
}

// scriptedChannel is one recorded channel.
type scriptedChannel struct {
	executor *ScriptedExecutor
	target   string
	once     sync.Once
}

// Send implements controlplane.TerminalChannel.
func (c *scriptedChannel) Send(_ context.Context, _ uint64, data []byte) (controlplane.TerminalReceipt, error) {
	c.executor.mu.Lock()
	c.executor.sends[c.target]++
	gate := c.executor.gate
	c.executor.mu.Unlock()
	if gate != nil {
		<-gate
	}
	c.executor.mu.Lock()
	defer c.executor.mu.Unlock()
	if err := c.executor.failSend[c.target]; err != nil {
		return controlplane.TerminalReceipt{}, err
	}
	c.executor.batches[c.target] = append(c.executor.batches[c.target], append([]byte(nil), data...))
	return controlplane.TerminalReceipt{
		Result: controlplane.TerminalReceiptAccepted,
		Output: []byte(c.executor.output),
	}, nil
}

// Close implements controlplane.TerminalChannel.
func (c *scriptedChannel) Close(context.Context) (controlplane.TerminalReceipt, error) {
	c.once.Do(func() {
		c.executor.mu.Lock()
		defer c.executor.mu.Unlock()
		c.executor.closes[c.target]++
	})
	return controlplane.TerminalReceipt{Result: controlplane.TerminalReceiptAccepted}, nil
}

// StartOwner bootstraps the first Owner and returns its usable session.
func (term *Terminal) StartOwner(t *testing.T, username string) (string, string) {
	t.Helper()
	oneTime := term.Identity.BootstrapOwner(t, username)
	token, _ := term.Identity.SignIn(t, username, oneTime)
	term.Identity.ChangePassword(t, token, oneTime, username+"-pass-1")
	token, _ = term.Identity.SignIn(t, username, username+"-pass-1")
	return username, token
}

// OwnerToken returns the bootstrapped Owner's token.
func (term *Terminal) OwnerToken(t *testing.T) string {
	t.Helper()
	if term.ownerToken == "" {
		term.ownerUsername, term.ownerToken = term.StartOwner(t, "root")
	}
	return term.ownerToken
}

// AccountToken creates one account as the Owner and returns its token.
func (term *Terminal) AccountToken(t *testing.T, username, role string) string {
	t.Helper()
	password := username + "-initial-1"
	status, body := term.Identity.HTTP.Call(t, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": username, "role": role, "oneTimePassword": password,
	}, map[string]string{"Authorization": "Bearer " + term.OwnerToken(t)})
	if status != http.StatusCreated {
		t.Fatalf("create account %s = %d %v", username, status, body)
	}
	token, _ := term.Identity.SignIn(t, username, password)
	term.Identity.ChangePassword(t, token, password, username+"-pass-1")
	token, _ = term.Identity.SignIn(t, username, username+"-pass-1")
	return token
}

// Call sends a console-gated request.
func (term *Terminal) Call(t *testing.T, method, path string, body any, token string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	copied := map[string]string{}
	for key, value := range headers {
		copied[key] = value
	}
	if token != "" {
		copied["Authorization"] = "Bearer " + token
	}
	return term.HTTP.Call(t, method, path, body, copied)
}

// Create asks for one session.
func (term *Terminal) Create(t *testing.T, token, extension string, target controlplane.TerminalTarget) (int, map[string]any) {
	t.Helper()
	return term.CreateWith(t, token, extension, target, nil)
}

// CreateWith asks for one session with extra request headers, so a case can
// bind the entry origin (KOSD-C-91).
func (term *Terminal) CreateWith(t *testing.T, token, extension string, target controlplane.TerminalTarget, headers map[string]string) (int, map[string]any) {
	t.Helper()
	return term.Call(t, http.MethodPost, "/api/v1/terminal/sessions", map[string]any{
		"extension":  extension,
		"targetType": target.Type,
		"targetId":   target.ID,
		"targetUid":  target.UID,
		"namespace":  target.Namespace,
		"container":  target.Container,
	}, token, headers)
}

// Attach connects one session with a terminal token.
func (term *Terminal) Attach(t *testing.T, token, sessionID, query string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	path := "/api/v1/terminal/sessions/" + sessionID + "/attach"
	if query != "" {
		path += "?" + query
	}
	return term.Call(t, http.MethodPost, path, nil, token, headers)
}

// Input sends one batch with a terminal token.
func (term *Terminal) Input(t *testing.T, token, sessionID string, sequence uint64, data string) (int, map[string]any) {
	t.Helper()
	return term.InputWith(t, token, sessionID, sequence, data, nil)
}

// InputWith sends one batch with extra request headers, so a case can repeat
// the entry origin the token is bound to.
func (term *Terminal) InputWith(t *testing.T, token, sessionID string, sequence uint64, data string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	return term.Call(t, http.MethodPost, "/api/v1/terminal/sessions/"+sessionID+"/input", map[string]any{
		"sequence": sequence,
		"data":     base64Of(data),
	}, token, headers)
}

// Status reads one session as its operator.
func (term *Terminal) Status(t *testing.T, token, sessionID string) (int, map[string]any) {
	t.Helper()
	return term.Call(t, http.MethodGet, "/api/v1/terminal/sessions/"+sessionID, nil, token, nil)
}

// Renew issues a new token.
func (term *Terminal) Renew(t *testing.T, token, sessionID string) (int, map[string]any) {
	t.Helper()
	return term.Call(t, http.MethodPost, "/api/v1/terminal/sessions/"+sessionID+"/renew", nil, token, nil)
}

// End ends one session.
func (term *Terminal) End(t *testing.T, token, sessionID string) (int, map[string]any) {
	t.Helper()
	return term.Call(t, http.MethodDelete, "/api/v1/terminal/sessions/"+sessionID, nil, token, nil)
}

// Audit reads one session's trail.
func (term *Terminal) AuditEvents(t *testing.T, token, sessionID, query string) (int, map[string]any) {
	t.Helper()
	path := "/api/v1/terminal/sessions/" + sessionID + "/audit"
	if query != "" {
		path += "?" + query
	}
	return term.Call(t, http.MethodGet, path, nil, token, nil)
}

// Nodes reads the node inventory.
func (term *Terminal) Nodes(t *testing.T, token, extension string) (int, map[string]any) {
	t.Helper()
	return term.Call(t, http.MethodGet, "/api/v1/terminal/nodes?extension="+extension, nil, token, nil)
}

// SetIdle sets the terminal idle setting through the settings surface.
func (term *Terminal) SetIdle(t *testing.T, token, value string) (int, map[string]any) {
	t.Helper()
	return term.Call(t, http.MethodPut, "/api/v1/settings/"+controlplane.TerminalIdleSettingID, map[string]any{"value": value}, token, nil)
}

// PodTarget writes an application, its workload and one Pod, and returns the
// target identity the terminal surface reads (KOSD-C-95).
func (term *Terminal) PodTarget(t *testing.T, application, owner string) controlplane.TerminalTarget {
	t.Helper()
	return term.PodTargets(t, application, owner, "app")[0]
}

// PodTargetPhase is PodTarget with a chosen Pod phase, so a case can request
// an already ended target (KOSD-C-93).
func (term *Terminal) PodTargetPhase(t *testing.T, application, owner, phase string) controlplane.TerminalTarget {
	t.Helper()
	term.Cluster.CreateIn(t, Namespace, &unstructured.Unstructured{Object: RuntimeApplication(application, map[string]any{
		"owner": owner,
	}, application)})
	term.Cluster.CreateClusterObject(t, "apps", "v1", "deployments", Namespace, RuntimeDeployment(application, 1))
	pod := term.Cluster.CreatePod(t, PodFixture{
		Name:   application + "-0",
		Labels: map[string]string{"app": application},
		Ready:  true,
		Phase:  phase,
	})
	return controlplane.TerminalTarget{
		Type:      controlplane.TerminalTargetTypePod,
		ID:        pod.GetName(),
		UID:       string(pod.GetUID()),
		Namespace: pod.GetNamespace(),
		Container: "app",
	}
}

// PodTargets writes an application, its workload and one Pod carrying the
// named containers, and returns one target per container. The containers all
// share one target UID, which is what the per-target bound counts (KOSD-C-94).
func (term *Terminal) PodTargets(t *testing.T, application, owner string, containers ...string) []controlplane.TerminalTarget {
	t.Helper()
	term.Cluster.CreateIn(t, Namespace, &unstructured.Unstructured{Object: RuntimeApplication(application, map[string]any{
		"owner": owner,
	}, application)})
	term.Cluster.CreateClusterObject(t, "apps", "v1", "deployments", Namespace, RuntimeDeployment(application, 1))
	pod := term.Cluster.CreatePod(t, PodFixture{
		Name:       application + "-0",
		Labels:     map[string]string{"app": application},
		Ready:      true,
		Containers: containers[1:],
	})
	targets := make([]controlplane.TerminalTarget, 0, len(containers))
	for _, container := range containers {
		targets = append(targets, controlplane.TerminalTarget{
			Type:      controlplane.TerminalTargetTypePod,
			ID:        pod.GetName(),
			UID:       string(pod.GetUID()),
			Namespace: pod.GetNamespace(),
			Container: container,
		})
	}
	return targets
}

// TransferApplication moves one application's owner, so another account holds
// its write permission (KOSD-C-59).
func (term *Terminal) TransferApplication(t *testing.T, name, owner string) {
	t.Helper()
	resource := term.Cluster.Dynamic.Resource(schema.GroupVersionResource{
		Group: "kos.tsic.top", Version: "v1beta1", Resource: "applications",
	}).Namespace(Namespace)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	application, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read application %s: %v", name, err)
	}
	if err := unstructured.SetNestedField(application.Object, owner, "spec", "owner"); err != nil {
		t.Fatalf("set application %s owner: %v", name, err)
	}
	if _, err := resource.Update(ctx, application, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update application %s owner: %v", name, err)
	}
}

// NodeTarget writes one Ready Node and returns its target identity.
func (term *Terminal) NodeTarget(t *testing.T, name string) controlplane.TerminalTarget {
	t.Helper()
	node := term.Cluster.CreateNode(t, name)
	return controlplane.TerminalTarget{
		Type: controlplane.TerminalTargetTypeNode,
		ID:   node.GetName(),
		UID:  string(node.GetUID()),
	}
}

// CreateNode writes one Ready Node object.
func (c *Cluster) CreateNode(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	return c.CreateNodeWith(t, name, true)
}

// CreateNodeWith writes one Node object with the chosen Ready condition.
func (c *Cluster) CreateNodeWith(t *testing.T, name string, ready bool) *unstructured.Unstructured {
	t.Helper()
	resource := c.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "nodes"})
	status := "False"
	if ready {
		status = "True"
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": status}},
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	created, err := resource.Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	created.Object["status"] = object.Object["status"]
	updated, err := resource.UpdateStatus(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("write status of node %s: %v", name, err)
	}
	return updated
}

// DeleteNode removes one Node object and waits until the name is free, so a
// same-name replacement carries a new UID (KOSD-C-95).
func (c *Cluster) DeleteNode(t *testing.T, name string) {
	t.Helper()
	resource := c.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "nodes"})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if err := resource.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete node %s: %v", name, err)
	}
	Eventually(t, 10*time.Second, "node deletion", func() bool {
		_, err := resource.Get(ctx, name, metav1.GetOptions{})
		return err != nil
	})
}

// Nodes lists the Node objects the cluster holds.
func (c *Cluster) Nodes(t *testing.T) []unstructured.Unstructured {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	list, err := c.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	return list.Items
}

// base64Of encodes one input batch.
func base64Of(data string) string {
	return base64.StdEncoding.EncodeToString([]byte(data))
}

// FailureCode reads the stable code of one failure envelope.
func FailureCode(t *testing.T, body map[string]any) string {
	t.Helper()
	envelope, _ := body["error"].(map[string]any)
	code, _ := envelope["code"].(string)
	return code
}

// Capture returns one nested string from a response.
func Capture(body map[string]any, path ...string) string {
	return String(body, path...)
}

// Describe renders one response for a failure message.
func Describe(body map[string]any) string {
	return fmt.Sprintf("%v", body)
}

// HasPrefix is a tiny helper kept here so cases do not import strings.
func HasPrefix(value, prefix string) bool { return strings.HasPrefix(value, prefix) }
