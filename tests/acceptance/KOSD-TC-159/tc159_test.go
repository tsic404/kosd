// Package kosdtc159 checks KOSD-AC-73's browser entry boundary on this
// repository's side: a browser session reaches the domain API and nothing else,
// obtains no cluster credential and takes no write path beside the control-plane
// API. Identity comes only from the bearer a front forwards, so a console cookie
// neither authenticates, widens, nor rescues a rejected token (KOSD-C-86).
//
// The desktop front is kos-dashboard's pkg/bff, which imports this module, so
// this case cannot start it here. The front's own half of the same criterion is
// carried in kos-dashboard: DASH-TC-140 builds the real front with bff.New and
// checks the browser answers carry no cluster credential, DASH-TC-141 and
// DASH-TC-161 carry the entry and layout halves.
package kosdtc159

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	ownerName = "owner"
	ownerPass = "owner-password"
	member    = "alice"
	memberKey = "alice-password"

	widgetsGroup = "widgets.example.test"
	pageID       = "widgets"
	appID        = "widgets"
	appEntry     = "https://widgets.example.test/"
	unknownToken = "not-a-session-token"

	// bffSessionCookie is the cookie name the desktop front sets. The control
	// plane must not read identity out of it (KOSD-C-86).
	bffSessionCookie = "kos_bff_session"
)

// declaration installs one page and one window application, so the browser
// session has every surface a product contributes: a page with an action, its
// projection and the launcher entry.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "demo.widgets",
		Version:    "1.0.0",
		APIGroup:   widgetsGroup,
		Contributions: extension.Contributions{
			Pages: []extension.Page{{
				ID: pageID, Title: "Widgets", Location: "panel",
				Items: []extension.PageItem{{
					ID: "recent", Label: "Recent", Source: extension.PageSourceLocal,
					Store: "widgets.recent", Reason: "只读展示",
				}},
				Actions: []extension.Action{{
					ID: "install", Method: http.MethodPost, Path: "/api/v1/action-requests",
				}},
			}},
			Apps: []extension.App{{
				ID:      appID,
				Name:    "Widgets",
				Version: "1.0.0",
				Icon:    "https://widgets.example.test/icon.svg",
				Entry:   appEntry,
				Window: extension.AppWindow{
					Width: 1200, Height: 800, MinWidth: 720, MinHeight: 480,
					Resizable: true, Mode: extension.AppWindowSingle, DefaultState: extension.AppStateNormal,
				},
				Permissions: []string{},
			}},
		},
	}
}

// TestTC159BrowserReachesOnlyTheControlPlaneSurface enumerates what a signed-in
// browser can reach, checks no surface hands out cluster credentials, and
// probes the write paths beside the control-plane API.
func TestTC159BrowserReachesOnlyTheControlPlaneSurface(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	owner := shell.SignInOwner(t, ownerName, ownerPass)
	alice := shell.Account(t, owner, member, memberKey, write.RoleMember)
	installation := cluster.InstallPackage(t, "widgets", declaration(), extension.MemoryArtifacts{})
	shell.Use(installation)

	console, err := url.Parse(shell.Accounts.HTTP.URL)
	if err != nil {
		t.Fatalf("read the console address: %v", err)
	}

	// Every surface a browser session reads. Each answer is the control-plane
	// or shell projection, and none carries a cluster credential (KOSD-C-12, KOSD-C-14).
	surfaces := []string{
		"/api/v1/auth/session",
		"/api/v1/pages",
		"/api/v1/pages/" + pageID,
		"/api/v1/apps",
		"/api/v1/resources/Application?namespace=" + harness.Namespace,
		"/api/v1/settings",
		"/api/v1/catalogue/entries",
		"/api/v1/accounts",
	}
	for _, path := range surfaces {
		status, payload, err := shell.Accounts.HTTP.Raw(http.MethodGet, path, nil, harness.Bearer(owner))
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, status, payload)
		}
		assertNoClusterCredentials(t, path, payload)
	}
	// The health probe carries only the verdict and its reason, never the
	// measurement values behind them (KOSD-C-56, KOSD-C-57).
	status, payload, err := shell.Accounts.HTTP.Raw(http.MethodGet, "/api/v1/system/health", nil, harness.Bearer(owner))
	if err != nil {
		t.Fatalf("GET /api/v1/system/health: %v", err)
	}
	if status != http.StatusOK && status != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/v1/system/health = %d %s, want the probe verdict", status, payload)
	}
	assertNoClusterCredentials(t, "/api/v1/system/health", payload)

	// The applications the launcher opens live on their own origins: the browser
	// never reaches an application through the console origin (KOSD-C-73).
	status, body := shell.Call(t, http.MethodGet, "/api/v1/apps", nil, alice)
	if status != http.StatusOK {
		t.Fatalf("read the applications = %d %v", status, body)
	}
	apps := harness.Items(t, body)
	if len(apps) != 1 {
		t.Fatalf("the projection carries %d applications, want one", len(apps))
	}
	entry, err := url.Parse(harness.String(apps[0], "manifest", "entry"))
	if err != nil || entry.Host == "" {
		t.Fatalf("application entry = %q, want an absolute address", harness.String(apps[0], "manifest", "entry"))
	}
	if entry.Scheme != "https" || strings.EqualFold(entry.Host, console.Host) {
		t.Fatalf("application entry = %q, want https on an origin other than the console %q", entry, console.Host)
	}

	// A page's write actions name control-plane API paths: the shell carries no
	// second write channel between the browser and an object (KOSD-C-12).
	_, payload, err = shell.Accounts.HTTP.Raw(http.MethodGet, "/api/v1/pages/"+pageID, nil, harness.Bearer(owner))
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}
	page := decode(t, payload)
	actions, _ := page["actions"].([]any)
	if len(actions) == 0 {
		t.Fatalf("the page carries no action: %v", page["actions"])
	}
	for _, raw := range actions {
		action, _ := raw.(map[string]any)
		entry, _ := action["entry"].(map[string]any)
		method, _ := entry["method"].(string)
		path, _ := entry["path"].(string)
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodDelete:
		default:
			t.Fatalf("action %v method %q is not a control-plane write", action["id"], method)
		}
		if !strings.HasPrefix(path, "/api/v1/") {
			t.Fatalf("action %v path %q is not a control-plane API path", action["id"], path)
		}
	}

	// The page read route and the application projection are read-only: neither
	// offers a write the control-plane API does not already serve.
	status, body = shell.Call(t, http.MethodPost, "/api/v1/pages/"+pageID, map[string]any{"x": 1}, owner)
	if harness.String(body, "error", "code") != controlplane.CodePageReadOnly {
		t.Fatalf("write through the page route = %d %v, want page_read_only", status, body)
	}
	status, payload, err = shell.Accounts.HTTP.Raw(http.MethodPost, "/api/v1/apps",
		map[string]any{"id": appID}, harness.Bearer(owner))
	if err != nil {
		t.Fatalf("POST /api/v1/apps: %v", err)
	}
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("write through the application projection = %d %s, want the read-only projection", status, payload)
	}

	// There is no proxy, raw or native write route beside the domain API, and
	// none of the guessed credential routes exists.
	for _, path := range []string{
		"/api/v1/proxy/namespaces/" + harness.Namespace + "/pods/probe",
		"/api/v1/raw/Pod/probe",
		"/api/v1/resources/Pod/probe/proxy",
		"/api/v1/kubeconfig",
		"/api/v1/cluster/credentials",
		"/api/v1/serviceaccount-token",
	} {
		if status, body = shell.Call(t, http.MethodGet, path, nil, owner); status != http.StatusNotFound {
			t.Fatalf("GET %s = %d %v, want the catch-all 404", path, status, body)
		}
	}

	// Out-of-range reads and writes beside the modeled kinds are refused with
	// one error category and no object content (KOSD-C-12, KOSD-C-59).
	status, readBody := shell.Call(t, http.MethodGet, "/api/v1/resources/Pod?namespace="+harness.Namespace, nil, alice)
	statusWrite, writeBody := shell.Call(t, http.MethodPost, "/api/v1/resources/Pod?namespace="+harness.Namespace,
		map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"name": "probe", "namespace": harness.Namespace},
			"spec": map[string]any{"containers": []any{
				map[string]any{"name": "probe", "image": "busybox"},
			}},
		}, alice)
	readCode := harness.String(readBody, "error", "code")
	writeCode := harness.String(writeBody, "error", "code")
	if readCode == "" || readCode != writeCode {
		t.Fatalf("out-of-range read %d %v and write %d %v report different categories",
			status, readBody, statusWrite, writeBody)
	}
	if _, found := readBody["items"]; found {
		t.Fatalf("the out-of-range read returned object content: %v", readBody)
	}
	if _, err := cluster.Kube.CoreV1().Pods(harness.Namespace).Get(t.Context(), "probe", metav1.GetOptions{}); err == nil {
		t.Fatal("the browser wrote a native Pod")
	}

	// A credential the control plane cannot validate is unauthenticated on every
	// session surface and writes nothing; the host turns no token into a session.
	for _, request := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/apps", nil},
		{http.MethodGet, "/api/v1/resources/Application?namespace=" + harness.Namespace, nil},
		{http.MethodPost, "/api/v1/resources/Application?namespace=" + harness.Namespace, map[string]any{
			"apiVersion": api.Group + "/v1beta1",
			"kind":       "Application",
			"metadata":   map[string]any{"name": "forged", "namespace": harness.Namespace},
			"spec":       map[string]any{},
		}},
	} {
		status, body = shell.Call(t, request.method, request.path, request.body, unknownToken)
		if status != http.StatusUnauthorized || harness.String(body, "error", "code") != controlplane.CodeUnauthenticated {
			t.Fatalf("%s %s with an unvalidated token = %d %v, want unauthenticated", request.method, request.path, status, body)
		}
	}
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).
		Get(t.Context(), "forged", metav1.GetOptions{}); err == nil {
		t.Fatal("an unvalidated token wrote an Application")
	}

	requireCookieNeverCarriesIdentity(t, shell, owner, alice)
}

// TODO(TSI-4714): 控制面接受窗口应用令牌后，用同一有效令牌经 BFF 与直连各做一次范围内读、范围外读与写请求，核对返回数据、错误类别一致且上游令牌未被替换。

// requireCookieNeverCarriesIdentity checks the identity rule a front relies on:
// the console cookie neither authenticates a request, nor widens one, nor
// rescues a rejected bearer token (KOSD-C-86).
func requireCookieNeverCarriesIdentity(t *testing.T, shell *harness.Shell, owner, alice string) {
	t.Helper()
	sessionCookie := bffSessionCookie + "=" + owner

	// A cookie on its own is not a credential: the front has to forward a token.
	for _, path := range []string{"/api/v1/apps", "/api/v1/auth/session"} {
		status, payload, err := shell.Accounts.HTTP.Raw(http.MethodGet, path, nil, map[string]string{"Cookie": sessionCookie})
		if err != nil {
			t.Fatalf("GET %s with only a console cookie: %v", path, err)
		}
		body := decode(t, payload)
		if status != http.StatusUnauthorized || harness.String(body, "error", "code") != controlplane.CodeUnauthenticated {
			t.Fatalf("GET %s with only a console cookie = %d %s, want unauthenticated", path, status, payload)
		}
	}

	// A rejected token does not fall back to the cookie identity.
	status, payload, err := shell.Accounts.HTTP.Raw(http.MethodGet, "/api/v1/apps", nil, map[string]string{
		"Cookie":        sessionCookie,
		"Authorization": "Bearer " + unknownToken,
	})
	if err != nil {
		t.Fatalf("GET /api/v1/apps with a rejected token and a console cookie: %v", err)
	}
	if body := decode(t, payload); status != http.StatusUnauthorized ||
		harness.String(body, "error", "code") != controlplane.CodeUnauthenticated {
		t.Fatalf("a rejected token plus a console cookie = %d %s, want unauthenticated", status, payload)
	}

	// A cookie for another account does not widen the request: the bearer token
	// decides the identity, so an Owner-only write stays refused to a Member.
	memberHeaders := map[string]string{"Cookie": sessionCookie, "Authorization": "Bearer " + alice}
	status, payload, err = shell.Accounts.HTTP.Raw(http.MethodGet, "/api/v1/auth/session", nil, memberHeaders)
	if err != nil {
		t.Fatalf("GET /api/v1/auth/session with a member token and an owner cookie: %v", err)
	}
	if body := decode(t, payload); status != http.StatusOK || harness.String(body, "username") != member {
		t.Fatalf("session = %d %s, want the bearer account %q", status, payload, member)
	}
	status, payload, err = shell.Accounts.HTTP.Raw(http.MethodPost, "/api/v1/accounts",
		map[string]any{"username": "cookie-widen", "role": string(write.RoleMember)}, memberHeaders)
	if err != nil {
		t.Fatalf("POST /api/v1/accounts with a member token: %v", err)
	}
	body := decode(t, payload)
	if status != http.StatusForbidden || harness.String(body, "error", "code") != controlplane.CodePermissionDenied {
		t.Fatalf("an Owner-only account write with a Member token and an Owner cookie = %d %s, want permission_denied", status, payload)
	}
	if _, stored := findAccount(t, shell, owner, "cookie-widen"); stored {
		t.Fatal("the cookie widened the member token into an account write")
	}
}

// findAccount reports whether the accounts projection carries one username.
func findAccount(t *testing.T, shell *harness.Shell, token, username string) (map[string]any, bool) {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, "/api/v1/accounts", nil, token)
	if status != http.StatusOK {
		t.Fatalf("read the accounts = %d %v", status, body)
	}
	for _, item := range harness.Items(t, body) {
		if harness.String(item, "username") == username {
			return item, true
		}
	}
	return nil, false
}

// assertNoClusterCredentials fails when one browser-visible body carries a
// kubeconfig, a client certificate, a private key or a service-account token.
func assertNoClusterCredentials(t *testing.T, surface string, payload []byte) {
	t.Helper()
	lower := strings.ToLower(string(payload))
	for _, marker := range []string{
		"kubeconfig",
		"client-key-data",
		"client-certificate-data",
		"certificate-authority-data",
		"begin private key",
		"kubernetes.default.svc",
		"system:serviceaccount",
	} {
		if strings.Contains(lower, marker) {
			t.Fatalf("%s carries cluster credentials (%q): %s", surface, marker, payload)
		}
	}
}

// decode parses one JSON object answer.
func decode(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	decoded := map[string]any{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	return decoded
}
