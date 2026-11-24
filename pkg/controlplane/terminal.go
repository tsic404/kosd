package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// The terminal surface's fixed values (KOSD-C-90, KOSD-C-91, KOSD-C-94).
const (
	// TerminalIdleSettingID is the cluster-scoped setting the terminal
	// component contributes for the idle bound (KOSD-C-94).
	TerminalIdleSettingID = "terminal.idleTimeoutMinutes"
	// TerminalDefaultIdleMinutes is the idle bound a session takes when the
	// setting is undeclared or unwritten (KOSD-C-90).
	TerminalDefaultIdleMinutes = 10
	// TerminalMinIdleMinutes and TerminalMaxIdleMinutes bound an accepted
	// setting value (KOSD-C-94).
	TerminalMinIdleMinutes = 1
	TerminalMaxIdleMinutes = 60
	// TerminalTokenLifetime is the terminal token's validity (KOSD-C-91).
	TerminalTokenLifetime = 5 * time.Minute
	// TerminalAccountLimit and TerminalTargetLimit are the per-account and
	// per-target session bounds (KOSD-C-90, KOSD-C-94).
	TerminalAccountLimit = 4
	TerminalTargetLimit  = 4
	// TerminalNodeIdentity is the node execution identity (KOSD-C-93).
	TerminalNodeIdentity = "root@node"
	// TerminalTargetTypeNode and TerminalTargetTypePod are the target kinds
	// (KOSD-C-95).
	TerminalTargetTypeNode = "node"
	TerminalTargetTypePod  = "pod"
)

// Session states (KOSD-C-90). Only TerminalStateConnected accepts input.
const (
	TerminalStateConnecting = "connecting"
	TerminalStateConnected  = "connected"
	TerminalStateClosed     = "closed"
	TerminalStateFailed     = "failed"
)

// Session end reasons a reader can observe. The idle reason is fixed by
// KOSD-C-94; the rest name why a session left the live set.
const (
	TerminalReasonIdle            = "terminal_idle_timeout"
	TerminalReasonEnded           = "terminal_session_ended"
	TerminalReasonTokenExpired    = "terminal_token_expired"
	TerminalReasonUnauthenticated = "terminal_unauthenticated"
)

// Terminal audit events (KOSD-C-92) and input receipt results.
const (
	TerminalEventEstablish = "establish"
	TerminalEventReject    = "reject"
	TerminalEventInput     = "input"
	TerminalEventRenew     = "renew"
	TerminalEventEnd       = "end"

	TerminalReceiptAccepted  = "accepted"
	TerminalReceiptRejected  = "rejected"
	TerminalReceiptDuplicate = "duplicate"
	TerminalReceiptUnknown   = "unknown"
)

// TerminalTarget is the identity of one session's target (KOSD-C-95). A node
// carries empty namespace and container strings.
type TerminalTarget struct {
	// Type is node or pod.
	Type string `json:"targetType"`
	// ID is the object's metadata.name.
	ID string `json:"targetId"`
	// UID is the object's metadata.uid.
	UID string `json:"targetUid"`
	// Namespace is the Pod's namespace; empty for a node.
	Namespace string `json:"namespace"`
	// Container is the selected Pod container; empty for a node.
	Container string `json:"container"`
}

// Ref renders the target as the object reference the audit stream carries.
func (t TerminalTarget) Ref() ObjectRef {
	kind := "Node"
	if t.Type == TerminalTargetTypePod {
		kind = "Pod"
	}
	name := t.ID
	if t.Type == TerminalTargetTypePod && t.Container != "" {
		name = t.ID + "/" + t.Container
	}
	return ObjectRef{Group: "terminal", Kind: kind, Namespace: t.Namespace, Name: name}
}

// Identity is the execution identity a session reaches the target as
// (KOSD-C-93). A Pod session runs as the selected container's own shell.
func (t TerminalTarget) Identity() string {
	if t.Type == TerminalTargetTypeNode {
		return TerminalNodeIdentity
	}
	return "container:" + t.Namespace + "/" + t.ID + "/" + t.Container
}

// TerminalSession is the console session behind a terminal request (KOSD-C-63).
type TerminalSession struct {
	// Username is the account's login name.
	Username string
	// Role is the account's current role.
	Role string
	// ConsoleHash locates the console session that made the request, so a
	// terminal token dies when that session ends (KOSD-C-91).
	ConsoleHash string
	// ExpiresAt is the console session's own deadline.
	ExpiresAt time.Time
}

// TerminalConsole resolves the console session and checks that a bound console
// session is still usable. It is the only seam that knows the account store.
type TerminalConsole interface {
	// Session returns the console session behind the request; false means the
	// request carries none.
	Session(ctx context.Context) (TerminalSession, bool)
	// Alive returns the console session with that digest and the account's
	// current role; false means it ended, was disabled or expired.
	Alive(ctx context.Context, consoleHash string) (TerminalSession, bool)
	// WithSession puts a resolved session on a context, so the authorization
	// seam judges a token-bound request as the same account (KOSD-C-91).
	WithSession(ctx context.Context, session TerminalSession) context.Context
}

// TerminalAudit stores terminal events in the control-plane audit stream
// (KOSD-C-92) and reads them per session (KOSD-C-96). A failed write is
// reported to the caller, because the terminal surface refuses to run without
// its trail.
type TerminalAudit interface {
	// Append writes one event.
	Append(ctx context.Context, event TerminalEvent) error
	// Read returns events of one session after the given event id, oldest
	// first, and the id a further read starts after; zero means no more.
	Read(ctx context.Context, sessionID string, after int64, limit int) ([]TerminalEvent, int64, error)
	// Available reports whether the trail can answer. Input and renewal check
	// it before delivering, so a batch is never accepted untrailed.
	Available(ctx context.Context) error
}

// TerminalEvent is one terminal audit record (KOSD-C-92): the operator, the
// declaring extension, the session, the target's immutable identity, the
// container, the execution identity, the event, its result and its time. An
// input event also carries the batch sequence and the executor's receipt. Raw
// input, output and tokens never enter it.
type TerminalEvent struct {
	// ID is the event's position in the trail.
	ID int64 `json:"eventId"`
	// Actor is the account the request was made by.
	Actor string `json:"actor"`
	// Extension is the identifier of the terminal component.
	Extension string `json:"extension"`
	// SessionID is the session the event belongs to; empty for a rejection
	// that never created one.
	SessionID string `json:"sessionId,omitempty"`
	// TargetType, TargetID and TargetUID name the immutable target.
	TargetType string `json:"targetType"`
	TargetID   string `json:"targetId"`
	TargetUID  string `json:"targetUid"`
	// Namespace and Container scope a Pod target; both are empty for a node.
	Namespace string `json:"namespace"`
	Container string `json:"container"`
	// ExecutionIdentity is the identity the session runs as.
	ExecutionIdentity string `json:"executionIdentity"`
	// Event is one of the TerminalEvent constants.
	Event string `json:"event"`
	// Result is accepted, rejected, failed or unknown; an input event may also
	// be duplicate.
	Result string `json:"result"`
	// Sequence is the input batch sequence; nil for the other events.
	Sequence *uint64 `json:"sequence,omitempty"`
	// Receipt is the executor's answer to one input batch.
	Receipt string `json:"receipt,omitempty"`
	// OccurredAt is when the event happened.
	OccurredAt time.Time `json:"occurredAt"`
}

// TerminalExtensions reports whether a component is active (KOSD-F-16).
type TerminalExtensions interface {
	IsActive(identifier string) bool
}

// TerminalExecutor opens the execution channel of one target (ADR-043). A nil
// executor means this deployment has no execution service, which KOSD-C-93
// answers with terminal_executor_unavailable.
type TerminalExecutor interface {
	// Available reports whether the executor can accept a new session.
	Available(ctx context.Context) *Error
	// Open establishes the channel of one target.
	Open(ctx context.Context, target TerminalTarget, identity string) (TerminalChannel, error)
}

// TerminalChannel is one established execution channel. A channel reports the
// executor's own receipt for every batch; it does not claim a batch was a
// complete command or that it succeeded.
type TerminalChannel interface {
	// Send delivers one input batch and returns the executor's receipt.
	Send(ctx context.Context, sequence uint64, data []byte) (TerminalReceipt, error)
	// Close ends the channel.
	Close(ctx context.Context) (TerminalReceipt, error)
}

// TerminalReceipt is the executor's answer to one batch.
type TerminalReceipt struct {
	// Result is accepted, rejected or unknown.
	Result string
	// Output is what the executor produced while receiving the batch; it is
	// never stored and never audited.
	Output []byte
}

// Executor failures a target may report (KOSD-C-93).
var (
	ErrTerminalNodeUnreachable     = errors.New("terminal: node unreachable")
	ErrTerminalExecutorUnavailable = errors.New("terminal: executor unavailable")
	ErrTerminalTargetEnded         = errors.New("terminal: target ended")
	ErrTerminalCredentialBoundary  = errors.New("terminal: credential boundary not guaranteed")
)

// TerminalBoundaryTarget is the live target a credential-boundary judge sees
// (ADR-043): the identity a session would reach, the resolved Pod whose
// container would run the shell, and whether its application carries
// sensitive configuration that the shell could read.
type TerminalBoundaryTarget struct {
	// Target is the session's target identity (KOSD-C-95).
	Target TerminalTarget
	// Pod is the live Pod object for a Pod target; nil for a node.
	Pod map[string]any
	// Sensitive marks an application whose sensitive config values cannot be
	// ruled out of the container, or whose definition could not be read.
	Sensitive bool
}

// CredentialBoundary judges whether a session for one target can be isolated
// from the control plane's credentials and application sensitive parameters
// (ADR-043). A nil boundary refuses every target: the surface never assumes an
// isolation it cannot judge.
type CredentialBoundary interface {
	Judge(ctx context.Context, target TerminalBoundaryTarget) *Error
}

// TerminalConfig wires the terminal session surface (KOSD-C-90 to KOSD-C-96).
type TerminalConfig struct {
	// Console resolves console sessions; required.
	Console TerminalConsole
	// Audit stores terminal events; nil makes every terminal request fail with
	// terminal_audit_unavailable instead of running untrailed.
	Audit TerminalAudit
	// Extensions checks that the requesting component is active.
	Extensions TerminalExtensions
	// Executor opens execution channels; nil reports
	// terminal_executor_unavailable.
	Executor TerminalExecutor
	// Boundary judges credential isolation; nil refuses every target.
	Boundary CredentialBoundary
	// IdleTimeout reads the idle bound for new sessions (KOSD-C-94); nil uses
	// the default.
	IdleTimeout func(ctx context.Context) (int, *Error)
}

// terminalResolvedTarget is the live cluster state behind one requested
// target. Resolution never decides authorization: a caller learns the same
// "target unavailable" answer for an absent object and for one it may not
// reach (KOSD-C-93).
type terminalResolvedTarget struct {
	target         TerminalTarget
	node           map[string]any
	pod            map[string]any
	application    map[string]any
	applicationRef ObjectRef
	sensitive      bool
	nodeReady      bool
	ended          bool
}

// terminalSession is one live session. It is temporary state: it lives in this
// process only, adds no kernel object and is never reconciled (KOSD-C-90).
type terminalSession struct {
	// mu guards the mutable fields, so concurrent input batches and the idle
	// sweep see one state transition at a time.
	mu           sync.Mutex
	id           string
	account      string
	role         string
	extension    string
	origin       string
	consoleHash  string
	target       TerminalTarget
	identity     string
	state        string
	reason       string
	idleMinutes  int
	lastActive   time.Time
	tokenHash    string
	tokenExpires time.Time
	created      time.Time
	channel      TerminalChannel
	receipts     map[uint64]TerminalReceipt
	pending      map[uint64]bool
}

// terminalView is one session as a caller reads it (KOSD-C-90).
type terminalView struct {
	SessionID          string         `json:"sessionId"`
	TargetType         string         `json:"targetType"`
	TargetID           string         `json:"targetId"`
	TargetUID          string         `json:"targetUid"`
	Namespace          string         `json:"namespace"`
	Container          string         `json:"container"`
	ExecutionIdentity  string         `json:"executionIdentity"`
	State              string         `json:"state"`
	Reason             string         `json:"reason,omitempty"`
	ExpiresAt          time.Time      `json:"expiresAt"`
	IdleTimeoutMinutes int            `json:"idleTimeoutMinutes"`
	Limits             terminalLimits `json:"limits"`
	Usage              terminalUsage  `json:"usage"`
}

// terminalLimits is the account and target bound (KOSD-C-94).
type terminalLimits struct {
	Account int `json:"account"`
	Target  int `json:"target"`
}

// terminalUsage is the account and target occupancy at response time.
type terminalUsage struct {
	Account int `json:"account"`
	Target  int `json:"target"`
}

// terminalRegistry holds the live sessions and their quota accounting. Every
// mutation runs under one lock, so concurrent creates cannot pass a bound.
type terminalRegistry struct {
	mu       sync.Mutex
	sessions map[string]*terminalSession
	order    []string
	accounts map[string]int
	targets  map[string]int
}

func newTerminalRegistry() *terminalRegistry {
	return &terminalRegistry{
		sessions: map[string]*terminalSession{},
		accounts: map[string]int{},
		targets:  map[string]int{},
	}
}

// terminalStateIsLive reports whether a state occupies a quota slot
// (KOSD-C-94).
func terminalStateIsLive(state string) bool {
	return state == TerminalStateConnecting || state == TerminalStateConnected
}

// counts reads the occupancy one account contributes and one target holds.
func (r *terminalRegistry) counts(account, targetUID string) (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accounts[account], r.targets[targetUID]
}

// insert adds a session when both bounds allow it. It returns the existing
// session when the same account already holds a live session for the same
// extension, target and container, so a repeated create takes no second slot
// and delivers no second token (KOSD-C-94). A refused insert reports which
// bound was reached.
func (r *terminalRegistry) insert(session *terminalSession) (*terminalSession, *Error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.order {
		existing := r.sessions[id]
		if existing == nil {
			continue
		}
		existing.mu.Lock()
		live := terminalStateIsLive(existing.state)
		same := existing.account == session.account && existing.extension == session.extension &&
			existing.target.UID == session.target.UID && existing.target.Container == session.target.Container
		existing.mu.Unlock()
		if live && same {
			return existing, nil
		}
	}
	if r.accounts[session.account] >= TerminalAccountLimit || r.targets[session.target.UID] >= TerminalTargetLimit {
		return nil, terminalFailure(CodeTerminalSessionLimit, StageConnect,
			"账号或目标的终端会话已达上限，需先释放名额")
	}
	r.sessions[session.id] = session
	r.order = append(r.order, session.id)
	r.accounts[session.account]++
	r.targets[session.target.UID]++
	return session, nil
}

// get returns one session.
func (r *terminalRegistry) get(id string) (*terminalSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[id]
	return session, ok
}

// byToken resolves the live session one stored token digest names.
func (r *terminalRegistry) byToken(digest string) (*terminalSession, bool) {
	if digest == "" {
		return nil, false
	}
	for _, session := range r.snapshot() {
		session.mu.Lock()
		stored, live := session.tokenHash, terminalStateIsLive(session.state)
		session.mu.Unlock()
		if live && subtle.ConstantTimeCompare([]byte(stored), []byte(digest)) == 1 {
			return session, true
		}
	}
	return nil, false
}

// release frees a session's slot. It is called exactly once per live session,
// after close has moved the session out of the live states.
func (r *terminalRegistry) release(session *terminalSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[session.account]--
	r.targets[session.target.UID]--
	if r.accounts[session.account] <= 0 {
		delete(r.accounts, session.account)
	}
	if r.targets[session.target.UID] <= 0 {
		delete(r.targets, session.target.UID)
	}
}

// snapshot returns every session known to the registry.
func (r *terminalRegistry) snapshot() []*terminalSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	sessions := make([]*terminalSession, 0, len(r.order))
	for _, id := range r.order {
		if session := r.sessions[id]; session != nil {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

// newTerminalToken returns the plaintext token and the digest the server keeps.
func newTerminalToken() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, terminalTokenDigest(token), nil
}

// terminalTokenDigest hashes a token for storage; the plaintext is never kept
// (KOSD-C-91).
func terminalTokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newTerminalSessionID returns a random session identifier.
func newTerminalSessionID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "ts-" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return "ts-" + base64.RawURLEncoding.EncodeToString(raw)
}

// validIdleMinutes reports whether a stored setting value is acceptable
// (KOSD-C-94).
func validIdleMinutes(value int) bool {
	return value >= TerminalMinIdleMinutes && value <= TerminalMaxIdleMinutes
}

// TerminalIdleMinutes reads one stored setting value; an empty value selects
// the default. The second result is false for a value the model rejects.
func TerminalIdleMinutes(value string) (int, bool) {
	if value == "" {
		return TerminalDefaultIdleMinutes, true
	}
	minutes := 0
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
		minutes = minutes*10 + int(character-'0')
		if minutes > TerminalMaxIdleMinutes {
			return 0, false
		}
	}
	return minutes, validIdleMinutes(minutes)
}

// ValidateTerminalSetting is the write gate of the terminal idle setting
// (KOSD-C-94): only the global Owner writes it, and only as an integer inside
// the accepted range.
func ValidateTerminalSetting(id, role, value string) *Error {
	if id != TerminalIdleSettingID {
		return nil
	}
	if role != "Owner" {
		failure := terminalFailure(CodePermissionDenied, StageAuthorize, "仅全局 Owner 可以修改终端空闲时长")
		failure.RequiresUserAction = true
		return failure
	}
	minutes, ok := TerminalIdleMinutes(value)
	if !ok || !validIdleMinutes(minutes) {
		return invalidField("终端空闲时长必须是 1 至 60 的整数分钟")
	}
	return nil
}

// terminalService is the terminal surface over the registry and its seams.
type terminalService struct {
	config   TerminalConfig
	registry *terminalRegistry
	now      func() time.Time
	log      *slog.Logger
}

func newTerminalService(config TerminalConfig, now func() time.Time, log *slog.Logger) *terminalService {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &terminalService{
		config:   config,
		registry: newTerminalRegistry(),
		now:      now,
		log:      log,
	}
}

// idleMinutes reads the effective idle bound of a new session (KOSD-C-94).
func (t *terminalService) idleMinutes(ctx context.Context) (int, *Error) {
	if t.config.IdleTimeout == nil {
		return TerminalDefaultIdleMinutes, nil
	}
	minutes, failure := t.config.IdleTimeout(ctx)
	if failure != nil {
		return 0, failure
	}
	if !validIdleMinutes(minutes) {
		return 0, terminalFailure(CodeInternal, StageRead, "终端空闲配置无效")
	}
	return minutes, nil
}

// append records one terminal event. Audit is on the correctness path of the
// terminal surface (KOSD-C-92), so a failure is returned, never swallowed.
func (t *terminalService) append(ctx context.Context, event TerminalEvent) error {
	if t.config.Audit == nil {
		return errors.New("terminal: no audit trail is configured")
	}
	event.OccurredAt = t.now().UTC()
	return t.config.Audit.Append(ctx, event)
}

// event builds one audit record from a session (KOSD-C-92).
func (t *terminalService) event(session *terminalSession, event, result string) TerminalEvent {
	return TerminalEvent{
		Actor:             session.account,
		Extension:         session.extension,
		SessionID:         session.id,
		TargetType:        session.target.Type,
		TargetID:          session.target.ID,
		TargetUID:         session.target.UID,
		Namespace:         session.target.Namespace,
		Container:         session.target.Container,
		ExecutionIdentity: session.identity,
		Event:             event,
		Result:            result,
	}
}

// view renders one session with the occupancy a reader sees (KOSD-C-90).
func (t *terminalService) view(session *terminalSession) terminalView {
	session.mu.Lock()
	state, reason := session.state, session.reason
	expires := session.tokenExpires
	idle := session.idleMinutes
	session.mu.Unlock()
	accountUsage, targetUsage := t.registry.counts(session.account, session.target.UID)
	return terminalView{
		SessionID:          session.id,
		TargetType:         session.target.Type,
		TargetID:           session.target.ID,
		TargetUID:          session.target.UID,
		Namespace:          session.target.Namespace,
		Container:          session.target.Container,
		ExecutionIdentity:  session.identity,
		State:              state,
		Reason:             reason,
		ExpiresAt:          expires.UTC(),
		IdleTimeoutMinutes: idle,
		Limits:             terminalLimits{Account: TerminalAccountLimit, Target: TerminalTargetLimit},
		Usage:              terminalUsage{Account: accountUsage, Target: targetUsage},
	}
}

// close ends a session with a reason, frees its slot and records the end once
// (KOSD-C-90, KOSD-C-92). The end record is written even when the reason is a
// failure, so a session never disappears from the trail.
func (t *terminalService) close(ctx context.Context, session *terminalSession, state, reason string) error {
	session.mu.Lock()
	if !terminalStateIsLive(session.state) {
		session.mu.Unlock()
		return nil
	}
	previous := session.state
	session.state = state
	session.reason = reason
	session.tokenHash = ""
	channel := session.channel
	session.channel = nil
	session.mu.Unlock()
	var (
		end    TerminalReceipt
		endErr error
	)
	if channel != nil {
		end, endErr = channel.Close(ctx)
	}
	t.registry.release(session)
	if previous == "" {
		return nil
	}
	result := terminalResultOf(state)
	switch {
	case endErr != nil, end.Result == TerminalReceiptUnknown:
		result = TerminalReceiptUnknown
	case end.Result == TerminalReceiptRejected:
		result = ResultRejected
	}
	event := t.event(session, TerminalEventEnd, result)
	event.Receipt = end.Result
	return t.append(ctx, event)
}

// terminalResultOf names the audit outcome of an ended session.
func terminalResultOf(state string) string {
	if state == TerminalStateFailed {
		return ResultFailed
	}
	return ResultAccepted
}

// sweep closes sessions whose component stopped being active, whose token ran
// out, or whose idle bound elapsed (KOSD-C-91, KOSD-C-94). A failed audit
// record is logged, because the session is already closed.
func (t *terminalService) sweep(ctx context.Context) int {
	now := t.now()
	closed := 0
	for _, session := range t.registry.snapshot() {
		session.mu.Lock()
		state := session.state
		lastActive := session.lastActive
		idleMinutes := session.idleMinutes
		expires := session.tokenExpires
		session.mu.Unlock()
		if !terminalStateIsLive(state) {
			continue
		}
		reason := ""
		switch {
		case t.config.Extensions == nil || !t.config.Extensions.IsActive(session.extension):
			reason = TerminalReasonUnauthenticated
		case !expires.IsZero() && !now.Before(expires):
			reason = TerminalReasonTokenExpired
		case state == TerminalStateConnected &&
			now.Sub(lastActive) >= time.Duration(idleMinutes)*time.Minute:
			reason = TerminalReasonIdle
		}
		if reason == "" {
			continue
		}
		if err := t.close(ctx, session, TerminalStateClosed, reason); err != nil {
			t.log.Error("terminal close could not be audited", "session", session.id, "reason", reason, "error", err)
		}
		closed++
	}
	return closed
}

// Run sweeps expired sessions until the context ends.
func (t *terminalService) Run(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.shutdown()
			return
		case <-ticker.C:
			t.sweep(ctx)
		}
	}
}

// shutdown ends every live session without waiting for its idle bound, so a
// stopped process leaves no execution channel open.
func (t *terminalService) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, session := range t.registry.snapshot() {
		session.mu.Lock()
		channel := session.channel
		session.channel = nil
		live := terminalStateIsLive(session.state)
		if live {
			session.state = TerminalStateClosed
			session.reason = TerminalReasonEnded
			session.tokenHash = ""
		}
		session.mu.Unlock()
		if channel != nil {
			_, _ = channel.Close(ctx)
		}
		if live {
			t.registry.release(session)
		}
	}
}
