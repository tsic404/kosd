package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tsic404/kosd/pkg/apptoken"
	"time"
)

// appTokenEntropy is the entropy of one application token. It is the contract's
// entropy and the value the host hands to the window once.
const appTokenEntropy = 32

// InstalledApps resolves the part of an extension declaration an application
// token needs: the entry origin one installed application declares. The
// extension registry implements it (KOSD-C-87).
type InstalledApps interface {
	// AppOrigin returns the entry origin of one installed application, and
	// whether the application is installed at all.
	AppOrigin(extension, appID string) (string, bool)
}

// AppTokens is the control plane's application-token table (KOSD-C-75,
// KOSD-C-86): the account layer writes it when a window opens and the account
// gate reads it when a request arrives. Only the digest of a token value is
// kept, and an entry whose window, account or application has ended, or whose
// deadline has passed, stops resolving.
//
// It holds its own handle to the same database file, so an unusable token
// table is answered as token validity being undetermined rather than as the
// account database being unavailable, which keeps the three failure classes
// distinct (KOSD-C-86).
type AppTokens struct {
	db        *sql.DB
	now       func() time.Time
	installed InstalledApps
}

// NewAppTokens builds the token table over the control-plane database. A nil
// installed resolver keeps every issued token resolvable until it is revoked or
// expires. The handle it opens must be released with Close.
func NewAppTokens(store *Store, installed InstalledApps) (*AppTokens, error) {
	db, err := sql.Open("sqlite", writableDSN(store.Path()))
	if err != nil {
		return nil, fmt.Errorf("identity: open the application-token table: %w", err)
	}
	return &AppTokens{db: db, now: store.Now, installed: installed}, nil
}

// Close releases the token table's handle.
func (a *AppTokens) Close() error {
	return a.db.Close()
}

// Issue mints one token for a binding and returns the value the window receives.
// A window that already holds a token loses it, so one window has exactly one
// live token, whatever its host does with the handshake (KOSD-C-75).
func (a *AppTokens) Issue(ctx context.Context, binding apptoken.Binding) (string, time.Time, error) {
	raw := make([]byte, appTokenEntropy)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("identity: draw an application token: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(raw)
	// The table's clock sets the deadline, so a token's lifetime is read from
	// the one clock that judges it when a request arrives.
	expiresAt := binding.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = a.now().Add(apptoken.DefaultTTL)
	}
	binding.ExpiresAt = expiresAt
	// The binding records the same serialized origin the browser will send, so a
	// declaration or a window that writes an explicit default port still matches
	// (KOSD-C-87).
	binding.Origin = apptoken.NormalizeOrigin(binding.Origin)
	binding.Grants = append([]apptoken.Grant(nil), binding.Grants...)
	binding.Objects = append([]apptoken.Object(nil), binding.Objects...)
	payload, err := json.Marshal(binding)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: encode an application token: %w", err)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: begin the application-token write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM app_tokens WHERE account = ? AND window_id = ?`,
		binding.Account, binding.WindowID); err != nil {
		return "", time.Time{}, fmt.Errorf("identity: clear the window's application token: %w", err)
	}
	now := a.now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO app_tokens
		(digest, account, app_id, extension, window_id, binding, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		appTokenDigest(value), binding.Account, binding.AppID, binding.Extension,
		binding.WindowID, string(payload), toMillis(now), toMillis(binding.ExpiresAt)); err != nil {
		return "", time.Time{}, fmt.Errorf("identity: store the application token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", time.Time{}, fmt.Errorf("identity: store the application token: %w", err)
	}
	return value, expiresAt, nil
}

// Verify implements apptoken.Verifier. A token no live row resolves, a token
// whose window, account, application or deadline is gone, and a token the
// application no longer declares all answer as invalid; only an unreadable
// table answers as unavailable (KOSD-C-86).
func (a *AppTokens) Verify(ctx context.Context, value string) (apptoken.Binding, error) {
	if err := ctx.Err(); err != nil {
		return apptoken.Binding{}, fmt.Errorf("%w: %v", apptoken.ErrUnavailable, err)
	}
	if value == "" {
		return apptoken.Binding{}, apptoken.ErrInvalid
	}
	digest := appTokenDigest(value)
	var (
		payload   string
		expiresAt int64
	)
	row := a.db.QueryRowContext(ctx, `SELECT binding, expires_at FROM app_tokens WHERE digest = ?`, digest)
	switch err := row.Scan(&payload, &expiresAt); {
	case errors.Is(err, sql.ErrNoRows):
		return apptoken.Binding{}, apptoken.ErrInvalid
	case err != nil:
		return apptoken.Binding{}, fmt.Errorf("%w: %v", apptoken.ErrUnavailable, err)
	}
	if !a.now().Before(fromMillis(expiresAt)) {
		// Drop it on the way out so a token that expired while the table was
		// unreadable does not stay resolvable.
		_, _ = a.db.ExecContext(ctx, `DELETE FROM app_tokens WHERE digest = ?`, digest)
		return apptoken.Binding{}, apptoken.ErrInvalid
	}
	var binding apptoken.Binding
	if err := json.Unmarshal([]byte(payload), &binding); err != nil {
		return apptoken.Binding{}, fmt.Errorf("%w: the stored binding is unreadable: %v", apptoken.ErrUnavailable, err)
	}
	if a.installed != nil {
		if _, ok := a.installed.AppOrigin(binding.Extension, binding.AppID); !ok {
			// The application was uninstalled or its declaration was withdrawn, so
			// the window it opened is gone.
			return apptoken.Binding{}, apptoken.ErrInvalid
		}
	}
	return binding, nil
}

// RevokeWindow ends one window's token, which is what closing the window does
// (KOSD-C-75). The account argument keeps one window's close from ending a
// token another account holds.
func (a *AppTokens) RevokeWindow(ctx context.Context, account, windowID string) error {
	return a.revoke(ctx, `DELETE FROM app_tokens WHERE account = ? AND window_id = ?`, account, windowID)
}

// RevokeAccount ends every token one account holds, which is what a sign-out
// does (KOSD-C-75).
func (a *AppTokens) RevokeAccount(ctx context.Context, account string) error {
	return a.revoke(ctx, `DELETE FROM app_tokens WHERE account = ?`, account)
}

// RevokeApp ends every token one application holds, which is what an uninstall
// does (KOSD-C-75).
func (a *AppTokens) RevokeApp(ctx context.Context, appID string) error {
	return a.revoke(ctx, `DELETE FROM app_tokens WHERE app_id = ?`, appID)
}

// revoke applies one revocation statement and names the table when it fails.
func (a *AppTokens) revoke(ctx context.Context, statement string, args ...any) error {
	if _, err := a.db.ExecContext(ctx, statement, args...); err != nil {
		return fmt.Errorf("identity: revoke an application token: %w", err)
	}
	return nil
}

// appTokenDigest is the stored form of one token value: the token is never
// stored in clear (KOSD-C-14).
func appTokenDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
