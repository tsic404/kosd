package controlplane

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maximumTerminalInputBytes bounds one input batch, so a single request cannot
// grow the session's memory without bound.
const maximumTerminalInputBytes = 64 * 1024

// One audit read page (KOSD-C-96).
const (
	terminalAuditPageSize = 50
	terminalAuditMaxPage  = 200
)

// terminalCreateRequest is one session creation (KOSD-C-90, KOSD-C-95). The
// caller submits the target's name and UID, because a same-name replacement
// must not inherit the session.
type terminalCreateRequest struct {
	// Extension is the terminal component the caller acts for.
	Extension string `json:"extension"`
	// TargetType is node or pod.
	TargetType string `json:"targetType"`
	// TargetID and TargetUID are the object's name and immutable identity.
	TargetID  string `json:"targetId"`
	TargetUID string `json:"targetUid"`
	// Namespace and Container scope a Pod; a node submits both empty.
	Namespace string `json:"namespace"`
	Container string `json:"container"`
}

// terminalCreateResponse carries the view and the one plaintext token
// delivery (KOSD-C-91).
type terminalCreateResponse struct {
	terminalView
	Token string `json:"token,omitempty"`
}

// terminalInputRequest is one input batch (KOSD-C-92).
type terminalInputRequest struct {
	// Sequence is the caller's per-session batch number.
	Sequence uint64 `json:"sequence"`
	// Data is the batch bytes, base64 encoded.
	Data string `json:"data"`
}

// terminalInputResponse is the executor's receipt for one batch.
type terminalInputResponse struct {
	Sequence  uint64 `json:"sequence"`
	Result    string `json:"result"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Output    string `json:"output,omitempty"`
}

// terminalAuditResponse is one page of a session's audit events (KOSD-C-96).
type terminalAuditResponse struct {
	Items      []TerminalEvent `json:"items"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// terminalNodeTarget is one node inventory item (KOSD-C-95).
type terminalNodeTarget struct {
	TargetType          string    `json:"targetType"`
	TargetID            string    `json:"targetId"`
	TargetUID           string    `json:"targetUid"`
	Namespace           string    `json:"namespace"`
	Container           string    `json:"container"`
	ExecutionIdentities []string  `json:"executionIdentities"`
	ObservedAt          time.Time `json:"observedAt"`
	Stale               bool      `json:"stale"`
}

// terminalNodeList is the node inventory one extension reads (KOSD-C-95).
type terminalNodeList struct {
	Items []terminalNodeTarget `json:"items"`
}

// terminalConsoleOf resolves the console session of one gated request. A
// request without one answers terminal_unauthenticated (KOSD-C-91).
func (s *Server) terminalConsoleOf(r *http.Request) (TerminalSession, *Error) {
	if s.terminal == nil || s.terminal.config.Console == nil {
		return TerminalSession{}, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "终端会话缺少控制台登录")
	}
	session, ok := s.terminal.config.Console.Session(r.Context())
	if !ok || session.Username == "" {
		return TerminalSession{}, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "终端会话缺少控制台登录")
	}
	return session, nil
}

// terminalByToken resolves the session one terminal token names (KOSD-C-91).
// The token is the only credential this path accepts: no cookie and no console
// session is consulted.
func (s *Server) terminalByToken(r *http.Request) (*terminalSession, *Error) {
	if s.terminal == nil {
		return nil, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "缺少终端令牌")
	}
	token := bearerOf(r)
	if token == "" {
		return nil, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "缺少终端令牌")
	}
	session, ok := s.terminal.registry.byToken(terminalTokenDigest(token))
	if !ok {
		return nil, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "终端令牌无效或已失效")
	}
	return session, nil
}

// terminalSessionPath checks that a token is used on the session it names
// (KOSD-C-91): a token for another session is forbidden, not silently
// redirected.
func terminalSessionPath(r *http.Request, session *terminalSession) *Error {
	if id := r.PathValue("sessionId"); id != "" && id != session.id {
		return terminalFailure(CodeTerminalForbidden, StageAuthorize, "令牌与终端会话不符")
	}
	return nil
}

// terminalExtensionActive reports whether the component a session acts for is
// still active (KOSD-F-16). An uninstalled component's tokens die with it.
func (s *Server) terminalExtensionActive(session *terminalSession) bool {
	return s.terminal.config.Extensions != nil &&
		s.terminal.config.Extensions.IsActive(session.extension)
}

// closeTerminalSession ends one session and logs an audit failure, which the
// caller cannot repair at that point.
func (s *Server) closeTerminalSession(ctx context.Context, session *terminalSession, state, reason string) {
	if err := s.terminal.close(ctx, session, state, reason); err != nil {
		s.log.Error("terminal session close could not be audited", "session", session.id, "reason", reason, "error", err)
	}
}

// terminalAuditReady checks the trail before a request changes execution state
// (KOSD-C-92): no new session, input batch or token is accepted untrailed.
func (s *Server) terminalAuditReady(ctx context.Context) *Error {
	if s.terminal.config.Audit == nil {
		return terminalFailure(CodeTerminalAuditUnavailable, StageApply, "终端审计不可用，已拒绝请求")
	}
	if err := s.terminal.config.Audit.Available(ctx); err != nil {
		s.log.Error("terminal audit unavailable", "error", err)
		return terminalFailure(CodeTerminalAuditUnavailable, StageApply, "终端审计不可用，已拒绝请求")
	}
	return nil
}

// handleTerminalCreate creates one terminal session (KOSD-C-90).
func (s *Server) handleTerminalCreate(w http.ResponseWriter, r *http.Request) {
	console, failure := s.terminalConsoleOf(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	var request terminalCreateRequest
	if failure := decodeBody(r, true, &request); failure != nil {
		writeError(w, failure)
		return
	}
	target, failure := terminalTargetOf(request)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if request.Extension == "" || !RecordableName(request.Extension) {
		writeError(w, invalidField("必须给出终端组件标识"))
		return
	}
	if s.terminal.config.Extensions == nil || !s.terminal.config.Extensions.IsActive(request.Extension) {
		writeError(w, &Error{
			Code:        CodeCapabilityMissing,
			Message:     "终端组件未激活，不能建立会话",
			Stage:       StageCapability,
			SideEffects: SideEffectsNone,
		})
		return
	}
	resolved, failure := s.resolveTerminalTarget(r.Context(), target)
	if failure != nil {
		s.rejectTerminal(r, console, request.Extension, target, failure)
		writeError(w, failure)
		return
	}
	if failure := s.authorizeTerminalTarget(r.Context(), console, resolved); failure != nil {
		s.rejectTerminal(r, console, request.Extension, target, failure)
		writeError(w, failure)
		return
	}
	if failure := s.terminalExecutorReady(r.Context(), resolved); failure != nil {
		s.rejectTerminal(r, console, request.Extension, target, failure)
		writeError(w, failure)
		return
	}
	if failure := s.judgeTerminalBoundary(r.Context(), resolved); failure != nil {
		s.rejectTerminal(r, console, request.Extension, target, failure)
		writeError(w, failure)
		return
	}
	if failure := s.terminalAuditReady(r.Context()); failure != nil {
		s.failAllSessions(r.Context(), "审计不可用，会话已结束")
		writeError(w, failure)
		return
	}
	idle, failure := s.terminal.idleMinutes(r.Context())
	if failure != nil {
		writeError(w, failure)
		return
	}
	token, digest, err := newTerminalToken()
	if err != nil {
		writeError(w, internal(StageConnect, "生成终端令牌失败"))
		return
	}
	now := s.now()
	session := &terminalSession{
		id:           newTerminalSessionID(),
		account:      console.Username,
		role:         console.Role,
		extension:    request.Extension,
		origin:       terminalOriginOf(r),
		consoleHash:  console.ConsoleHash,
		target:       target,
		identity:     target.Identity(),
		state:        TerminalStateConnecting,
		idleMinutes:  idle,
		lastActive:   now,
		tokenHash:    digest,
		tokenExpires: now.Add(TerminalTokenLifetime),
		created:      now,
		receipts:     map[uint64]TerminalReceipt{},
		pending:      map[uint64]bool{},
	}
	stored, failure := s.terminal.registry.insert(session)
	if failure != nil {
		s.rejectTerminal(r, console, request.Extension, target, failure)
		writeError(w, failure)
		return
	}
	if stored != session {
		// A repeated create answers the existing session without a second
		// token delivery (KOSD-C-94).
		writeJSON(w, http.StatusOK, terminalCreateResponse{terminalView: s.terminal.view(stored)})
		return
	}
	if err := s.terminal.append(r.Context(), s.terminal.event(session, TerminalEventEstablish, ResultAccepted)); err != nil {
		s.failAllSessions(r.Context(), "审计不可用，会话已结束")
		writeError(w, terminalFailure(CodeTerminalAuditUnavailable, StageApply, "终端审计不可用，会话未建立"))
		return
	}
	writeJSON(w, http.StatusCreated, terminalCreateResponse{terminalView: s.terminal.view(session), Token: token})
}

// handleTerminalStatus reads one session (KOSD-C-90).
func (s *Server) handleTerminalStatus(w http.ResponseWriter, r *http.Request) {
	console, failure := s.terminalConsoleOf(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	session, failure := s.terminalSessionFor(console, r.PathValue("sessionId"))
	if failure != nil {
		writeError(w, failure)
		return
	}
	s.expireTerminal(r.Context(), session)
	writeJSON(w, http.StatusOK, s.terminal.view(session))
}

// handleTerminalRenew issues a new token for one session after re-checking its
// scope and the current authorization (KOSD-C-91). It does not reset the idle
// bound (KOSD-C-94).
func (s *Server) handleTerminalRenew(w http.ResponseWriter, r *http.Request) {
	console, failure := s.terminalConsoleOf(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	session, failure := s.terminalSessionFor(console, r.PathValue("sessionId"))
	if failure != nil {
		writeError(w, failure)
		return
	}
	// A renewal delivers a plaintext token bound to the session's account, so
	// only that account may ask for one. An Owner may read another session's
	// status and audit, but a token issued to the Owner would misattribute
	// later input and revoke the account's own token (KOSD-C-91, KOSD-C-92).
	if session.account != console.Username {
		writeError(w, terminalFailure(CodeTerminalForbidden, StageAuthorize, "只有会话所属账号可以续期"))
		return
	}
	session.mu.Lock()
	live := terminalStateIsLive(session.state)
	hash, expires := session.tokenHash, session.tokenExpires
	session.mu.Unlock()
	if !live {
		writeError(w, terminalFailure(CodeTerminalTargetEnded, StageConnect, "会话已结束，不能续期"))
		return
	}
	if hash == "" || !s.now().Before(expires) {
		s.closeTerminalSession(r.Context(), session, TerminalStateClosed, TerminalReasonTokenExpired)
		writeError(w, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "终端令牌已到期，不能续期"))
		return
	}
	if !s.terminalExtensionActive(session) {
		s.closeTerminalSession(r.Context(), session, TerminalStateClosed, TerminalReasonUnauthenticated)
		writeError(w, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "终端组件未激活，不能续期"))
		return
	}
	if failure := s.reauthorizeTerminal(r.Context(), console, session); failure != nil {
		s.failTerminalSession(r.Context(), session, failure.Code)
		writeError(w, failure)
		return
	}
	if failure := s.terminalAuditReady(r.Context()); failure != nil {
		s.failAllSessions(r.Context(), "审计不可用，会话已结束")
		writeError(w, failure)
		return
	}
	token, digest, err := newTerminalToken()
	if err != nil {
		writeError(w, internal(StageConnect, "生成终端令牌失败"))
		return
	}
	session.mu.Lock()
	session.tokenHash = digest
	session.tokenExpires = s.now().Add(TerminalTokenLifetime)
	session.mu.Unlock()
	if err := s.terminal.append(r.Context(), s.terminal.event(session, TerminalEventRenew, ResultAccepted)); err != nil {
		s.failAllSessions(r.Context(), "审计不可用，会话已结束")
		writeError(w, terminalFailure(CodeTerminalAuditUnavailable, StageApply, "终端审计不可用，续期未生效"))
		return
	}
	writeJSON(w, http.StatusOK, terminalCreateResponse{terminalView: s.terminal.view(session), Token: token})
}

// handleTerminalAttach connects one session with a terminal token
// (KOSD-C-91). It is served ahead of the console gate: the token is the only
// accepted credential and the request never falls back to a cookie.
func (s *Server) handleTerminalAttach(w http.ResponseWriter, r *http.Request) {
	session, failure := s.terminalByToken(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if failure := terminalSessionPath(r, session); failure != nil {
		writeError(w, failure)
		return
	}
	if failure := s.checkTerminalBinding(r, session); failure != nil {
		writeError(w, failure)
		return
	}
	if failure := s.checkTerminalToken(r.Context(), session); failure != nil {
		writeError(w, failure)
		return
	}
	session.mu.Lock()
	live := terminalStateIsLive(session.state)
	session.mu.Unlock()
	if !live {
		writeError(w, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "会话已结束，令牌已失效"))
		return
	}
	console, failure := s.consoleForSession(r.Context(), session)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if failure := s.reauthorizeTerminal(r.Context(), console, session); failure != nil {
		s.failTerminalSession(r.Context(), session, failure.Code)
		writeError(w, failure)
		return
	}
	session.mu.Lock()
	channel := session.channel
	session.mu.Unlock()
	if channel == nil {
		opened, failure := s.openTerminalChannel(r.Context(), session)
		if failure != nil {
			writeError(w, failure)
			return
		}
		session.mu.Lock()
		if !terminalStateIsLive(session.state) {
			session.mu.Unlock()
			_, _ = opened.Close(r.Context())
			writeError(w, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "会话已结束"))
			return
		}
		session.channel = opened
		session.mu.Unlock()
	}
	session.mu.Lock()
	if session.state == TerminalStateConnecting {
		session.state = TerminalStateConnected
		// The idle bound starts at the successful attach (KOSD-C-94).
		session.lastActive = s.now()
	}
	session.mu.Unlock()
	writeJSON(w, http.StatusOK, s.terminal.view(session))
}

// handleTerminalInput receives one input batch with a terminal token
// (KOSD-C-92). A duplicate sequence returns the original receipt without
// delivering the batch again.
func (s *Server) handleTerminalInput(w http.ResponseWriter, r *http.Request) {
	session, failure := s.terminalByToken(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if failure := terminalSessionPath(r, session); failure != nil {
		writeError(w, failure)
		return
	}
	if failure := s.checkTerminalBinding(r, session); failure != nil {
		writeError(w, failure)
		return
	}
	if failure := s.checkTerminalToken(r.Context(), session); failure != nil {
		writeError(w, failure)
		return
	}
	console, failure := s.consoleForSession(r.Context(), session)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if failure := s.reauthorizeTerminal(r.Context(), console, session); failure != nil {
		s.failTerminalSession(r.Context(), session, failure.Code)
		writeError(w, failure)
		return
	}
	if failure := s.terminalAuditReady(r.Context()); failure != nil {
		s.failAllSessions(r.Context(), "审计不可用，会话已结束")
		writeError(w, failure)
		return
	}
	var request terminalInputRequest
	if failure := decodeBody(r, true, &request); failure != nil {
		writeError(w, failure)
		return
	}
	if request.Sequence == 0 {
		writeError(w, invalidField("输入序号必须从 1 开始"))
		return
	}
	data, err := base64.StdEncoding.DecodeString(request.Data)
	if err != nil || len(data) == 0 {
		writeError(w, invalidField("输入批次必须是 base64 编码的非空字节"))
		return
	}
	if len(data) > maximumTerminalInputBytes {
		writeError(w, invalidField("输入批次超过 64 KiB"))
		return
	}
	now := s.now()
	session.mu.Lock()
	if session.state == TerminalStateConnected &&
		now.Sub(session.lastActive) >= time.Duration(session.idleMinutes)*time.Minute {
		session.mu.Unlock()
		if err := s.terminal.close(r.Context(), session, TerminalStateClosed, TerminalReasonIdle); err != nil {
			s.log.Error("terminal idle close could not be audited", "session", session.id, "error", err)
		}
		writeError(w, terminalFailure(CodeConflict, StageConnect, "会话已因空闲结束，不再接受输入"))
		return
	}
	if session.state != TerminalStateConnected {
		session.mu.Unlock()
		writeError(w, terminalFailure(CodeConflict, StageConnect, "只有已连接的会话接受输入"))
		return
	}
	if receipt, duplicate := session.receipts[request.Sequence]; duplicate {
		session.mu.Unlock()
		if failure := s.auditDuplicateInput(r, session, request.Sequence, receipt); failure != nil {
			writeError(w, failure)
			return
		}
		writeJSON(w, http.StatusOK, terminalInputResponse{
			Sequence: request.Sequence, Result: receipt.Result, Duplicate: true,
		})
		return
	}
	// A sequence whose delivery is still in flight is already claimed. The
	// second request must not wait for or repeat the Send, and its receipt is
	// unknown until the first delivery reports (KOSD-C-92).
	if session.pending[request.Sequence] {
		session.mu.Unlock()
		if failure := s.auditDuplicateInput(r, session, request.Sequence, TerminalReceipt{Result: TerminalReceiptUnknown}); failure != nil {
			writeError(w, failure)
			return
		}
		writeJSON(w, http.StatusOK, terminalInputResponse{
			Sequence: request.Sequence, Result: TerminalReceiptUnknown, Duplicate: true,
		})
		return
	}
	channel := session.channel
	if channel == nil {
		session.mu.Unlock()
		s.failTerminalSession(r.Context(), session, CodeTerminalExecutorUnavailable)
		writeError(w, terminalFailure(CodeTerminalExecutorUnavailable, StageConnect, "执行服务不可用，会话已结束"))
		return
	}
	// Claim the sequence under the same lock that saw it absent, so a
	// concurrent duplicate cannot reach Send (KOSD-C-92).
	session.pending[request.Sequence] = true
	session.mu.Unlock()
	receipt, err := channel.Send(r.Context(), request.Sequence, data)
	if err != nil {
		// Keep the sequence claimed and record an unknown receipt under the
		// lock that saw it absent: a concurrent duplicate must not reach Send
		// while the failure is being closed (KOSD-C-92).
		session.mu.Lock()
		session.receipts[request.Sequence] = TerminalReceipt{Result: TerminalReceiptUnknown}
		session.mu.Unlock()
		failure := terminalExecutorFailure(err)
		failure.SideEffects = SideEffectsApplied
		failure.Applied = []ObjectRef{session.target.Ref()}
		s.failTerminalSession(r.Context(), session, failure.Code)
		writeError(w, failure)
		return
	}
	if receipt.Result == "" {
		receipt.Result = TerminalReceiptAccepted
	}
	session.mu.Lock()
	delete(session.pending, request.Sequence)
	if !terminalStateIsLive(session.state) {
		session.mu.Unlock()
		writeError(w, terminalFailure(CodeTerminalTargetEnded, StageConnect, "会话已结束，输入未计入"))
		return
	}
	session.receipts[request.Sequence] = receipt
	if receipt.Result == TerminalReceiptAccepted {
		session.lastActive = s.now()
	}
	session.mu.Unlock()
	sequence := request.Sequence
	event := s.terminal.event(session, TerminalEventInput, receipt.Result)
	event.Sequence = &sequence
	event.Receipt = receipt.Result
	if err := s.terminal.append(r.Context(), event); err != nil {
		s.failAllSessions(r.Context(), "审计不可用，会话已结束")
		writeError(w, terminalFailure(CodeTerminalAuditUnavailable, StageApply, "终端审计不可用，会话已结束"))
		return
	}
	writeJSON(w, http.StatusOK, terminalInputResponse{
		Sequence: request.Sequence,
		Result:   receipt.Result,
		Output:   base64.StdEncoding.EncodeToString(receipt.Output),
	})
}

// auditDuplicateInput records a repeated sequence without delivering it again
// (KOSD-C-92). A trail that stops answering ends every session.
func (s *Server) auditDuplicateInput(r *http.Request, session *terminalSession, sequence uint64, receipt TerminalReceipt) *Error {
	event := s.terminal.event(session, TerminalEventInput, TerminalReceiptDuplicate)
	event.Sequence = &sequence
	event.Receipt = receipt.Result
	if err := s.terminal.append(r.Context(), event); err != nil {
		s.failAllSessions(r.Context(), "审计不可用，会话已结束")
		return terminalFailure(CodeTerminalAuditUnavailable, StageApply, "终端审计不可用，重复输入未记录")
	}
	return nil
}

// handleTerminalEnd ends one session and releases its slot (KOSD-C-90,
// KOSD-C-94). A repeated end has no second effect.
func (s *Server) handleTerminalEnd(w http.ResponseWriter, r *http.Request) {
	console, failure := s.terminalConsoleOf(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	session, failure := s.terminalSessionFor(console, r.PathValue("sessionId"))
	if failure != nil {
		writeError(w, failure)
		return
	}
	session.mu.Lock()
	live := terminalStateIsLive(session.state)
	session.mu.Unlock()
	if live {
		if err := s.terminal.close(r.Context(), session, TerminalStateClosed, TerminalReasonEnded); err != nil {
			s.failAllSessions(r.Context(), "审计不可用，会话已结束")
			writeError(w, terminalFailure(CodeTerminalAuditUnavailable, StageApply, "终端审计不可用，结束未能记录"))
			return
		}
	}
	writeJSON(w, http.StatusOK, s.terminal.view(session))
}

// handleTerminalAudit reads one session's audit events (KOSD-C-96). The
// operator reads its own session and the global Owner reads every session;
// any other caller learns nothing, including whether the session exists.
func (s *Server) handleTerminalAudit(w http.ResponseWriter, r *http.Request) {
	console, failure := s.terminalConsoleOf(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if s.terminal.config.Audit == nil {
		writeError(w, terminalFailure(CodeTerminalAuditUnavailable, StageRead, "终端审计不可用"))
		return
	}
	sessionID := r.PathValue("sessionId")
	first, _, err := s.terminal.config.Audit.Read(r.Context(), sessionID, 0, 1)
	if err != nil {
		writeError(w, terminalFailure(CodeTerminalAuditUnavailable, StageRead, "终端审计不可用"))
		return
	}
	if len(first) == 0 || (console.Username != first[0].Actor && console.Role != "Owner") {
		writeError(w, terminalFailure(CodeTerminalForbidden, StageAuthorize, "没有可读的会话审计"))
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	limit := terminalAuditPageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, invalidField("limit 必须是正整数"))
			return
		}
		limit = min(parsed, terminalAuditMaxPage)
	}
	events, next, err := s.terminal.config.Audit.Read(r.Context(), sessionID, after, limit)
	if err != nil {
		writeError(w, terminalFailure(CodeTerminalAuditUnavailable, StageRead, "终端审计不可用"))
		return
	}
	response := terminalAuditResponse{Items: events}
	if next > 0 {
		response.NextCursor = strconv.FormatInt(next, 10)
	}
	writeJSON(w, http.StatusOK, response)
}

// handleTerminalNodes lists the node targets one extension may offer
// (KOSD-C-95). Only the current global Owner reads it; a source failure is
// never answered with an empty list.
func (s *Server) handleTerminalNodes(w http.ResponseWriter, r *http.Request) {
	console, failure := s.terminalConsoleOf(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if console.Role != "Owner" {
		writeError(w, terminalFailure(CodeTerminalForbidden, StageAuthorize, "只有全局 Owner 可以读取节点目标清单"))
		return
	}
	extension := r.URL.Query().Get("extension")
	if extension == "" || !RecordableName(extension) {
		writeError(w, invalidField("必须给出终端组件标识"))
		return
	}
	if s.terminal.config.Extensions == nil || !s.terminal.config.Extensions.IsActive(extension) {
		writeError(w, &Error{
			Code:        CodeCapabilityMissing,
			Message:     "终端组件未激活，不能读取节点清单",
			Stage:       StageCapability,
			SideEffects: SideEffectsNone,
		})
		return
	}
	nodeType := ObjectType{Group: "", Version: "v1", Kind: "Node", Resource: "nodes"}
	observed, err := s.source.List(r.Context(), nodeType, "")
	if err != nil {
		s.log.Error("terminal node inventory read failed", "error", err)
		writeError(w, terminalFailure(CodeTerminalTargetSourceUnavailable, StageRead, "节点目标来源不可用"))
		return
	}
	now := s.now()
	items := make([]terminalNodeTarget, 0, len(observed))
	for _, node := range observed {
		items = append(items, terminalNodeTarget{
			TargetType:          TerminalTargetTypeNode,
			TargetID:            nestedText(node.Object, "metadata", "name"),
			TargetUID:           nestedText(node.Object, "metadata", "uid"),
			Namespace:           "",
			Container:           "",
			ExecutionIdentities: []string{TerminalNodeIdentity},
			ObservedAt:          node.ObservedAt.UTC(),
			Stale:               now.Sub(node.ObservedAt) > StaleThreshold,
		})
	}
	writeJSON(w, http.StatusOK, terminalNodeList{Items: items})
}

// failAllSessions ends every live session, which KOSD-C-92 requires once the
// audit trail stops answering.
func (s *Server) failAllSessions(ctx context.Context, reason string) {
	for _, session := range s.terminal.registry.snapshot() {
		session.mu.Lock()
		live := terminalStateIsLive(session.state)
		session.mu.Unlock()
		if !live {
			continue
		}
		if err := s.terminal.close(ctx, session, TerminalStateFailed, reason); err != nil {
			s.log.Error("terminal session could not be closed after an audit failure", "session", session.id, "error", err)
		}
	}
}

// rejectTerminal records a refused create, so a rejection is as visible as an
// accepted session (KOSD-C-92). A trail that cannot record the refusal is
// itself reported, never hidden.
func (s *Server) rejectTerminal(r *http.Request, console TerminalSession, extension string, target TerminalTarget, failure *Error) {
	event := TerminalEvent{
		Actor:             console.Username,
		Extension:         extension,
		TargetType:        target.Type,
		TargetID:          target.ID,
		TargetUID:         target.UID,
		Namespace:         target.Namespace,
		Container:         target.Container,
		ExecutionIdentity: target.Identity(),
		Event:             TerminalEventReject,
		Result:            terminalRejectResult(failure),
	}
	if err := s.terminal.append(r.Context(), event); err != nil {
		s.log.Error("terminal rejection could not be audited", "code", failure.Code, "error", err)
	}
}

// terminalRejectResult names why a create was refused without leaking the
// target: authorization and existence answer alike (KOSD-C-93).
func terminalRejectResult(failure *Error) string {
	switch failure.Code {
	case CodeTerminalCredentialBoundaryUnavailable, CodeTerminalExecutorUnavailable, CodeTerminalNodeUnreachable:
		return ResultFailed
	}
	return ResultRejected
}

// terminalSessionFor returns the session one console caller may act on. A
// session another account holds answers forbidden (KOSD-C-96).
func (s *Server) terminalSessionFor(console TerminalSession, id string) (*terminalSession, *Error) {
	session, ok := s.terminal.registry.get(id)
	if !ok {
		return nil, terminalFailure(CodeTerminalForbidden, StageAuthorize, "没有这个终端会话")
	}
	if console.Username != session.account && console.Role != "Owner" {
		return nil, terminalFailure(CodeTerminalForbidden, StageAuthorize, "没有这个终端会话")
	}
	return session, nil
}

// consoleForSession rebuilds the console session a token is bound to, so the
// binding survives the token-auth path without trusting a cookie.
func (s *Server) consoleForSession(ctx context.Context, session *terminalSession) (TerminalSession, *Error) {
	if s.terminal.config.Console == nil || session.consoleHash == "" {
		return TerminalSession{}, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "控制台会话已结束")
	}
	current, ok := s.terminal.config.Console.Alive(ctx, session.consoleHash)
	if !ok || current.Username != session.account {
		return TerminalSession{}, terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "控制台会话已结束")
	}
	return current, nil
}

// checkTerminalBinding compares what a token-bound request repeats of its
// binding (KOSD-C-91). A mismatch is forbidden and never falls back to another
// identity. The binding travels in the query string, because the input body is
// the batch itself.
func (s *Server) checkTerminalBinding(r *http.Request, session *terminalSession) *Error {
	query := r.URL.Query()
	check := func(name, actual, expected string) *Error {
		provided := query.Get(name)
		if provided != "" && provided != expected {
			return terminalFailure(CodeTerminalForbidden, StageAuthorize, "令牌与"+name+"不符")
		}
		return nil
	}
	if failure := check("account", query.Get("account"), session.account); failure != nil {
		return failure
	}
	if failure := check("targetType", query.Get("targetType"), session.target.Type); failure != nil {
		return failure
	}
	if failure := check("targetId", query.Get("targetId"), session.target.ID); failure != nil {
		return failure
	}
	if failure := check("targetUid", query.Get("targetUid"), session.target.UID); failure != nil {
		return failure
	}
	if failure := check("namespace", query.Get("namespace"), session.target.Namespace); failure != nil {
		return failure
	}
	if failure := check("container", query.Get("container"), session.target.Container); failure != nil {
		return failure
	}
	if terminalOriginOf(r) != session.origin {
		return terminalFailure(CodeTerminalForbidden, StageAuthorize, "令牌与入口来源不符")
	}
	return nil
}

// checkTerminalToken checks the token's own validity and the console session it
// is bound to (KOSD-C-91).
func (s *Server) checkTerminalToken(ctx context.Context, session *terminalSession) *Error {
	session.mu.Lock()
	hash, expires := session.tokenHash, session.tokenExpires
	session.mu.Unlock()
	if hash == "" || !s.now().Before(expires) {
		s.closeTerminalSession(ctx, session, TerminalStateClosed, TerminalReasonTokenExpired)
		return terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "终端令牌已失效")
	}
	if !s.terminalExtensionActive(session) {
		s.closeTerminalSession(ctx, session, TerminalStateClosed, TerminalReasonUnauthenticated)
		return terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "终端组件未激活，令牌已失效")
	}
	if session.consoleHash != "" && (s.terminal.config.Console == nil ||
		!consoleAlive(ctx, s.terminal.config.Console, session.consoleHash)) {
		s.closeTerminalSession(ctx, session, TerminalStateClosed, TerminalReasonUnauthenticated)
		return terminalFailure(CodeTerminalUnauthenticated, StageAuthorize, "控制台会话已结束")
	}
	return nil
}

// consoleAlive is the token path's console check.
func consoleAlive(ctx context.Context, console TerminalConsole, hash string) bool {
	_, ok := console.Alive(ctx, hash)
	return ok
}

// expireTerminal closes a session whose token or idle bound ran out without a
// renewal (KOSD-C-91, KOSD-C-94).
func (s *Server) expireTerminal(ctx context.Context, session *terminalSession) {
	now := s.now()
	session.mu.Lock()
	if !terminalStateIsLive(session.state) {
		session.mu.Unlock()
		return
	}
	expires := session.tokenExpires
	connected := session.state == TerminalStateConnected
	lastActive := session.lastActive
	idleMinutes := session.idleMinutes
	session.mu.Unlock()
	switch {
	case !expires.IsZero() && !now.Before(expires):
		s.failTerminalSessionBestEffort(session, TerminalStateClosed, TerminalReasonTokenExpired)
	case connected && now.Sub(lastActive) >= time.Duration(idleMinutes)*time.Minute:
		s.failTerminalSessionBestEffort(session, TerminalStateClosed, TerminalReasonIdle)
	}
}

// failTerminalSession ends a session after a failure and records it.
func (s *Server) failTerminalSession(ctx context.Context, session *terminalSession, reason string) {
	if err := s.terminal.close(ctx, session, TerminalStateFailed, reason); err != nil {
		s.log.Error("terminal failure could not be audited", "session", session.id, "error", err)
	}
}

// failTerminalSessionBestEffort is failTerminalSession for a path that carries
// no request to audit on.
func (s *Server) failTerminalSessionBestEffort(session *terminalSession, state, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.terminal.close(ctx, session, state, reason); err != nil {
		s.log.Error("terminal expiration could not be audited", "session", session.id, "error", err)
	}
}

// reauthorizeTerminal re-runs the target judgement before a renewal or a
// reattach (KOSD-C-91): a revoked permission ends the session instead of
// extending it.
func (s *Server) reauthorizeTerminal(ctx context.Context, console TerminalSession, session *terminalSession) *Error {
	resolved, failure := s.resolveTerminalTarget(ctx, session.target)
	if failure != nil {
		return failure
	}
	if failure := s.authorizeTerminalTarget(ctx, console, resolved); failure != nil {
		return failure
	}
	return s.judgeTerminalBoundary(ctx, resolved)
}

// openTerminalChannel opens the executor channel of one attach.
func (s *Server) openTerminalChannel(ctx context.Context, session *terminalSession) (TerminalChannel, *Error) {
	if s.terminal.config.Executor == nil {
		s.failTerminalSession(ctx, session, CodeTerminalExecutorUnavailable)
		return nil, terminalFailure(CodeTerminalExecutorUnavailable, StageConnect, "执行服务不可用，会话已结束")
	}
	channel, err := s.terminal.config.Executor.Open(ctx, session.target, session.identity)
	if err != nil {
		failure := terminalExecutorFailure(err)
		s.failTerminalSession(ctx, session, failure.Code)
		return nil, failure
	}
	return channel, nil
}

// terminalExecutorReady reports whether a create may start: an ended target is
// a conflict, an unreachable node or a missing executor is unavailable
// (KOSD-C-93). It does not open a channel; that happens at attach.
func (s *Server) terminalExecutorReady(ctx context.Context, resolved terminalResolvedTarget) *Error {
	if resolved.ended {
		return terminalFailure(CodeTerminalTargetEnded, StageConnect, "目标已终止，需重新选择")
	}
	if resolved.target.Type == TerminalTargetTypeNode && !resolved.nodeReady {
		return terminalFailure(CodeTerminalNodeUnreachable, StageConnect, "节点失联，暂不能建立会话")
	}
	if s.terminal.config.Executor == nil {
		return terminalFailure(CodeTerminalExecutorUnavailable, StageConnect, "执行服务不可用，暂不能建立会话")
	}
	if failure := s.terminal.config.Executor.Available(ctx); failure != nil {
		failure.Stage = StageConnect
		failure.SideEffects = SideEffectsNone
		return failure
	}
	return nil
}

// terminalExecutorFailure maps an executor error to KOSD-C-93's envelope.
func terminalExecutorFailure(err error) *Error {
	switch {
	case errors.Is(err, ErrTerminalNodeUnreachable):
		return terminalFailure(CodeTerminalNodeUnreachable, StageConnect, "节点失联，会话已结束")
	case errors.Is(err, ErrTerminalTargetEnded):
		return terminalFailure(CodeTerminalTargetEnded, StageConnect, "目标已终止，需重新选择")
	case errors.Is(err, ErrTerminalCredentialBoundary):
		return terminalFailure(CodeTerminalCredentialBoundaryUnavailable, StageConnect, "无法保证凭据隔离，已拒绝连接")
	default:
		return terminalFailure(CodeTerminalExecutorUnavailable, StageConnect, "执行服务不可用，会话已结束")
	}
}

// terminalTargetOf validates the submitted target fields (KOSD-C-95).
func terminalTargetOf(request terminalCreateRequest) (TerminalTarget, *Error) {
	target := TerminalTarget{
		Type:      strings.ToLower(strings.TrimSpace(request.TargetType)),
		ID:        strings.TrimSpace(request.TargetID),
		UID:       strings.TrimSpace(request.TargetUID),
		Namespace: strings.TrimSpace(request.Namespace),
		Container: strings.TrimSpace(request.Container),
	}
	switch target.Type {
	case TerminalTargetTypeNode:
		if target.Namespace != "" || target.Container != "" {
			return TerminalTarget{}, invalidField("节点目标的命名空间与容器必须是空字符串")
		}
	case TerminalTargetTypePod:
		if target.Namespace == "" || target.Container == "" {
			return TerminalTarget{}, invalidField("Pod 目标必须给出命名空间与容器")
		}
	default:
		return TerminalTarget{}, invalidField("目标类型只能是 node 或 pod")
	}
	if !RecordableName(target.ID) {
		return TerminalTarget{}, invalidField("目标名称不合法")
	}
	if target.UID == "" || len(target.UID) > 128 {
		return TerminalTarget{}, invalidField("必须给出目标对象的 metadata.uid")
	}
	return target, nil
}

// terminalOriginOf reads the entry origin a browser sends (KOSD-C-91).
func terminalOriginOf(r *http.Request) string {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if len(origin) > 512 {
		origin = origin[:512]
	}
	return origin
}

// bearerOf reads one bearer token; no cookie is consulted (KOSD-C-91).
func bearerOf(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
