package identity

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsic404/kosd/pkg/access"
	"github.com/tsic404/kosd/pkg/apptoken"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/write"
)

// openTokenTable builds the control-plane database and the application-token
// table on it, which is the pair the production entry wires.
func openTokenTable(t *testing.T, clock *testClock, installed InstalledApps) (*Store, *AppTokens) {
	t.Helper()
	store, err := Open(context.Background(), OpenOptions{
		Path: filepath.Join(t.TempDir(), "kos.db"),
		Now:  clock.Now,
	})
	if err != nil {
		t.Fatalf("open the control-plane database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tokens, err := NewAppTokens(store, installed)
	if err != nil {
		t.Fatalf("open the application-token table: %v", err)
	}
	t.Cleanup(func() { _ = tokens.Close() })
	return store, tokens
}

// fixedDeclarations answers the extension seam from a fixed declaration.
type fixedDeclarations struct {
	reads []access.DeclaredRead
	apps  map[string]string
}

// ObjectWriters implements access.ExtensionRoles.
func (f fixedDeclarations) ObjectWriters(string, string) ([]write.Role, bool) { return nil, false }

// DeclaredBy implements access.ExtensionRoles.
func (f fixedDeclarations) DeclaredBy(string, string) (string, bool) { return "", false }

// ReadScopes implements access.ExtensionRoles.
func (f fixedDeclarations) ReadScopes(string) []access.DeclaredRead { return f.reads }

// AppOrigin implements access.ExtensionRoles.
func (f fixedDeclarations) AppOrigin(_, appID string) (string, bool) {
	origin, ok := f.apps[appID]
	return origin, ok
}

// fixedObjects answers kernel-object reads from a fixed list.
type fixedObjects struct {
	applications []map[string]any
}

// List implements controlplane.ObjectSource.
func (f fixedObjects) List(context.Context, controlplane.ObjectType, string) ([]controlplane.ObservedObject, error) {
	objects := make([]controlplane.ObservedObject, 0, len(f.applications))
	for _, application := range f.applications {
		objects = append(objects, controlplane.ObservedObject{Object: application})
	}
	return objects, nil
}

// Get implements controlplane.ObjectSource.
func (f fixedObjects) Get(_ context.Context, _ controlplane.ObjectType, namespace, name string) (controlplane.ObservedObject, error) {
	for _, application := range f.applications {
		metadata, _ := application["metadata"].(map[string]any)
		if metadata["name"] == name && metadata["namespace"] == namespace {
			return controlplane.ObservedObject{Object: application}, nil
		}
	}
	return controlplane.ObservedObject{}, errors.New("not found")
}

// applicationObject is one readable application object the fixture serves.
func applicationObject(name, owner string, members ...string) map[string]any {
	entries := make([]any, 0, len(members))
	for _, member := range members {
		entries = append(entries, member)
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"owner":      owner,
			"visibility": map[string]any{"members": entries, "allowGuests": false},
		},
	}
}

// TestAppTokensIssueVerifyRevoke walks the table's own life: one token resolves
// once, and each end the contract names ends it.
func TestAppTokensIssueVerifyRevoke(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	_, tokens := openTokenTable(t, clock, nil)
	binding := apptoken.Binding{
		AppID: "shop", Extension: "demo.widgets", Account: "owner", WindowID: "w1",
		Origin: "https://shop.example.test:443",
		Grants: []apptoken.Grant{{Group: "kos.tsic.top", Kind: "Application"}},
	}
	value, _, err := tokens.Issue(ctx, binding)
	if err != nil {
		t.Fatalf("issue a token: %v", err)
	}
	resolved, err := tokens.Verify(ctx, value)
	if err != nil {
		t.Fatalf("verify the token: %v", err)
	}
	// An explicit default port is read in the browser's serialization (KOSD-C-87).
	if want := "https://shop.example.test"; resolved.Origin != want {
		t.Fatalf("binding origin = %q, want %q", resolved.Origin, want)
	}
	if len(resolved.Grants) != 1 || resolved.Grants[0].Kind != "Application" {
		t.Fatalf("binding grants = %+v, want the issued one", resolved.Grants)
	}
	if !resolved.ExpiresAt.Equal(clock.Now().Add(apptoken.DefaultTTL)) {
		t.Fatalf("deadline = %v, want the issue time plus the default ttl", resolved.ExpiresAt)
	}
	if _, err := tokens.Verify(ctx, "forged"); !errors.Is(err, apptoken.ErrInvalid) {
		t.Fatalf("a forged token = %v, want invalid", err)
	}
	if err := tokens.RevokeWindow(ctx, "owner", "w1"); err != nil {
		t.Fatalf("revoke the window: %v", err)
	}
	if _, err := tokens.Verify(ctx, value); !errors.Is(err, apptoken.ErrInvalid) {
		t.Fatalf("a closed window's token = %v, want invalid", err)
	}

	second, _, err := tokens.Issue(ctx, apptoken.Binding{AppID: "shop", Extension: "demo.widgets", Account: "owner", WindowID: "w2"})
	if err != nil {
		t.Fatalf("issue a second window's token: %v", err)
	}
	if err := tokens.RevokeAccount(ctx, "owner"); err != nil {
		t.Fatalf("revoke the account: %v", err)
	}
	if _, err := tokens.Verify(ctx, second); !errors.Is(err, apptoken.ErrInvalid) {
		t.Fatalf("a signed-out account's token = %v, want invalid", err)
	}
	third, _, err := tokens.Issue(ctx, apptoken.Binding{AppID: "shop", Extension: "demo.widgets", Account: "shopper", WindowID: "w3"})
	if err != nil {
		t.Fatalf("issue an account that stays signed in: %v", err)
	}
	if err := tokens.RevokeApp(ctx, "shop"); err != nil {
		t.Fatalf("revoke the application: %v", err)
	}
	if _, err := tokens.Verify(ctx, third); !errors.Is(err, apptoken.ErrInvalid) {
		t.Fatalf("an uninstalled application's token = %v, want invalid", err)
	}
}

// TestAppTokensExpire pins the deadline: a token past it answers as unknown
// instead of staying resolvable.
func TestAppTokensExpire(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	_, tokens := openTokenTable(t, clock, nil)
	value, _, err := tokens.Issue(ctx, apptoken.Binding{
		AppID: "shop", Extension: "demo.widgets", Account: "owner", WindowID: "w1",
		ExpiresAt: clock.Now().Add(apptoken.DefaultTTL),
	})
	if err != nil {
		t.Fatalf("issue a token: %v", err)
	}
	clock.Advance(apptoken.DefaultTTL + time.Second)
	if _, err := tokens.Verify(ctx, value); !errors.Is(err, apptoken.ErrInvalid) {
		t.Fatalf("an expired token = %v, want invalid", err)
	}
}

// TestAppTokensOneLiveTokenPerWindow pins that a reopened window holds exactly
// one token.
func TestAppTokensOneLiveTokenPerWindow(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	_, tokens := openTokenTable(t, clock, nil)
	first, _, err := tokens.Issue(ctx, apptoken.Binding{AppID: "shop", Extension: "demo.widgets", Account: "owner", WindowID: "w1"})
	if err != nil {
		t.Fatalf("issue the first token: %v", err)
	}
	second, _, err := tokens.Issue(ctx, apptoken.Binding{AppID: "shop", Extension: "demo.widgets", Account: "owner", WindowID: "w1"})
	if err != nil {
		t.Fatalf("issue the second token: %v", err)
	}
	if _, err := tokens.Verify(ctx, first); !errors.Is(err, apptoken.ErrInvalid) {
		t.Fatalf("the replaced token = %v, want invalid", err)
	}
	if _, err := tokens.Verify(ctx, second); err != nil {
		t.Fatalf("the current token = %v, want valid", err)
	}
}

// TestAppTokensUninstalledApplication pins that a token the declaration no
// longer knows is refused, which is what an uninstall must do (KOSD-C-75).
func TestAppTokensUninstalledApplication(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	installed := &switchableDeclarations{}
	_, tokens := openTokenTable(t, clock, installed)
	value, _, err := tokens.Issue(ctx, apptoken.Binding{
		AppID: "shop", Extension: "demo.widgets", Account: "owner", WindowID: "w1",
	})
	if err != nil {
		t.Fatalf("issue a token: %v", err)
	}
	if _, err := tokens.Verify(ctx, value); err != nil {
		t.Fatalf("a declared application's token = %v, want valid", err)
	}
	installed.uninstalled = true
	if _, err := tokens.Verify(ctx, value); !errors.Is(err, apptoken.ErrInvalid) {
		t.Fatalf("an uninstalled application's token = %v, want invalid", err)
	}
}

// switchableDeclarations answers the extension seam until the case uninstalls
// the application.
type switchableDeclarations struct {
	uninstalled bool
}

// ObjectWriters implements access.ExtensionRoles.
func (d *switchableDeclarations) ObjectWriters(string, string) ([]write.Role, bool) {
	return nil, false
}

// DeclaredBy implements access.ExtensionRoles.
func (d *switchableDeclarations) DeclaredBy(string, string) (string, bool) { return "", false }

// ReadScopes implements access.ExtensionRoles.
func (d *switchableDeclarations) ReadScopes(string) []access.DeclaredRead { return nil }

// AppOrigin implements access.ExtensionRoles.
func (d *switchableDeclarations) AppOrigin(string, string) (string, bool) {
	if d.uninstalled {
		return "", false
	}
	return "https://shop.example.test", true
}

// TestAppTokensUnreadableTable pins the unavailable class: a token whose table
// cannot answer is told apart from one the table does not know (KOSD-C-86).
func TestAppTokensUnreadableTable(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	_, tokens := openTokenTable(t, clock, nil)
	value, _, err := tokens.Issue(ctx, apptoken.Binding{AppID: "shop", Extension: "demo.widgets", Account: "owner", WindowID: "w1"})
	if err != nil {
		t.Fatalf("issue a token: %v", err)
	}
	if err := tokens.Close(); err != nil {
		t.Fatalf("close the token table: %v", err)
	}
	if _, err := tokens.Verify(ctx, value); !errors.Is(err, apptoken.ErrUnavailable) {
		t.Fatalf("an unreadable table = %v, want unavailable", err)
	}
}

// TestAppTokenIssuerMintsTheDeclarationAndTheRange pins what a minted token
// carries: the declared entry origin, the declared classes the session's role
// satisfies and the applications the account can read now (KOSD-C-85,
// KOSD-C-87). The caller supplies neither the range nor the origin's port form.
func TestAppTokenIssuerMintsTheDeclarationAndTheRange(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	store, tokens := openTokenTable(t, clock, nil)
	declarations := fixedDeclarations{
		apps: map[string]string{"shop": "https://shop.example.test:443"},
		reads: []access.DeclaredRead{
			{Grants: []apptoken.Grant{{Group: "kos.tsic.top", Kind: "Application"}}},
			{Grants: []apptoken.Grant{{Group: "widgets.example.test", Kind: "Widget"}}, RequiredRole: write.RoleOwner},
		},
	}
	objects := fixedObjects{applications: []map[string]any{
		applicationObject("shop-app", "owner", "member"),
		applicationObject("theirs-app", "someone-else"),
	}}
	service := New(store, Config{Now: clock.Now, AppTokens: tokens})
	issuer := NewAppTokenIssuer(service, tokens, &access.Policy{Objects: objects, Extensions: declarations})

	window, err := issuer.Issue(ctx, Session{Username: "member", Role: write.RoleMember}, WindowTokenRequest{
		Application: "shop", Extension: "demo.widgets", WindowID: "w1", Origin: "https://shop.example.test",
	})
	if err != nil {
		t.Fatalf("mint the window token: %v", err)
	}
	binding, err := tokens.Verify(ctx, window.Token)
	if err != nil {
		t.Fatalf("verify the minted token: %v", err)
	}
	if want := "https://shop.example.test"; binding.Origin != want {
		t.Fatalf("binding origin = %q, want %q", binding.Origin, want)
	}
	if len(binding.Grants) != 1 || binding.Grants[0].Kind != "Application" {
		t.Fatalf("grants = %+v, want the class the member role satisfies", binding.Grants)
	}
	if len(binding.Objects) != 1 || binding.Objects[0].Name != "shop-app" {
		t.Fatalf("objects = %+v, want the applications the member can read", binding.Objects)
	}
	if !window.ExpiresAt.Equal(binding.ExpiresAt) {
		t.Fatalf("reported deadline %v and binding deadline %v differ", window.ExpiresAt, binding.ExpiresAt)
	}

	if _, err := issuer.Issue(ctx, Session{Username: "member", Role: write.RoleMember}, WindowTokenRequest{
		Application: "shop", Extension: "demo.widgets", WindowID: "w1", Origin: "https://elsewhere.example.test",
	}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("an origin the declaration does not name = %v, want permission denied", err)
	}
	if _, err := issuer.Issue(ctx, Session{Username: "member", Role: write.RoleMember}, WindowTokenRequest{
		Application: "uninstalled", Extension: "demo.widgets", WindowID: "w1", Origin: "https://shop.example.test",
	}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("an uninstalled application = %v, want permission denied", err)
	}
	if _, err := issuer.Issue(ctx, Session{Username: "", Role: write.RoleMember}, WindowTokenRequest{
		Application: "shop", Extension: "demo.widgets", WindowID: "w1", Origin: "https://shop.example.test",
	}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a session that signs nothing = %v, want unauthenticated", err)
	}
}
