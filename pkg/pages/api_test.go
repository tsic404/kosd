package pages

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
)

// pageFailure is the C-12 failure envelope as a write refusal carries it.
type pageFailure struct {
	Code               string `json:"code"`
	Message            string `json:"message"`
	Stage              string `json:"stage"`
	Retryable          *bool  `json:"retryable"`
	RequiresUserAction *bool  `json:"requiresUserAction"`
	SideEffects        string `json:"sideEffects"`
}

// pageEnvelope is one failure response.
type pageEnvelope struct {
	Error pageFailure `json:"error"`
}

// pageAPI builds the surface both a read-only and a writable page declare.
func pageAPI() *API {
	service := New(declarations{
		{
			ID: "native", Title: "Native", Location: "native", ReadOnly: true,
			ReadOnlyReason: "原生视图只能查看，写操作必须经 kubectl 与 kubeconfig",
			Items:          []extension.PageItem{localItem("fields")},
		},
		{
			ID: "apps", Title: "Apps", Location: "panel",
			Items: []extension.PageItem{{
				ID: "apps", Label: "Apps", Source: extension.PageSourceLocal, Store: "apps", Action: "stop",
			}},
			Actions: []extension.Action{{ID: "stop", Method: http.MethodPost, Path: "/api/v1/action-requests"}},
		},
		{
			ID: ".", Title: "Dot", Location: "dot", ReadOnly: true,
			ReadOnlyReason: "点页面只读，写操作经领域接口",
			Items:          []extension.PageItem{localItem("dot-value")},
		},
		{
			ID: "..", Title: "Dot Dot", Location: "dotdot", ReadOnly: true,
			ReadOnlyReason: "双点页面只读，写操作经领域接口",
			Items:          []extension.PageItem{localItem("dotdot-value")},
		},
	}, Config{})
	return NewAPI(service)
}

// callPage sends one request through the page surface.
func callPage(handler http.Handler, method, path, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// decodePageFailure decodes one failure response.
func decodePageFailure(t *testing.T, recorder *httptest.ResponseRecorder) pageEnvelope {
	t.Helper()
	var envelope pageEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("answered %q: %v", recorder.Body.String(), err)
	}
	return envelope
}

// TestPageReadRoutesRefuseWriteMethods checks KOSD-C-89 and KOSD-AC-81: both
// the collection read route and a single-page read route keep the method-not-
// allowed status and Allow declaration while carrying the C-12 envelope.
func TestPageReadRoutesRefuseWriteMethods(t *testing.T) {
	api := pageAPI()
	handler := api.Handler()
	for _, path := range []string{
		"/api/v1/pages",
		"/api/v1/pages/native",
		"/api/v1/pages/native%2Ffields",
		"/api/v1/pages/%2E",
		"/api/v1/pages/%2E%2E",
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			t.Run(method+" "+path, func(t *testing.T) {
				recorder := callPage(handler, method, path, "")
				if recorder.Code != http.StatusMethodNotAllowed {
					t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
				}
				if allow := recorder.Header().Get("Allow"); allow != "GET, HEAD" {
					t.Fatalf("Allow = %q, want GET, HEAD", allow)
				}
				if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
					t.Fatalf("Content-Type = %q, want application/json", contentType)
				}
				failure := decodePageFailure(t, recorder).Error
				if failure.Code != controlplane.CodePageReadOnly {
					t.Fatalf("code = %q, want the page read-only code", failure.Code)
				}
				if failure.Code == controlplane.CodeReadOnly {
					t.Fatalf("code = %q; a page refusal must differ from the cluster read-only code", failure.Code)
				}
				if failure.Message == "" || failure.Stage == "" || failure.SideEffects == "" ||
					failure.Retryable == nil || failure.RequiresUserAction == nil {
					t.Fatalf("failure = %+v, want every C-12 field", failure)
				}
			})
		}
	}
}

// TestSinglePageWriteRefusalCarriesTheDeclaredReason checks KOSD-C-89's
// message rule: a read-only page states the reason it declared, while a
// writable page points the caller at page actions or the domain API.
func TestSinglePageWriteRefusalCarriesTheDeclaredReason(t *testing.T) {
	handler := pageAPI().Handler()
	recorder := callPage(handler, http.MethodPost, "/api/v1/pages/native", "")
	failure := decodePageFailure(t, recorder).Error
	if !strings.Contains(failure.Message, "原生视图只能查看，写操作必须经 kubectl 与 kubeconfig") {
		t.Fatalf("message = %q, want the declared read-only reason", failure.Message)
	}

	recorder = callPage(handler, http.MethodPost, "/api/v1/pages/apps", "")
	failure = decodePageFailure(t, recorder).Error
	if !strings.Contains(failure.Message, "页面动作") || !strings.Contains(failure.Message, "领域接口") {
		t.Fatalf("message = %q, want the page-action and domain-interface route", failure.Message)
	}
}

// TestPageWriteRefusalDoesNotResolveASession checks KOSD-C-89's ordering rule:
// the refusal is the same with no session and with a session, and the handler
// behind the refusal never runs.
func TestPageWriteRefusalDoesNotResolveASession(t *testing.T) {
	reached := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	})
	handler := pageAPI().RefuseWrites(next)

	for _, path := range []string{
		"/api/v1/pages/native",
		"/api/v1/pages/native%2Ffields",
		"/api/v1/pages/%2E",
		"/api/v1/pages/%2E%2E",
	} {
		unauthenticated := callPage(handler, http.MethodPost, path, "")
		authenticated := callPage(handler, http.MethodPost, path, "Bearer a-session")
		if unauthenticated.Code != authenticated.Code ||
			unauthenticated.Body.String() != authenticated.Body.String() ||
			unauthenticated.Header().Get("Allow") != authenticated.Header().Get("Allow") {
			t.Fatalf("%s refusal changed with the session:\nunauthenticated %d %q\nauthenticated %d %q",
				path, unauthenticated.Code, unauthenticated.Body.String(), authenticated.Code, authenticated.Body.String())
		}
	}
	if reached != 0 {
		t.Fatalf("the session-bearing handler ran %d times for a refused write", reached)
	}
}

// TestLiteralDotSegmentsKeepServeMuxCleaning checks that the route detector does
// not turn the literal dot forms into write refusals: ServeMux still cleans
// them and answers with its canonical redirect.
func TestLiteralDotSegmentsKeepServeMuxCleaning(t *testing.T) {
	handler := pageAPI().Handler()
	cases := []struct {
		path     string
		location string
	}{
		{path: "/api/v1/pages/.", location: "/api/v1/pages"},
		{path: "/api/v1/pages/..", location: "/api/v1"},
	}
	for _, testCase := range cases {
		recorder := callPage(handler, http.MethodPost, testCase.path, "")
		if recorder.Code != http.StatusTemporaryRedirect {
			t.Errorf("POST %s = %d %q, want the existing redirect", testCase.path, recorder.Code, recorder.Body.String())
			continue
		}
		if location := recorder.Header().Get("Location"); location != testCase.location {
			t.Errorf("POST %s Location = %q, want %q", testCase.path, location, testCase.location)
		}
	}
}

// TestReadMethodsStillAnswerAsBefore checks KOSD-C-89's last rule: a read
// without a session still gets the existing unauthenticated envelope.
func TestReadMethodsStillAnswerAsBefore(t *testing.T) {
	recorder := callPage(pageAPI().Handler(), http.MethodGet, "/api/v1/pages", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	failure := decodePageFailure(t, recorder).Error
	if failure.Code != "unauthenticated" || failure.Stage != "authorize" {
		t.Fatalf("failure = %+v, want the existing unauthenticated answer", failure)
	}
}

// TestPageReadRouteUsesServeMuxEscapedSegments checks that an encoded slash
// stays inside the single-page wildcard, as ServeMux matches it, while literal
// separators and the trailing-slash forms stay outside the page routes.
func TestPageReadRouteUsesServeMuxEscapedSegments(t *testing.T) {
	cases := []struct {
		path   string
		id     string
		route  bool
		reason string
	}{
		{path: "/api/v1/pages", route: true},
		{path: "/api/v1/pages/", reason: "ServeMux answers the trailing slash with 404"},
		{path: "/api/v1/pages/native", id: "native", route: true},
		{path: "/api/v1/pages/native%2Ffields", id: "native/fields", route: true},
		{path: "/api/v1/%70%61%67%65%73/native%2Ffields", id: "native/fields", route: true},
		{path: "/api/v1/pages/%2E", id: ".", route: true},
		{path: "/api/v1/pages/%2E%2E", id: "..", route: true},
		{path: "/api/v1/pages/%2e", id: ".", route: true},
		{path: "/api/v1/pages/native%zz", reason: "an invalid escape is a literal segment"},
		{path: "/api/v1/pages/%2F", reason: "a slash-only segment is not a wildcard match"},
		{path: "/api/v1/pages/.", reason: "a literal dot segment is cleaned before matching"},
		{path: "/api/v1/pages/..", reason: "a literal dot segment is cleaned before matching"},
		{path: "/api/v1/pages/native/fields", reason: "a literal slash is a second segment"},
		{path: "/api/v1/pages/native%2Ffields/", reason: "a trailing slash leaves an empty segment"},
		{path: "/api%2Fv1/pages/native", reason: "an encoded slash cannot join the collection prefix"},
	}
	for _, testCase := range cases {
		id, route := pageReadRoute(testCase.path)
		if route != testCase.route || id != testCase.id {
			t.Errorf("pageReadRoute(%q) = (%q, %v), want (%q, %v): %s",
				testCase.path, id, route, testCase.id, testCase.route, testCase.reason)
		}
	}
}
