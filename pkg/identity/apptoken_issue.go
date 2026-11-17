package identity

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/tsic404/kosd/pkg/access"
	"github.com/tsic404/kosd/pkg/apptoken"
	"github.com/tsic404/kosd/pkg/controlplane"
)

// AppTokenIssuer mints the token one window holds.
//
// The control plane owns the token table and writes it (the project's data
// ownership puts application tokens in the control-plane database, written by
// the account layer), so a token the host hands to a window is one the control
// plane can verify itself: the host relays it and nothing else, and the
// control plane remains the only point that judges it (KOSD-C-75, KOSD-C-86).
// The binding is taken from the extension declaration and from the account's
// readable range at issue time, never from the caller's claim, so a host cannot
// widen a token past what the declaration and the account allow (KOSD-C-85).
type AppTokenIssuer struct {
	service *Service
	tokens  *AppTokens
	policy  *access.Policy
}

// NewAppTokenIssuer builds the minter over the account service, the token
// table and the authorization rule set. A nil policy or token table leaves the
// mint routes unconfigured.
func NewAppTokenIssuer(service *Service, tokens *AppTokens, policy *access.Policy) *AppTokenIssuer {
	return &AppTokenIssuer{service: service, tokens: tokens, policy: policy}
}

// WindowTokenRequest names the window one console session opens a token for.
type WindowTokenRequest struct {
	// Application is the application identifier the declaration carries.
	Application string
	// Extension is the identifier of the extension the application belongs to.
	Extension string
	// WindowID is the window instance the token is bound to.
	WindowID string
	// Origin is the entry origin the window was opened from, as the host saw it.
	Origin string
}

// WindowToken is one minted token: the value to hand to the window, and the
// deadline it stops being usable at.
type WindowToken struct {
	// Token is the value; it is returned once and never stored in clear.
	Token string
	// ExpiresAt is the deadline the binding carries.
	ExpiresAt time.Time
}

// Issue mints one token for the window a console session opens.
func (i *AppTokenIssuer) Issue(ctx context.Context, session Session, request WindowTokenRequest) (WindowToken, error) {
	switch {
	case request.Application == "" || request.Extension == "" || request.WindowID == "":
		return WindowToken{}, invalid("窗口令牌请求需要应用、扩展与窗口实例")
	case utf8.RuneCountInString(request.WindowID) > MaxUsernameLength:
		return WindowToken{}, invalid("窗口实例标识过长")
	case utf8.RuneCountInString(request.Application) > MaxUsernameLength ||
		utf8.RuneCountInString(request.Extension) > MaxUsernameLength:
		return WindowToken{}, invalid("应用或扩展标识过长")
	case session.Username == "":
		return WindowToken{}, ErrUnauthenticated
	}
	if i.tokens == nil || i.policy == nil || i.policy.Extensions == nil {
		return WindowToken{}, fmt.Errorf("%w: the window token issuer is not configured", ErrServiceUnavailable)
	}
	declared, ok := i.policy.Extensions.AppOrigin(request.Extension, request.Application)
	if !ok || declared == "" {
		// The application is not installed under this extension, so the host has
		// no window to open a token for.
		return WindowToken{}, ErrPermissionDenied
	}
	// The origin the window was opened from is the declared entry origin; both
	// sides are read in the browser's serialization, so an explicit default port
	// in the declaration does not make a correct window foreign (KOSD-C-87).
	if apptoken.NormalizeOrigin(request.Origin) != apptoken.NormalizeOrigin(declared) {
		return WindowToken{}, ErrPermissionDenied
	}
	grants, objects, err := i.policy.AppReadScope(ctx,
		access.Session{Username: session.Username, Role: session.Role}, request.Extension)
	if err != nil {
		if errors.Is(err, controlplane.ErrUnavailable) {
			return WindowToken{}, fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
		}
		return WindowToken{}, ErrUnavailable
	}
	value, expiresAt, err := i.tokens.Issue(ctx, apptoken.Binding{
		AppID:     request.Application,
		Extension: request.Extension,
		Account:   session.Username,
		WindowID:  request.WindowID,
		Origin:    declared,
		Grants:    grants,
		Objects:   objects,
	})
	if err != nil {
		return WindowToken{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return WindowToken{Token: value, ExpiresAt: expiresAt}, nil
}

// RevokeWindow ends one window's token (KOSD-C-75).
func (i *AppTokenIssuer) RevokeWindow(ctx context.Context, account, windowID string) error {
	if i.tokens == nil {
		return nil
	}
	return i.tokens.RevokeWindow(ctx, account, windowID)
}
