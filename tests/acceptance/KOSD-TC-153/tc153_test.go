// Package kosdtc153 verifies KOSD-AC-71's application-token lifecycle
// (KOSD-TC-153): the control plane accepts an application token for the read
// classes the extension declares, refuses writes and out-of-scope reads without
// protected content, separates the three failure classes, and keeps a console
// cookie from widening the application identity.
package kosdtc153

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/pkg/access"
	"github.com/tsic404/kosd/pkg/apptoken"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	// widgetsGroup is the extension's own API group.
	widgetsGroup = "widgets.example.test"
	// extensionID is the identifier the declaration carries.
	extensionID = "demo.widgets"
	// shopOrigin is the entry origin of the application the token belongs to.
	shopOrigin = "https://shop.example.test"
	// adminOrigin is another installed application's origin.
	adminOrigin = "https://admin.example.test"
)

// declaration declares two applications and two read permissions: the
// application class is readable by every role, while the extension's own class
// needs the Owner.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: extensionID,
		Version:    "1.0.0",
		APIGroup:   widgetsGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: widgetsGroup, Kind: "Widget", Resource: "widgets",
				Versions: []string{"v1"}, Namespaced: true,
			}},
			Apps: []extension.App{
				{
					ID: "shop", Name: "Shop", Version: "1.0.0",
					Entry:       shopOrigin + "/index.html",
					Icon:        "icon.svg",
					Permissions: []string{},
					Window: extension.AppWindow{
						Width: 800, Height: 600, MinWidth: 640, MinHeight: 480,
						Mode: extension.AppWindowSingle, DefaultState: extension.AppStateNormal,
					},
				},
				{
					ID: "admin", Name: "Admin", Version: "1.0.0",
					Entry:       adminOrigin + "/index.html",
					Icon:        "icon.svg",
					Permissions: []string{},
					Window: extension.AppWindow{
						Width: 800, Height: 600, MinWidth: 640, MinHeight: 480,
						Mode: extension.AppWindowSingle, DefaultState: extension.AppStateNormal,
					},
				},
			},
		},
		Permissions: []extension.Permission{
			{
				Contribution: "Application",
				ReadScope: extension.ReadScope{
					Kinds: []extension.GroupKind{{Group: "kos.tsic.top", Kind: "Application"}},
				},
			},
			{
				Contribution: "Widget",
				ReadScope: extension.ReadScope{
					Kinds: []extension.GroupKind{{Group: widgetsGroup, Kind: "Widget"}},
				},
				RequiredRole: string(write.RoleOwner),
			},
		},
	}
}

// switchableVerifier resolves the store's tokens until a case makes the
// verification unavailable (KOSD-C-86).
type switchableVerifier struct {
	store       *apptoken.Store
	unavailable bool
}

func (v *switchableVerifier) Verify(ctx context.Context, value string) (apptoken.Binding, error) {
	if v.unavailable {
		return apptoken.Binding{}, apptoken.ErrUnavailable
	}
	return v.store.Verify(ctx, value)
}

// switchableRoles is the installed declaration as one case needs it: a read the
// extension currently declares can be withdrawn, and the change takes effect on
// the next request like a permission loss does (KOSD-C-85).
type switchableRoles struct {
	access.ExtensionRoles
	readsRevoked bool
}

// ReadScopes implements access.ExtensionRoles.
func (r *switchableRoles) ReadScopes(identifier string) []access.DeclaredRead {
	if r.readsRevoked {
		return nil
	}
	return r.ExtensionRoles.ReadScopes(identifier)
}

// application creates one application object the account lists.
func application(t *testing.T, cluster *harness.Cluster, name, owner string, members ...string) {
	t.Helper()
	entries := make([]any, 0, len(members))
	for _, member := range members {
		entries = append(entries, member)
	}
	cluster.Create(t, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec": map[string]any{
			"owner": owner,
			"visibility": map[string]any{
				"members":     entries,
				"allowGuests": false,
			},
		},
	}})
}

// TestTC153ApplicationTokenLifecycle exercises the read set, the refusal
// classes and the failure classes one application token goes through.
func TestTC153ApplicationTokenLifecycle(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	installation := cluster.InstallPackage(t, "widgets", declaration(), extension.MemoryArtifacts{})
	registry := installation.Registry()

	store := apptoken.NewStore(nil)
	verifier := &switchableVerifier{store: store}
	system := cluster.StartAccessWithAppTokens(t, registry, verifier, func(config *controlplane.Config) {
		config.Extensions = registry
	})

	owner := system.Owner(t, "owner", "owner-password")
	system.Account(t, owner, "member", "member-password", write.RoleMember)
	system.Account(t, owner, "signer", "signer-password", write.RoleMember)
	application(t, cluster, "shop-app", "owner", "member")
	// One widget exists, so an out-of-scope single read is refused by the
	// authorization seam and not answered as a missing object.
	createWidget(t, cluster, "some-widget")

	issue := func(binding apptoken.Binding) string {
		t.Helper()
		value, err := store.Issue(binding)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		return value
	}
	binding := func(account, window string, grants []apptoken.Grant) apptoken.Binding {
		return apptoken.Binding{
			AppID: "shop", Extension: extensionID, Account: account, WindowID: window,
			Origin: shopOrigin, Grants: grants,
		}
	}
	applicationGrant := []apptoken.Grant{{Group: "kos.tsic.top", Kind: "Application"}}
	// The issuer records the objects the account could read at that moment
	// (KOSD-C-85), so the token starts inside that upper bound.
	shopObject := apptoken.Object{Group: "kos.tsic.top", Kind: "Application", Namespace: harness.Namespace, Name: "shop-app"}
	scoped := func(account, window string, grants []apptoken.Grant, objects ...apptoken.Object) apptoken.Binding {
		issued := binding(account, window, grants)
		issued.Objects = objects
		return issued
	}
	token := issue(scoped("member", "window-1", applicationGrant, shopObject))

	// The declared class reads, and the audit trail names the application.
	status, body := system.Call(t, token, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin})
	if status != http.StatusOK {
		t.Fatalf("in-scope list = %d %v, want 200", status, body)
	}
	if count(t, body) != 1 {
		t.Fatalf("in-scope list items = %v, want the one visible application", body)
	}
	if status, body := system.Call(t, token, http.MethodGet,
		"/api/v1/resources/Application/shop-app?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusOK {
		t.Fatalf("in-scope single read = %d %v, want 200", status, body)
	}
	// HEAD shares the read branch, so a successful and a refused HEAD are
	// asserted separately from GET (KOSD-C-85 limits the methods to GET/HEAD).
	if status, body := system.Call(t, token, http.MethodHead,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusOK {
		t.Fatalf("in-scope HEAD = %d %v, want 200", status, body)
	}
	if status, body := system.Call(t, token, http.MethodHead,
		"/api/v1/resources/Widget/some-widget?apiVersion="+widgetsGroup+"/v1&namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusForbidden {
		t.Fatalf("out-of-scope HEAD = %d %v, want 403", status, body)
	}
	entries, err := system.Identity.Store.AuditEntries(context.Background(), 100)
	if err != nil {
		t.Fatalf("read the audit trail: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Actor == "member" && entry.Extension == extensionID && strings.Contains(entry.Object, "/App/shop") {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit trail does not name the application: %+v", entries)
	}

	// An out-of-scope single read and a write are refused as permission denied
	// and leave no content and no state change.
	status, body = system.Call(t, token, http.MethodGet,
		"/api/v1/resources/Widget/some-widget?apiVersion="+widgetsGroup+"/v1&namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin})
	if status != http.StatusForbidden || harness.String(body, "error", "code") != "permission_denied" {
		t.Fatalf("out-of-scope read = %d %v, want 403 permission_denied", status, body)
	}
	if _, leaked := body["spec"]; leaked {
		t.Fatalf("out-of-scope read leaked protected content: %v", body)
	}
	status, body = system.Call(t, token, http.MethodPost, "/api/v1/resources/Application", map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "written", "namespace": harness.Namespace},
		"spec":       map[string]any{"owner": "member"},
	}, map[string]string{"Origin": shopOrigin})
	if status != http.StatusForbidden || harness.String(body, "error", "code") != "permission_denied" {
		t.Fatalf("write with an application token = %d %v, want 403 permission_denied", status, body)
	}
	if status, body := system.Call(t, owner, http.MethodGet,
		"/api/v1/resources/Application/written?namespace="+harness.Namespace, nil, nil); status != http.StatusNotFound {
		t.Fatalf("application token wrote an object: read = %d %v, want 404", status, body)
	}

	// A role the declaration does not grant is permission denied even when the
	// token itself was issued for the class.
	widgetGrant := []apptoken.Grant{{Group: widgetsGroup, Kind: "Widget"}}
	if status, body := system.Call(t, issue(binding("member", "window-6", widgetGrant)), http.MethodGet,
		"/api/v1/resources/Widget?apiVersion="+widgetsGroup+"/v1&namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusForbidden {
		t.Fatalf("role-short read = %d %v, want 403", status, body)
	} else if harness.String(body, "error", "code") != "permission_denied" {
		t.Fatalf("role-short read = %d %v, want permission_denied, not another class", status, body)
	}
	ownerToken := issue(binding("owner", "window-2", widgetGrant))
	if status, body := system.Call(t, ownerToken, http.MethodGet,
		"/api/v1/resources/Widget?apiVersion="+widgetsGroup+"/v1&namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusOK {
		t.Fatalf("owner-role read = %d %v, want 200", status, body)
	}

	// The request origin must be the application's declared entry origin.
	if status, body := system.Call(t, token, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": adminOrigin}); status != http.StatusForbidden {
		t.Fatalf("foreign-origin read = %d %v, want 403", status, body)
	}
	if status, body := system.Call(t, token, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil, nil); status != http.StatusForbidden {
		t.Fatalf("origin-less read = %d %v, want 403", status, body)
	}

	// A token minted without the class stays an upper bound after the
	// declaration widens.
	empty := issue(binding("member", "window-3", nil))
	if status, body := system.Call(t, empty, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusForbidden {
		t.Fatalf("token without the grant = %d %v, want 403", status, body)
	}

	// Forged, expired and revoked tokens are unauthenticated.
	if status, body := system.Call(t, "forged-token", http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusUnauthorized || harness.String(body, "error", "code") != "unauthenticated" {
		t.Fatalf("forged token = %d %v, want 401 unauthenticated", status, body)
	}
	expired := issue(apptoken.Binding{
		AppID: "shop", Extension: extensionID, Account: "member", WindowID: "window-4",
		Origin: shopOrigin, Grants: applicationGrant, ExpiresAt: time.Now().Add(-time.Minute),
	})
	if status, body := system.Call(t, expired, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusUnauthorized {
		t.Fatalf("expired token = %d %v, want 401", status, body)
	}
	revoked := issue(binding("member", "window-5", applicationGrant))
	store.RevokeWindow("member", "window-5")
	if status, body := system.Call(t, revoked, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d %v, want 401", status, body)
	}
	// Sign-out and uninstall end every matching token and answer
	// unauthenticated at the HTTP boundary, exactly like closing the window
	// (KOSD-C-75).
	signedOut := issue(binding("signer", "window-7", applicationGrant))
	store.RevokeAccount("signer")
	if status, body := system.Call(t, signedOut, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusUnauthorized {
		t.Fatalf("token after sign-out = %d %v, want 401", status, body)
	}
	uninstalled := issue(binding("signer", "window-8", applicationGrant))
	store.RevokeApp("shop")
	if status, body := system.Call(t, uninstalled, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusUnauthorized {
		t.Fatalf("token after uninstall = %d %v, want 401", status, body)
	}
	// Uninstall revokes every token of the application, so the checks below
	// run against a fresh valid token again (KOSD-C-75).
	token = issue(scoped("member", "window-1", applicationGrant, shopObject))

	// A console cookie never widens the application identity.
	if status, body := system.Call(t, token, http.MethodGet,
		"/api/v1/resources/Widget?apiVersion="+widgetsGroup+"/v1&namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin, "Cookie": "kos_session=" + owner}); status != http.StatusForbidden {
		t.Fatalf("cookie widened an application token = %d %v, want 403", status, body)
	}

	// A read outside the callable set is refused and returns no account data.
	if status, body := system.Call(t, token, http.MethodGet,
		"/api/v1/accounts", nil, map[string]string{"Origin": shopOrigin}); status != http.StatusForbidden || harness.String(body, "error", "code") != "permission_denied" {
		t.Fatalf("account surface with an application token = %d %v, want 403 permission_denied", status, body)
	}

	// A verification that cannot answer is service unavailable on both the
	// in-scope and the out-of-scope read, never a pass, never an empty result
	// and never a downgrade to unauthenticated.
	verifier.unavailable = true
	status, body = system.Call(t, token, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin})
	if status != http.StatusServiceUnavailable || harness.String(body, "error", "code") != "service_unavailable" {
		t.Fatalf("unavailable verification = %d %v, want 503 service_unavailable", status, body)
	}
	if _, leaked := body["items"]; leaked {
		t.Fatalf("unavailable verification returned a result: %v", body)
	}
	if status, body := system.Call(t, token, http.MethodGet,
		"/api/v1/resources/Widget/some-widget?apiVersion="+widgetsGroup+"/v1&namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusServiceUnavailable || harness.String(body, "error", "code") != "service_unavailable" {
		t.Fatalf("unavailable out-of-scope read = %d %v, want 503 service_unavailable", status, body)
	}
}

// createWidget writes one object of the extension class.
func createWidget(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := cluster.Dynamic.Resource(schema.GroupVersionResource{
		Group: widgetsGroup, Version: "v1", Resource: "widgets",
	}).Namespace(harness.Namespace).Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": widgetsGroup + "/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec":       map[string]any{"label": "protected"},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create widget %s: %v", name, err)
	}
}

// TestTC153IssueTimeObjectScope proves the object range recorded at issue time
// is an upper bound: a later visibility change cannot widen a token already
// minted, a collection read omits what the range does not admit, and a single
// read outside the range is answered as permission denied.
func TestTC153IssueTimeObjectScope(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	installation := cluster.InstallPackage(t, "widgets", declaration(), extension.MemoryArtifacts{})
	registry := installation.Registry()

	store := apptoken.NewStore(nil)
	system := cluster.StartAccessWithAppTokens(t, registry, store, func(config *controlplane.Config) {
		config.Extensions = registry
	})

	owner := system.Owner(t, "owner", "owner-password")
	system.Account(t, owner, "member", "member-password", write.RoleMember)
	application(t, cluster, "visible-app", "owner", "member")
	application(t, cluster, "later-app", "owner")

	grant := []apptoken.Grant{{Group: "kos.tsic.top", Kind: "Application"}}
	shopObject := func(name string) apptoken.Object {
		return apptoken.Object{Group: "kos.tsic.top", Kind: "Application", Namespace: harness.Namespace, Name: name}
	}
	issue := func(window string, objects ...apptoken.Object) string {
		t.Helper()
		value, err := store.Issue(apptoken.Binding{
			AppID: "shop", Extension: extensionID, Account: "member", WindowID: window,
			Origin: shopOrigin, Grants: grant, Objects: objects,
		})
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		return value
	}
	listNames := func(token string) []string {
		t.Helper()
		status, body := system.Call(t, token, http.MethodGet,
			"/api/v1/resources/Application?namespace="+harness.Namespace, nil,
			map[string]string{"Origin": shopOrigin})
		if status != http.StatusOK {
			t.Fatalf("list = %d %v, want 200", status, body)
		}
		items, _ := body["items"].([]any)
		names := make([]string, 0, len(items))
		for _, item := range items {
			projection, _ := item.(map[string]any)
			ref, _ := projection["ref"].(map[string]any)
			if name, ok := ref["name"].(string); ok {
				names = append(names, name)
			}
		}
		return names
	}

	// The token records only what the account could read when it was minted.
	token := issue("window-scope-1", shopObject("visible-app"))
	if names := listNames(token); len(names) != 1 || names[0] != "visible-app" {
		t.Fatalf("issue-time list = %v, want only visible-app", names)
	}
	if status, body := system.Call(t, token, http.MethodGet,
		"/api/v1/resources/Application/later-app?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusForbidden || harness.String(body, "error", "code") != "permission_denied" {
		t.Fatalf("out-of-range single read = %d %v, want 403 permission_denied", status, body)
	} else if _, leaked := body["spec"]; leaked {
		t.Fatalf("out-of-range single read leaked protected content: %v", body)
	}

	// Making the object visible afterwards must not widen the token already
	// minted: the issue-time range stays the upper bound.
	setMembers(t, cluster, "later-app", "member")
	if names := listNames(token); len(names) != 1 || names[0] != "visible-app" {
		t.Fatalf("list after the grant widened = %v, want only visible-app", names)
	}
	if status, body := system.Call(t, token, http.MethodGet,
		"/api/v1/resources/Application/later-app?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusForbidden || harness.String(body, "error", "code") != "permission_denied" {
		t.Fatalf("single read after the grant widened = %d %v, want 403 permission_denied", status, body)
	}

	// A token minted now records both objects and reaches both.
	wider := issue("window-scope-2", shopObject("visible-app"), shopObject("later-app"))
	if names := listNames(wider); len(names) != 2 {
		t.Fatalf("list of a wider token = %v, want both applications", names)
	}
	if status, body := system.Call(t, wider, http.MethodGet,
		"/api/v1/resources/Application/later-app?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusOK {
		t.Fatalf("single read of a wider token = %d %v, want 200", status, body)
	}

	// An object that leaves the account's visibility after issue stays a
	// permission refusal, never an absent object.
	setMembers(t, cluster, "visible-app")
	if status, body := system.Call(t, wider, http.MethodGet,
		"/api/v1/resources/Application/visible-app?namespace="+harness.Namespace, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusForbidden || harness.String(body, "error", "code") != "permission_denied" {
		t.Fatalf("single read of a no-longer-visible object = %d %v, want 403 permission_denied", status, body)
	} else if _, leaked := body["spec"]; leaked {
		t.Fatalf("refused read leaked protected content: %v", body)
	}
	if names := listNames(wider); len(names) != 1 || names[0] != "later-app" {
		t.Fatalf("list after the object left visibility = %v, want only later-app", names)
	}
}

// TestTC153WithoutAuthorizationPolicy proves an application identity is never
// let through when the C-85 rule set is missing: the answer is service
// unavailable, not a permission and not a pass (KOSD-C-86).
func TestTC153WithoutAuthorizationPolicy(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	installation := cluster.InstallPackage(t, "widgets", declaration(), extension.MemoryArtifacts{})
	registry := installation.Registry()

	store := apptoken.NewStore(nil)
	system := cluster.StartAccessWithAppTokens(t, registry, store, func(config *controlplane.Config) {
		config.Extensions = registry
		config.Authorizer = identity.Authorizer{}
	})

	owner := system.Owner(t, "owner", "owner-password")
	system.Account(t, owner, "member", "member-password", write.RoleMember)
	application(t, cluster, "shop-app", "owner", "member")

	// A token with no grants and a foreign origin would fail any real check, so
	// a pass here would prove the bypass.
	value, err := store.Issue(apptoken.Binding{
		AppID: "shop", Extension: extensionID, Account: "member", WindowID: "window-policy",
		Origin: shopOrigin,
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	for _, headers := range []map[string]string{
		{"Origin": shopOrigin},
		{"Origin": adminOrigin},
	} {
		status, body := system.Call(t, value, http.MethodGet,
			"/api/v1/resources/Application?namespace="+harness.Namespace, nil, headers)
		if status != http.StatusServiceUnavailable || harness.String(body, "error", "code") != "service_unavailable" {
			t.Fatalf("read without a policy = %d %v, want 503 service_unavailable", status, body)
		}
		if _, leaked := body["items"]; leaked {
			t.Fatalf("read without a policy returned a result: %v", body)
		}
	}
}

// setMembers rewrites one application's visibility member list.
func setMembers(t *testing.T, cluster *harness.Cluster, name string, members ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	resource := cluster.Dynamic.Resource(schema.GroupVersionResource{
		Group: "kos.tsic.top", Version: "v1beta1", Resource: "applications",
	}).Namespace(harness.Namespace)
	stored, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read application %s: %v", name, err)
	}
	entries := make([]any, 0, len(members))
	for _, member := range members {
		entries = append(entries, member)
	}
	if err := unstructured.SetNestedSlice(stored.Object, entries, "spec", "visibility", "members"); err != nil {
		t.Fatalf("set members of %s: %v", name, err)
	}
	if _, err := resource.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update application %s: %v", name, err)
	}
}

// TestTC153UnfilteredEventReadClasses proves the predicate the event collection
// read carries on its own: a read that names no subject still fails as the right
// class when the identity is outside the declared origin or when the rule cannot
// answer, and neither failure arrives as a successful window with no events
// (KOSD-C-85, KOSD-C-86).
func TestTC153UnfilteredEventReadClasses(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	installation := cluster.InstallPackage(t, "widgets", declaration(), extension.MemoryArtifacts{})
	registry := installation.Registry()

	store := apptoken.NewStore(nil)
	system := cluster.StartAccessWithAppTokens(t, registry, store, func(config *controlplane.Config) {
		config.Extensions = registry
	})

	owner := system.Owner(t, "owner", "owner-password")
	system.Account(t, owner, "member", "member-password", write.RoleMember)
	application(t, cluster, "shop-app", "owner", "member")
	storeEvent(t, cluster, "shop-visible", "Application", "shop-app", system.Identity.Clock.Now().Add(-5*time.Minute))

	value, err := store.Issue(apptoken.Binding{
		AppID: "shop", Extension: extensionID, Account: "member", WindowID: "window-events",
		Origin: shopOrigin, Grants: []apptoken.Grant{{Group: "kos.tsic.top", Kind: "Application"}},
		Objects: []apptoken.Object{{Group: "kos.tsic.top", Kind: "Application", Namespace: harness.Namespace, Name: "shop-app"}},
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	// The declared origin reads the window; the same token from another origin
	// is refused, never answered with an empty window.
	status, body := system.Call(t, value, http.MethodGet,
		"/api/v1/events?namespace="+harness.Namespace, nil, map[string]string{"Origin": shopOrigin})
	if status != http.StatusOK {
		t.Fatalf("declared-origin event read = %d %v, want 200", status, body)
	}
	if items, _ := body["events"].([]any); len(items) != 1 {
		t.Fatalf("declared-origin events = %v, want the stored event", body["events"])
	}
	for _, origin := range []string{adminOrigin, ""} {
		headers := map[string]string{}
		if origin != "" {
			headers["Origin"] = origin
		}
		status, body := system.Call(t, value, http.MethodGet,
			"/api/v1/events?namespace="+harness.Namespace, nil, headers)
		if status != http.StatusForbidden || harness.String(body, "error", "code") != "permission_denied" {
			t.Fatalf("event read from origin %q = %d %v, want 403 permission_denied", origin, status, body)
		}
		if _, leaked := body["events"]; leaked {
			t.Fatalf("refused event read answered with events: %v", body)
		}
	}

	// An event about a class no extension declares is outside the declared read
	// range, so it is absent from the window; the read itself still succeeds and
	// carries no content of the excluded subject (KOSD-C-85).
	storeEvent(t, cluster, "plugin-opaque", "Opaque", "hidden-subject", system.Identity.Clock.Now().Add(-3*time.Minute))
	status, body = system.Call(t, value, http.MethodGet,
		"/api/v1/events?namespace="+harness.Namespace, nil, map[string]string{"Origin": shopOrigin})
	if status != http.StatusOK {
		t.Fatalf("event read with an undeclared subject = %d %v, want 200", status, body)
	}
	events, _ := body["events"].([]any)
	for _, raw := range events {
		event, _ := raw.(map[string]any)
		if harness.String(event, "involvedObject", "kind") == "Opaque" {
			t.Fatalf("event about an undeclared class was returned: %v", event)
		}
	}
	if len(events) != 1 {
		t.Fatalf("events = %v, want only the declared subject's event", events)
	}
}

// TestTC153UnfilteredEventReadWithoutPolicy proves the third failure class on the
// same collection read: with no rule set to judge the identity, the window is
// service unavailable rather than an empty success (KOSD-C-86).
func TestTC153UnfilteredEventReadWithoutPolicy(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	installation := cluster.InstallPackage(t, "widgets", declaration(), extension.MemoryArtifacts{})
	registry := installation.Registry()

	store := apptoken.NewStore(nil)
	system := cluster.StartAccessWithAppTokens(t, registry, store, func(config *controlplane.Config) {
		config.Extensions = registry
		config.Authorizer = identity.Authorizer{}
	})

	owner := system.Owner(t, "owner", "owner-password")
	system.Account(t, owner, "member", "member-password", write.RoleMember)
	application(t, cluster, "shop-app", "owner", "member")
	storeEvent(t, cluster, "shop-visible", "Application", "shop-app", system.Identity.Clock.Now().Add(-5*time.Minute))

	value, err := store.Issue(apptoken.Binding{
		AppID: "shop", Extension: extensionID, Account: "member", WindowID: "window-events-policy",
		Origin: shopOrigin, Grants: []apptoken.Grant{{Group: "kos.tsic.top", Kind: "Application"}},
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	status, body := system.Call(t, value, http.MethodGet,
		"/api/v1/events?namespace="+harness.Namespace, nil, map[string]string{"Origin": shopOrigin})
	if status != http.StatusServiceUnavailable || harness.String(body, "error", "code") != "service_unavailable" {
		t.Fatalf("event read without a policy = %d %v, want 503 service_unavailable", status, body)
	}
	if _, leaked := body["events"]; leaked {
		t.Fatalf("event read without a policy answered with events: %v", body)
	}
}

// storeEvent writes one event about one subject into the cluster, inside C-64's
// window.
func storeEvent(t *testing.T, cluster *harness.Cluster, name, kind, subject string, lastSeen time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := cluster.Dynamic.Resource(schema.GroupVersionResource{
		Version: "v1", Resource: "events",
	}).Namespace(harness.Namespace).Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"involvedObject": map[string]any{
			"apiVersion": "kos.tsic.top/v1beta1",
			"kind":       kind,
			"namespace":  harness.Namespace,
			"name":       subject,
		},
		"type":          "Warning",
		"reason":        "BackendUnavailable",
		"message":       "no ready backend",
		"lastTimestamp": lastSeen.UTC().Format(time.RFC3339Nano),
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create event %s: %v", name, err)
	}
}

// TestTC153EmptyReadIntersection proves the collection-level question is asked
// on both halves: a legal origin is not enough when the identity holds no read
// authorization at all. An empty grant, a declaration that no longer declares
// the read and a declared role the account does not hold are refused as
// permission denied without an `events` field, not answered as a successful
// window with nothing in it; an authorized identity still filters one
// unreadable subject out instead of failing the page, and a session read is
// unchanged (KOSD-C-85, KOSD-C-86).
func TestTC153EmptyReadIntersection(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	installation := cluster.InstallPackage(t, "widgets", declaration(), extension.MemoryArtifacts{})
	registry := installation.Registry()
	roles := &switchableRoles{ExtensionRoles: registry}

	store := apptoken.NewStore(nil)
	system := cluster.StartAccessWithAppTokens(t, roles, store, func(config *controlplane.Config) {
		config.Extensions = registry
	})

	owner := system.Owner(t, "owner", "owner-password")
	member := system.Account(t, owner, "member", "member-password", write.RoleMember)
	application(t, cluster, "shop-app", "owner", "member")
	application(t, cluster, "blog-app", "owner")
	now := system.Identity.Clock.Now()
	storeEvent(t, cluster, "shop-visible", "Application", "shop-app", now.Add(-5*time.Minute))
	storeEvent(t, cluster, "blog-hidden", "Application", "blog-app", now.Add(-4*time.Minute))

	issue := func(window string, grants []apptoken.Grant) string {
		t.Helper()
		objects := []apptoken.Object{{Group: "kos.tsic.top", Kind: "Application", Namespace: harness.Namespace, Name: "shop-app"}}
		value, err := store.Issue(apptoken.Binding{
			AppID: "shop", Extension: extensionID, Account: "member", WindowID: window,
			Origin: shopOrigin, Grants: grants, Objects: objects,
		})
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		return value
	}
	applicationGrant := []apptoken.Grant{{Group: "kos.tsic.top", Kind: "Application"}}
	valid := issue("window-intersection", applicationGrant)
	refused := func(t *testing.T, token, label string) {
		t.Helper()
		status, body := system.Call(t, token, http.MethodGet,
			"/api/v1/events?namespace="+harness.Namespace, nil, map[string]string{"Origin": shopOrigin})
		if status != http.StatusForbidden || harness.String(body, "error", "code") != "permission_denied" {
			t.Fatalf("%s = %d %v, want 403 permission_denied", label, status, body)
		}
		if _, leaked := body["events"]; leaked {
			t.Fatalf("%s answered with events: %v", label, body)
		}
	}

	// The control: a token inside the intersection reads the window, and a
	// subject outside its issue-time range is dropped per item rather than
	// failing the page.
	status, body := system.Call(t, valid, http.MethodGet,
		"/api/v1/events?namespace="+harness.Namespace, nil, map[string]string{"Origin": shopOrigin})
	if status != http.StatusOK {
		t.Fatalf("authorized event read = %d %v, want 200", status, body)
	}
	if events, _ := body["events"].([]any); len(events) != 1 {
		t.Fatalf("authorized events = %v, want only the subject inside the issue-time range", body["events"])
	}

	// A token issued with no read grant at all.
	refused(t, issue("window-empty-grants", nil), "event read with an empty grant")

	// The current declaration no longer declares the read the token was issued
	// for; the same token reads again once the declaration is back.
	roles.readsRevoked = true
	refused(t, valid, "event read after the declaration withdrew the read")
	roles.readsRevoked = false
	if status, body := system.Call(t, valid, http.MethodGet,
		"/api/v1/events?namespace="+harness.Namespace, nil, map[string]string{"Origin": shopOrigin}); status != http.StatusOK {
		t.Fatalf("event read after the declaration came back = %d %v, want 200", status, body)
	}

	// The class is declared and granted, but the role the declaration requires
	// is one the account does not hold.
	refused(t, issue("window-role-short", []apptoken.Grant{{Group: widgetsGroup, Kind: "Widget"}}),
		"event read with a declared role the account does not hold")

	// A session reads the same window with C-64's own visibility: the collection
	// admission above belongs to the application identity, not to sessions.
	if status, body := system.Call(t, member, http.MethodGet,
		"/api/v1/events?namespace="+harness.Namespace, nil, nil); status != http.StatusOK {
		t.Fatalf("session event read = %d %v, want 200", status, body)
	}
}

// count returns the number of items a list response carries.
func count(t *testing.T, body map[string]any) int {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("list response has no items: %v", body)
	}
	return len(items)
}
