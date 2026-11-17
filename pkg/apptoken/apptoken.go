// Package apptoken carries the window application token model the control
// plane verifies (KOSD-C-75, KOSD-C-86). A token is an opaque random value
// delivered once at the handshake; only its digest is stored, and the binding
// records the account, application, window instance, read authorization and
// expiry the token was issued with.
package apptoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// tokenBytes is the entropy of one application token (KOSD-C-75).
	tokenBytes = 32
	// DefaultTTL is how long a token stays usable (KOSD-C-75).
	DefaultTTL = 15 * time.Minute
)

// Failures a verifier distinguishes (KOSD-C-86). A token that cannot be
// resolved is unauthenticated; a verifier that cannot answer is unavailable.
var (
	// ErrInvalid marks a missing, forged, expired or revoked token.
	ErrInvalid = errors.New("apptoken: the token is missing, forged, expired or revoked")
	// ErrUnavailable marks a token validity that cannot be determined right
	// now, which the control plane answers as service unavailable.
	ErrUnavailable = errors.New("apptoken: the token validity cannot be determined")
)

// NormalizeOrigin renders an origin the way a browser serializes one
// (KOSD-C-87): lower-case scheme and host, and the scheme's default port
// omitted. A declaration, an issued binding and a request Origin then compare
// equal exactly when they name the same scheme, host and effective port.
// An entry that names no usable origin returns the empty string.
func NormalizeOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := parsed.Port()
	if port == "" || isDefaultPort(scheme, port) {
		return scheme + "://" + host
	}
	return scheme + "://" + host + ":" + port
}

// isDefaultPort reports whether a port is the scheme's default, which a
// browser omits when it serializes an origin.
func isDefaultPort(scheme, port string) bool {
	switch scheme {
	case "http", "ws":
		return port == "80"
	case "https", "wss":
		return port == "443"
	default:
		return false
	}
}

// Grant is one API group and kind a token may read (KOSD-C-85). An empty kind
// covers every kind of the group.
type Grant struct {
	// Group is the API group, empty for the core group.
	Group string `json:"group,omitempty"`
	// Kind is the object kind, empty for every kind of the group.
	Kind string `json:"kind,omitempty"`
}

// Object names one object a token may read. It is the issue-time half of the
// C-85 intersection for classes whose visibility is decided per object: a
// class grant never widens to objects the account could not read when the
// token was minted, however the account's permissions move afterwards.
type Object struct {
	// Group is the API group, empty for the core group.
	Group string `json:"group,omitempty"`
	// Kind is the object kind.
	Kind string `json:"kind"`
	// Namespace is the object namespace; empty for a cluster-scoped object.
	Namespace string `json:"namespace,omitempty"`
	// Name is the object name.
	Name string `json:"name"`
}

// Matches reports whether one object names the same object as ref.
func (o Object) Matches(group, kind, namespace, name string) bool {
	return o.Group == group && o.Kind == kind && o.Namespace == namespace && o.Name == name
}

// Covers reports whether one grant admits a group and kind.
func (g Grant) Covers(group, kind string) bool {
	if g.Group != group {
		return false
	}
	return g.Kind == "" || g.Kind == kind
}

// Binding is the identity one token carries (KOSD-C-75): who opened which
// window of which application, what it may read, and until when.
type Binding struct {
	// AppID is the application the token belongs to.
	AppID string `json:"appId"`
	// Extension is the extension identifier the application belongs to.
	Extension string `json:"extension"`
	// Account is the account that opened the window.
	Account string `json:"account"`
	// WindowID is the window instance the token is bound to.
	WindowID string `json:"windowId"`
	// Origin is the application entry origin recorded at issue time.
	Origin string `json:"origin"`
	// Grants is the read authorization recorded when the token was issued: the
	// intersection of the declared read scope and the account's readable
	// objects at that moment (KOSD-C-85). It is an upper bound, never a
	// replacement for the current checks.
	Grants []Grant `json:"grants,omitempty"`
	// Objects is the issue-time readable range for classes whose visibility is
	// decided per object (KOSD-C-85). A class named by Grants but absent here
	// admits no object of that class: the issuer records what it could see, so
	// a later grant cannot widen an old token.
	Objects []Object `json:"objects,omitempty"`
	// Permissions are the capabilities the application declared (KOSD-C-75).
	Permissions []string `json:"permissions,omitempty"`
	// ExpiresAt is the moment the token stops being usable.
	ExpiresAt time.Time `json:"expiresAt"`
}

// Verifier resolves one token value to the binding it carries. The window host
// that minted the token is its only issuer, so it is also the only verifier
// (KOSD-C-75, KOSD-C-86).
type Verifier interface {
	Verify(ctx context.Context, value string) (Binding, error)
}

// Store is the token table a host fills when it mints a token and the control
// plane reads when it judges one. It keeps only the digest of a token value
// (KOSD-C-14) and drops entries at their expiry.
type Store struct {
	mu     sync.Mutex
	now    func() time.Time
	ttl    time.Duration
	tokens map[[sha256.Size]byte]Binding
}

// NewStore builds a token table. A nil clock reads the wall clock, and a
// non-positive ttl takes the contract's fifteen minutes.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		now:    now,
		ttl:    DefaultTTL,
		tokens: map[[sha256.Size]byte]Binding{},
	}
}

// Issue mints one token for a binding and returns the value to deliver. The
// caller stores nothing: the table keeps the digest only.
func (s *Store) Issue(binding Binding) (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("apptoken: draw a token: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(raw)
	if binding.ExpiresAt.IsZero() {
		binding.ExpiresAt = s.now().Add(s.ttl)
	}
	// The binding records the same serialized origin the browser will send, so
	// an entry declared with an explicit default port still matches (KOSD-C-87).
	binding.Origin = NormalizeOrigin(binding.Origin)
	binding.Grants = append([]Grant(nil), binding.Grants...)
	binding.Objects = append([]Object(nil), binding.Objects...)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.tokens[sha256.Sum256([]byte(value))] = binding
	return value, nil
}

// Verify implements Verifier.
func (s *Store) Verify(ctx context.Context, value string) (Binding, error) {
	if err := ctx.Err(); err != nil {
		return Binding{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	binding, found := s.tokens[sha256.Sum256([]byte(value))]
	if !found {
		return Binding{}, ErrInvalid
	}
	binding.Grants = append([]Grant(nil), binding.Grants...)
	binding.Objects = append([]Object(nil), binding.Objects...)
	return binding, nil
}

// RevokeWindow ends one window's token, which is what closing the window does
// (KOSD-C-75). The account argument keeps one window's close from revoking a
// token another account holds.
func (s *Store) RevokeWindow(account, windowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for digest, binding := range s.tokens {
		if binding.WindowID == windowID && binding.Account == account {
			delete(s.tokens, digest)
		}
	}
}

// RevokeAccount ends every token one account holds, which is what a sign-out
// does (KOSD-C-75).
func (s *Store) RevokeAccount(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for digest, binding := range s.tokens {
		if binding.Account == account {
			delete(s.tokens, digest)
		}
	}
}

// RevokeApp ends every token one application holds, which is what an uninstall
// does (KOSD-C-75).
func (s *Store) RevokeApp(appID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for digest, binding := range s.tokens {
		if binding.AppID == appID {
			delete(s.tokens, digest)
		}
	}
}

// pruneLocked drops expired entries so a revoked-by-expiry token answers as
// invalid rather than staying resolvable.
func (s *Store) pruneLocked() {
	now := s.now()
	for digest, binding := range s.tokens {
		if !now.Before(binding.ExpiresAt) {
			delete(s.tokens, digest)
		}
	}
}
