package kosdtc153

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsic404/kosd/pkg/access"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// productionServer builds the HTTP surface the deployed kos-control-plane
// process serves: the control-plane database, the account service, the
// application-token table the account layer writes, and the account gate that
// reads it ahead of the domain API. It is not the acceptance harness: every
// constructor and the mount order are the ones cmd/kos-control-plane uses.
func productionServer(t *testing.T, cluster *harness.Cluster, registry *extension.Registry) (*httptest.Server, *identity.Store, *identity.AppTokens, *identity.Service, *harness.Clock) {
	t.Helper()
	clock := harness.NewClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	store, err := identity.Open(context.Background(), identity.OpenOptions{
		Path: filepath.Join(t.TempDir(), "kos.db"),
		Now:  clock.Now,
	})
	if err != nil {
		t.Fatalf("open the control-plane database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	windowTokens, err := identity.NewAppTokens(store, registry)
	if err != nil {
		t.Fatalf("open the application-token table: %v", err)
	}
	t.Cleanup(func() { _ = windowTokens.Close() })
	trail := identity.AuditTrail{Store: store}
	accounts := identity.New(store, identity.Config{
		Now:         clock.Now,
		Owned:       identity.ClusterOwnedObjects{Dynamic: cluster.Dynamic},
		Preferences: registry,
		Auditor:     trail,
		AppTokens:   windowTokens,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	authorizer := identity.Authorizer{Policy: &access.Policy{
		Objects:    controlplane.NewClusterSource(cluster.Dynamic),
		Extensions: registry,
	}}
	domain, err := controlplane.New(controlplane.Config{
		Dynamic:    cluster.Dynamic,
		Extensions: registry,
		Authorizer: authorizer,
		Auditor:    trail,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        clock.Now,
		Terminal: controlplane.TerminalConfig{
			Console:    identity.TerminalSessions{Service: accounts, Store: store},
			Audit:      identity.TerminalAuditTrail{Store: store},
			Extensions: registry,
		},
	})
	if err != nil {
		t.Fatalf("build the control plane: %v", err)
	}
	t.Cleanup(func() { domain.Close() })
	issuer := identity.NewAppTokenIssuer(accounts, windowTokens, authorizer.Policy)
	accountsAPI := identity.NewAPIWithAppTokens(accounts, windowTokens, issuer).Handler(domain.Handler())
	root := http.NewServeMux()
	root.Handle("GET /api/v1/system/health", domain.Handler())
	root.Handle("POST /api/v1/terminal/sessions/{sessionId}/attach", domain.TerminalPublicHandler())
	root.Handle("POST /api/v1/terminal/sessions/{sessionId}/input", domain.TerminalPublicHandler())
	root.Handle("/", accountsAPI)
	server := httptest.NewServer(root)
	t.Cleanup(server.Close)
	return server, store, windowTokens, accounts, clock
}

// call sends one request to the assembled server and returns the status and the
// decoded body.
func call(t *testing.T, server *httptest.Server, method, path, token string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	decoded := map[string]any{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("%s %s answered a non-JSON body: %q", method, path, payload)
		}
	}
	return response.StatusCode, decoded
}

// login trades a login name and password for a session token.
func login(t *testing.T, server *httptest.Server, username, password string) string {
	t.Helper()
	status, body := call(t, server, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username": username, "password": password,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("login %s = %d %v, want 200", username, status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("login %s returned no token: %v", username, body)
	}
	return token
}

// signIn replaces a one-time password and returns the session token the
// replacement leaves usable.
func signIn(t *testing.T, server *httptest.Server, username, oneTime, password string) string {
	t.Helper()
	token := login(t, server, username, oneTime)
	if status, body := call(t, server, http.MethodPut, "/api/v1/auth/password", token, map[string]any{
		"currentPassword": oneTime, "newPassword": password,
	}, nil); status != http.StatusNoContent {
		t.Fatalf("change the first password = %d %v, want 204", status, body)
	}
	return login(t, server, username, password)
}

// TestTC153ProductionEntryVerifiesHostTokens runs the window-token lifecycle
// against the assembly the deployed control plane actually serves (KOSD-C-75,
// KOSD-C-85, KOSD-C-86, KOSD-C-87).
func TestTC153ProductionEntryVerifiesHostTokens(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	installation := cluster.InstallPackage(t, "widgets", declaration(), extension.MemoryArtifacts{})
	registry := installation.Registry()
	application(t, cluster, "shop-app", "owner", "member")
	application(t, cluster, "theirs-app", "someone-else")
	createWidget(t, cluster, "some-widget")

	server, _, windowTokens, accounts, clock := productionServer(t, cluster, registry)

	bootstrap, err := accounts.BootstrapOwner(context.Background(), "owner")
	if err != nil {
		t.Fatalf("prepare the first Owner: %v", err)
	}
	owner := signIn(t, server, "owner", bootstrap.OneTimePassword, "owner-password")
	status, body := call(t, server, http.MethodPost, "/api/v1/accounts", owner, map[string]any{
		"username": "member", "role": string(write.RoleMember),
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create the member account = %d %v, want 201", status, body)
	}
	oneTime, _ := body["oneTimePassword"].(string)
	member := signIn(t, server, "member", oneTime, "member-password")

	// The host takes the token for a window from the control plane, and the
	// control plane keeps it: the table the gate reads is the table that wrote
	// the value it handed over.
	status, window := call(t, server, http.MethodPost, "/api/v1/app-tokens", member, map[string]any{
		"application": "shop", "extension": extensionID, "windowId": "window-1", "origin": shopOrigin,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("take the window token = %d %v, want 201", status, window)
	}
	token, _ := window["token"].(string)
	if token == "" {
		t.Fatalf("the window token response carries no token: %v", window)
	}

	// A valid token reads inside the declared range and the issue-time range.
	status, body = call(t, server, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, token, nil,
		map[string]string{"Origin": shopOrigin})
	if status != http.StatusOK {
		t.Fatalf("in-scope list = %d %v, want 200", status, body)
	}
	if count(t, body) != 1 {
		t.Fatalf("in-scope list items = %v, want the one application the member can read", body)
	}
	if status, body := call(t, server, http.MethodGet,
		"/api/v1/resources/Application/shop-app?namespace="+harness.Namespace, token, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusOK {
		t.Fatalf("in-scope single read = %d %v, want 200", status, body)
	}
	// An object outside the issue-time range is refused, not hidden.
	if status, body := call(t, server, http.MethodGet,
		"/api/v1/resources/Application/theirs-app?namespace="+harness.Namespace, token, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusForbidden || harness.String(body, "error", "code") != "permission_denied" {
		t.Fatalf("out-of-range single read = %d %v, want 403 permission_denied", status, body)
	}
	// A class the declaration holds for another role is refused as well.
	if status, body := call(t, server, http.MethodGet,
		"/api/v1/resources/Widget?apiVersion="+widgetsGroup+"/v1&namespace="+harness.Namespace, token, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusForbidden {
		t.Fatalf("a class the role does not satisfy = %d %v, want 403", status, body)
	}
	// Writes are outside the callable set, and a console cookie never widens the
	// application identity.
	if status, body := call(t, server, http.MethodPost,
		"/api/v1/resources/Widget?apiVersion="+widgetsGroup+"/v1&namespace="+harness.Namespace, token,
		map[string]any{"spec": map[string]any{}}, map[string]string{"Origin": shopOrigin}); status != http.StatusForbidden {
		t.Fatalf("a write with an application token = %d %v, want 403", status, body)
	}
	if status, body := call(t, server, http.MethodGet,
		"/api/v1/resources/Widget?apiVersion="+widgetsGroup+"/v1&namespace="+harness.Namespace, token, nil,
		map[string]string{"Origin": shopOrigin, "Cookie": "kos_session=" + owner}); status != http.StatusForbidden {
		t.Fatalf("a console cookie widened the application identity = %d %v, want 403", status, body)
	}

	// A token the table does not know stays unauthenticated, and the terminal
	// routes keep answering in their own vocabulary.
	if status, body := call(t, server, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, "forged", nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusUnauthorized {
		t.Fatalf("a forged token = %d %v, want 401", status, body)
	}
	if status, body := call(t, server, http.MethodPost,
		"/api/v1/terminal/sessions/window-1/attach", token, map[string]any{}, nil); status != http.StatusUnauthorized ||
		harness.String(body, "error", "code") != "terminal_unauthenticated" {
		t.Fatalf("an application token on a terminal route = %d %v, want the terminal unauthenticated refusal", status, body)
	}

	// Closing the window ends the token at the HTTP boundary.
	if status, body := call(t, server, http.MethodDelete, "/api/v1/app-tokens/window-1", member, nil, nil); status != http.StatusNoContent {
		t.Fatalf("close the window = %d %v, want 204", status, body)
	}
	if status, body := call(t, server, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, token, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusUnauthorized {
		t.Fatalf("a closed window's token = %d %v, want 401", status, body)
	}

	// A sign-out ends the tokens the account holds, with or without a cookie.
	status, window = call(t, server, http.MethodPost, "/api/v1/app-tokens", member, map[string]any{
		"application": "shop", "extension": extensionID, "windowId": "window-2", "origin": shopOrigin,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("take a second window token = %d %v, want 201", status, window)
	}
	second, _ := window["token"].(string)
	if status, body := call(t, server, http.MethodPost, "/api/v1/auth/logout", member, nil, nil); status != http.StatusNoContent {
		t.Fatalf("sign out = %d %v, want 204", status, body)
	}
	for _, headers := range []map[string]string{
		{"Origin": shopOrigin},
		{"Origin": shopOrigin, "Cookie": "kos_session=" + owner},
	} {
		if status, body := call(t, server, http.MethodGet,
			"/api/v1/resources/Application?namespace="+harness.Namespace, second, nil, headers); status != http.StatusUnauthorized {
			t.Fatalf("a signed-out account's token = %d %v, want 401", status, body)
		}
	}

	// The deadline ends a token too.
	status, window = call(t, server, http.MethodPost, "/api/v1/app-tokens", login(t, server, "member", "member-password"), map[string]any{
		"application": "shop", "extension": extensionID, "windowId": "window-3", "origin": shopOrigin,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("take a third window token = %d %v, want 201", status, window)
	}
	third, _ := window["token"].(string)
	clock.Advance(16 * time.Minute)
	if status, body := call(t, server, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, third, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusUnauthorized {
		t.Fatalf("an expired token = %d %v, want 401", status, body)
	}

	// A token table that cannot answer is told apart from one it does not know.
	status, window = call(t, server, http.MethodPost, "/api/v1/app-tokens", login(t, server, "member", "member-password"), map[string]any{
		"application": "shop", "extension": extensionID, "windowId": "window-4", "origin": shopOrigin,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("take a fourth window token = %d %v, want 201", status, window)
	}
	fourth, _ := window["token"].(string)
	if err := windowTokens.Close(); err != nil {
		t.Fatalf("close the token table: %v", err)
	}
	if status, body := call(t, server, http.MethodGet,
		"/api/v1/resources/Application?namespace="+harness.Namespace, fourth, nil,
		map[string]string{"Origin": shopOrigin}); status != http.StatusServiceUnavailable ||
		harness.String(body, "error", "code") != "service_unavailable" {
		t.Fatalf("an unreadable token table = %d %v, want 503 service_unavailable", status, body)
	}
}
