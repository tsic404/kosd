package kosdtc153

import (
	"net/http"
	"testing"

	"github.com/tsic404/kosd/pkg/apptoken"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// TestTC153OriginDefaultPort covers KOSD-C-87's exact-origin rule at the HTTP
// boundary: an entry declared with the HTTPS default port admits the Origin a
// browser serializes, while a non-default port stays its own origin.
func TestTC153OriginDefaultPort(t *testing.T) {
	decl := declaration()
	decl.Contributions.Apps[0].Entry = "https://shop.example.test:443/index.html"
	decl.Contributions.Apps[1].Entry = "https://admin.example.test:8443/index.html"

	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	installation := cluster.InstallPackage(t, "widgets", decl, extension.MemoryArtifacts{})
	registry := installation.Registry()

	store := apptoken.NewStore(nil)
	system := cluster.StartAccessWithAppTokens(t, registry, store, func(config *controlplane.Config) {
		config.Extensions = registry
	})

	owner := system.Owner(t, "owner", "owner-password")
	system.Account(t, owner, "member", "member-password", write.RoleMember)
	application(t, cluster, "shop-app", "owner", "member")

	grant := []apptoken.Grant{{Group: "kos.tsic.top", Kind: "Application"}}
	issue := func(appID, origin, window string) string {
		t.Helper()
		value, err := store.Issue(apptoken.Binding{
			AppID: appID, Extension: extensionID, Account: "member", WindowID: window,
			Origin: origin, Grants: grant,
		})
		if err != nil {
			t.Fatalf("issue %s token: %v", appID, err)
		}
		return value
	}
	list := "/api/v1/resources/Application?namespace=" + harness.Namespace

	// The declaration wrote :443; the browser sends the port-less origin.
	shop := issue("shop", "https://shop.example.test", "window-shop")
	if status, body := system.Call(t, shop, http.MethodGet, list, nil,
		map[string]string{"Origin": "https://shop.example.test"}); status != http.StatusOK {
		t.Fatalf("explicit default port = %d %v, want 200", status, body)
	}

	// The declaration wrote :8443, so the port-less origin is a different one.
	admin := issue("admin", "https://admin.example.test:8443", "window-admin")
	if status, body := system.Call(t, admin, http.MethodGet, list, nil,
		map[string]string{"Origin": "https://admin.example.test"}); status != http.StatusForbidden {
		t.Fatalf("non-default port with default origin = %d %v, want 403", status, body)
	}
	if status, body := system.Call(t, admin, http.MethodGet, list, nil,
		map[string]string{"Origin": "https://admin.example.test:8443"}); status != http.StatusOK {
		t.Fatalf("non-default port with matching origin = %d %v, want 200", status, body)
	}
}
