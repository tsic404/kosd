package apptoken

import (
	"context"
	"errors"
	"testing"
	"time"
)

// binding returns a usable binding on a fixed clock.
func binding() Binding {
	return Binding{
		AppID:     "shop",
		Extension: "demo.widgets",
		Account:   "member",
		WindowID:  "window-1",
		Origin:    "https://shop.example.test",
		Grants:    []Grant{{Group: "kos.tsic.top", Kind: "Application"}},
	}
}

// TestIssueVerifyRoundTrip proves a minted value resolves to the binding it
// was issued with, and that a second mint is a different value.
func TestIssueVerifyRoundTrip(t *testing.T) {
	store := NewStore(nil)
	value, err := store.Issue(binding())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(value) != 43 {
		t.Fatalf("token length = %d, want 43 characters of base64url entropy", len(value))
	}
	resolved, err := store.Verify(context.Background(), value)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if resolved.AppID != "shop" || resolved.Account != "member" || resolved.WindowID != "window-1" {
		t.Fatalf("resolved = %+v, want the issued binding", resolved)
	}
	if len(resolved.Grants) != 1 || !resolved.Grants[0].Covers("kos.tsic.top", "Application") {
		t.Fatalf("grants = %+v, want the issued grant", resolved.Grants)
	}
	if resolved.ExpiresAt.IsZero() || !resolved.ExpiresAt.After(time.Now()) {
		t.Fatalf("expiry = %v, want the default TTL in the future", resolved.ExpiresAt)
	}
	other, err := store.Issue(binding())
	if err != nil {
		t.Fatalf("issue again: %v", err)
	}
	if other == value {
		t.Fatal("two issued tokens share one value")
	}
}

// TestVerifyRefusesUnknownExpiredAndUnavailable separates the two failure
// classes a verifier reports (KOSD-C-86).
func TestVerifyRefusesUnknownExpiredAndUnavailable(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	store := NewStore(func() time.Time { return now })
	if _, err := store.Verify(context.Background(), "unknown"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown token error = %v, want ErrInvalid", err)
	}
	expired := binding()
	expired.ExpiresAt = now.Add(-time.Second)
	value, err := store.Issue(expired)
	if err != nil {
		t.Fatalf("issue expired: %v", err)
	}
	if _, err := store.Verify(context.Background(), value); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired token error = %v, want ErrInvalid", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Verify(cancelled, "any"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled verification error = %v, want ErrUnavailable", err)
	}
}

// TestRevokeWindowIsScopedToOneAccount proves closing one window ends only that
// window's token (KOSD-C-75).
func TestRevokeWindowIsScopedToOneAccount(t *testing.T) {
	store := NewStore(nil)
	first, err := store.Issue(binding())
	if err != nil {
		t.Fatalf("issue first: %v", err)
	}
	otherAccount := binding()
	otherAccount.Account = "other"
	second, err := store.Issue(otherAccount)
	if err != nil {
		t.Fatalf("issue second: %v", err)
	}
	store.RevokeWindow("member", "window-1")
	if _, err := store.Verify(context.Background(), first); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revoked window token error = %v, want ErrInvalid", err)
	}
	if _, err := store.Verify(context.Background(), second); err != nil {
		t.Fatalf("another account's token was revoked: %v", err)
	}
}

// TestRevokeAccountAndAppEndEveryMatch proves sign-out and uninstall revoke the
// tokens they own.
func TestRevokeAccountAndAppEndEveryMatch(t *testing.T) {
	store := NewStore(nil)
	first, _ := store.Issue(binding())
	sameAccount := binding()
	sameAccount.WindowID = "window-2"
	second, _ := store.Issue(sameAccount)
	store.RevokeAccount("member")
	for _, value := range []string{first, second} {
		if _, err := store.Verify(context.Background(), value); !errors.Is(err, ErrInvalid) {
			t.Fatalf("token after RevokeAccount error = %v, want ErrInvalid", err)
		}
	}
	third, _ := store.Issue(binding())
	store.RevokeApp("shop")
	if _, err := store.Verify(context.Background(), third); !errors.Is(err, ErrInvalid) {
		t.Fatalf("token after RevokeApp error = %v, want ErrInvalid", err)
	}
}

// TestGrantCoversKinds proves the empty kind is the group-wide form and a named
// kind never widens to its group.
func TestGrantCoversKinds(t *testing.T) {
	groupOnly := Grant{Group: "kos.tsic.top"}
	if !groupOnly.Covers("kos.tsic.top", "Application") || groupOnly.Covers("widgets.example.test", "Widget") {
		t.Fatalf("group-only grant covers the wrong set")
	}
	named := Grant{Group: "widgets.example.test", Kind: "Widget"}
	if !named.Covers("widgets.example.test", "Widget") || named.Covers("widgets.example.test", "Gadget") {
		t.Fatalf("named grant covers the wrong set")
	}
}

// TestNormalizeOriginFollowsBrowserSerialization pins the origin form a
// browser sends: the scheme's default port is omitted, a non-default port is
// kept, and scheme and host are lower-cased (KOSD-C-87).
func TestNormalizeOriginFollowsBrowserSerialization(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{"https://shop.example.test:443/index.html", "https://shop.example.test"},
		{"https://shop.example.test/index.html", "https://shop.example.test"},
		{"https://shop.example.test:443", "https://shop.example.test"},
		{"http://shop.example.test:80/app", "http://shop.example.test"},
		{"https://Shop.Example.Test:443/app", "https://shop.example.test"},
		{"https://shop.example.test:8443/index.html", "https://shop.example.test:8443"},
		{"http://shop.example.test:8080", "http://shop.example.test:8080"},
		{"https://[::1]:443/app", "https://[::1]"},
		{"", ""},
		{"not an origin", ""},
		{"/relative/path", ""},
	}
	for _, testCase := range cases {
		if got := NormalizeOrigin(testCase.raw); got != testCase.want {
			t.Errorf("NormalizeOrigin(%q) = %q, want %q", testCase.raw, got, testCase.want)
		}
	}
}
