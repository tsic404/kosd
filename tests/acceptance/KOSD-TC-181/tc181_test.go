// Package kosdtc181 verifies that both page read routes refuse write methods
// with the control plane's error envelope before session determination, and
// that reads keep their existing success shape (KOSD-AC-81, KOSD-TC-181).
package kosdtc181

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	ownerName       = "owner"
	ownerPassword   = "owner-password"
	nativePage      = "native"
	slashPage       = "native/fields"
	appsPage        = "apps"
	dotPage         = "."
	dotDotPage      = ".."
	nativeReason    = "原生视图只能查看，写操作必须经 kubectl 与 kubeconfig"
	slashReason     = "原生字段视图只能查看，写操作必须经领域接口"
	dotReason       = "点页面只读，写操作经领域接口"
	dotDotReason    = "双点页面只读，写操作经领域接口"
	pageReadOnly    = "page_read_only"
	clusterReadOnly = "read_only"
)

// declaration declares four read-only pages and one writable page.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "demo.pages",
		Version:    "1.0.0",
		APIGroup:   "pages.example.test",
		Contributions: extension.Contributions{
			Pages: []extension.Page{
				{
					ID: nativePage, Title: "Native", Location: "native", ReadOnly: true,
					ReadOnlyReason: nativeReason,
					Items: []extension.PageItem{{
						ID: "fields", Label: "Fields", Source: extension.PageSourceLocal, Store: "native.fields",
					}},
				},
				{
					ID: slashPage, Title: "Native Fields", Location: "native/fields", ReadOnly: true,
					ReadOnlyReason: slashReason,
					Items: []extension.PageItem{{
						ID: "detail", Label: "Detail", Source: extension.PageSourceLocal, Store: "native.fields.detail",
					}},
				},
				{
					ID: dotPage, Title: "Dot", Location: "dot", ReadOnly: true,
					ReadOnlyReason: dotReason,
					Items: []extension.PageItem{{
						ID: "value", Label: "Value", Source: extension.PageSourceLocal, Store: "dot.value",
					}},
				},
				{
					ID: dotDotPage, Title: "Dot Dot", Location: "dotdot", ReadOnly: true,
					ReadOnlyReason: dotDotReason,
					Items: []extension.PageItem{{
						ID: "value", Label: "Value", Source: extension.PageSourceLocal, Store: "dotdot.value",
					}},
				},
				{
					ID: appsPage, Title: "Apps", Location: "panel",
					Items: []extension.PageItem{{
						ID: "apps", Label: "Apps", Source: extension.PageSourceLocal, Store: "apps", Action: "stop",
					}},
					Actions: []extension.Action{{
						ID: "stop", Method: http.MethodPost, Path: "/api/v1/action-requests",
					}},
				},
			},
		},
	}
}

// response is one answer from the composed shell.
type response struct {
	status int
	header http.Header
	body   []byte
}

// call sends one request with an optional session.
func call(t *testing.T, shell *harness.Shell, method, path, token string) response {
	t.Helper()
	request, err := http.NewRequest(method, shell.Accounts.HTTP.URL+path, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	answer, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return response{status: answer.StatusCode, header: answer.Header.Clone(), body: body}
}

// decode decodes one JSON object.
func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	decoded := map[string]any{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return decoded
}

// failure reads the C-12 failure from one response.
func failure(t *testing.T, answer response) map[string]any {
	t.Helper()
	carried, ok := decode(t, answer.body)["error"].(map[string]any)
	if !ok {
		t.Fatalf("answer carries no error object: %q", answer.body)
	}
	return carried
}

// TestTC181PageWriteRefusal checks both page routes against both callers and
// compares the page projection before and after.
func TestTC181PageWriteRefusal(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	installation := cluster.InstallPackage(t, "pages", declaration(), extension.MemoryArtifacts{})
	shell.Use(installation)
	owner := shell.SignInOwner(t, ownerName, ownerPassword)

	before := map[string]map[string]any{}
	readPaths := []string{
		"/api/v1/pages",
		"/api/v1/pages/" + nativePage,
		"/api/v1/pages/native%2Ffields",
		"/api/v1/pages/%2E",
		"/api/v1/pages/%2E%2E",
		"/api/v1/pages/" + appsPage,
	}
	for _, path := range readPaths {
		answer := call(t, shell, http.MethodGet, path, owner)
		if answer.status != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, answer.status, answer.body)
		}
		before[path] = decode(t, answer.body)
	}

	writePaths := []string{
		"/api/v1/pages",
		"/api/v1/pages/" + nativePage,
		"/api/v1/pages/native%2Ffields",
		"/api/v1/pages/%2E",
		"/api/v1/pages/%2E%2E",
		"/api/v1/pages/" + appsPage,
	}
	for _, path := range writePaths {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			unauthenticated := call(t, shell, method, path, "")
			authenticated := call(t, shell, method, path, owner)
			for name, answer := range map[string]response{"unauthenticated": unauthenticated, "authenticated": authenticated} {
				if answer.status != http.StatusMethodNotAllowed {
					t.Fatalf("%s %s %s = %d %s", name, method, path, answer.status, answer.body)
				}
				if allow := answer.header.Get("Allow"); allow != "GET, HEAD" {
					t.Fatalf("%s %s Allow = %q, want GET, HEAD", method, path, allow)
				}
				carried := failure(t, answer)
				if code := harness.String(carried, "code"); code != pageReadOnly {
					t.Fatalf("%s %s code = %q, want %q", method, path, code, pageReadOnly)
				}
				if code := harness.String(carried, "code"); code == clusterReadOnly {
					t.Fatalf("%s %s code = %q, want a code distinct from cluster read-only", method, path, code)
				}
				for _, field := range []string{"message", "stage", "retryable", "requiresUserAction", "sideEffects"} {
					if _, present := carried[field]; !present {
						t.Fatalf("%s %s failure is missing %s: %v", method, path, field, carried)
					}
				}
				message := harness.String(carried, "message")
				switch path {
				case "/api/v1/pages":
					if !strings.Contains(message, "页面读取面只读") {
						t.Fatalf("collection message = %q, want the page-read surface wording", message)
					}
				case "/api/v1/pages/" + nativePage:
					if !strings.Contains(message, nativeReason) {
						t.Fatalf("single-page message = %q, want the declared reason", message)
					}
				case "/api/v1/pages/native%2Ffields":
					if !strings.Contains(message, slashReason) {
						t.Fatalf("encoded single-page message = %q, want the declared reason", message)
					}
				case "/api/v1/pages/%2E":
					if !strings.Contains(message, dotReason) {
						t.Fatalf("encoded dot message = %q, want the declared reason", message)
					}
				case "/api/v1/pages/%2E%2E":
					if !strings.Contains(message, dotDotReason) {
						t.Fatalf("encoded dot-dot message = %q, want the declared reason", message)
					}
				case "/api/v1/pages/" + appsPage:
					if !strings.Contains(message, "页面动作") || !strings.Contains(message, "领域接口") {
						t.Fatalf("writable-page message = %q, want the page-action and domain-interface route", message)
					}
				}
			}
			if !bytes.Equal(unauthenticated.body, authenticated.body) {
				t.Fatalf("%s %s answer changed with the session:\n%s\n%s", method, path, unauthenticated.body, authenticated.body)
			}
		}
	}

	for path, want := range before {
		answer := call(t, shell, http.MethodGet, path, owner)
		if answer.status != http.StatusOK {
			t.Fatalf("GET %s after refusals = %d %s", path, answer.status, answer.body)
		}
		if got := decode(t, answer.body); !reflect.DeepEqual(got, want) {
			t.Fatalf("page projection at %s changed after refused writes", path)
		}
	}
}
