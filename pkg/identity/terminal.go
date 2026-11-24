package identity

import (
	"context"
	"time"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/write"
)

// TerminalSessions adapts the account service to the terminal surface: the
// console session behind a request, the same session by digest for the
// token-only paths, and a context the authorization seam can judge (KOSD-C-91).
type TerminalSessions struct {
	// Service resolves the session behind a gated request.
	Service *Service
	// Store reads a session by digest on the token-only paths.
	Store *Store
}

// Session implements controlplane.TerminalConsole.
func (t TerminalSessions) Session(ctx context.Context) (controlplane.TerminalSession, bool) {
	session, ok := FromContext(ctx)
	if !ok || session.Username == "" || t.Service == nil {
		return controlplane.TerminalSession{}, false
	}
	return terminalSessionOf(session), true
}

// Alive implements controlplane.TerminalConsole. A session whose account was
// disabled, that expired, or that a password change revoked is not alive.
func (t TerminalSessions) Alive(ctx context.Context, consoleHash string) (controlplane.TerminalSession, bool) {
	if t.Store == nil || consoleHash == "" {
		return controlplane.TerminalSession{}, false
	}
	stored, err := t.Store.Session(ctx, consoleHash)
	if err != nil {
		return controlplane.TerminalSession{}, false
	}
	now := t.Store.Now()
	if stored.Disabled || stored.MustChangePassword ||
		!now.Before(stored.IdleExpiresAt) || !now.Before(stored.AbsoluteExpiresAt) {
		return controlplane.TerminalSession{}, false
	}
	return controlplane.TerminalSession{
		Username:    stored.Username,
		Role:        string(stored.Role),
		ConsoleHash: stored.TokenHash,
		ExpiresAt:   earlier(stored.IdleExpiresAt, stored.AbsoluteExpiresAt),
	}, true
}

// WithSession implements controlplane.TerminalConsole: the authorization seam
// reads the same account and role from the context it would see on a gated
// request.
func (t TerminalSessions) WithSession(ctx context.Context, session controlplane.TerminalSession) context.Context {
	return withSession(ctx, Session{
		Username: session.Username,
		Role:     write.Role(session.Role),
	})
}

// terminalSessionOf renders one resolved console session.
func terminalSessionOf(session Session) controlplane.TerminalSession {
	return controlplane.TerminalSession{
		Username:    session.Username,
		Role:        string(session.Role),
		ConsoleHash: session.TokenHash(),
		ExpiresAt:   session.ExpiresAt,
	}
}

// earlier picks the deadline that comes first.
func earlier(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

// TerminalAuditTrail stores terminal events beside the control-plane audit
// trail, so they are exported and restored with the account database
// (KOSD-C-92, KOSD-C-96).
type TerminalAuditTrail struct {
	// Store is the control-plane database; required.
	Store *Store
}

// Append implements controlplane.TerminalAudit.
func (t TerminalAuditTrail) Append(ctx context.Context, event controlplane.TerminalEvent) error {
	return t.Store.AppendTerminalEvent(ctx, event)
}

// Read implements controlplane.TerminalAudit.
func (t TerminalAuditTrail) Read(ctx context.Context, sessionID string, after int64, limit int) ([]controlplane.TerminalEvent, int64, error) {
	return t.Store.TerminalEvents(ctx, sessionID, after, limit)
}

// Available implements controlplane.TerminalAudit.
func (t TerminalAuditTrail) Available(ctx context.Context) error {
	return t.Store.Ping(ctx)
}
