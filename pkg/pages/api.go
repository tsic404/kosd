package pages

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/identity"
)

// API is the HTTP surface of the page shell.
type API struct {
	service *Service
}

// NewAPI builds the HTTP surface over the shell.
func NewAPI(service *Service) *API { return &API{service: service} }

// RefuseWrites answers write methods on the page read routes before any session
// is resolved, then hands every other request to next (KOSD-C-89). Mount it in
// front of the identity middleware, where it can keep the refusal independent
// of the caller.
func (a *API) RefuseWrites(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, pageRoute := pageReadRoute(r.URL.EscapedPath())
		if !pageRoute || isReadMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Allow", "GET, HEAD")
		controlplane.WriteError(w, a.writeRefusal(id))
	})
}

// Handler serves the page routes. It expects the identity middleware in front
// of it: every request carries a resolved session (C-63).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages", a.list)
	mux.HandleFunc("GET /api/v1/pages/{id}", a.read)
	return a.RefuseWrites(mux)
}

// pageReadRoute reports whether escapedPath addresses the page read surface
// and, for the single-page route, the page identity. ServeMux matches on the
// escaped path and unescapes each segment; a percent-encoded slash therefore
// stays inside one wildcard segment instead of becoming a path separator.
// Only a literal dot segment is cleaned before matching, so an encoded dot
// remains a page identity.
func pageReadRoute(escapedPath string) (string, bool) {
	segments := strings.Split(escapedPath, "/")
	if len(segments) < 4 || len(segments) > 5 || segments[0] != "" {
		return "", false
	}
	for index, want := range []string{"api", "v1", "pages"} {
		segment, ok := decodeSegment(segments[index+1])
		if !ok || segment != want {
			return "", false
		}
	}
	switch len(segments) {
	case 4:
		return "", true
	case 5:
		id, ok := decodeSegment(segments[4])
		if !ok {
			return "", false
		}
		if id == "" || id == "/" || segments[4] == "." || segments[4] == ".." {
			return "", false
		}
		return id, true
	}
	return "", false
}

// decodeSegment follows ServeMux's segment decoding: an encoded slash stays
// inside one segment, and an invalid escape cannot name a page route because
// ServeMux treats it as a literal segment.
func decodeSegment(segment string) (string, bool) {
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return "", false
	}
	return decoded, true
}

// isReadMethod reports the methods the page read routes serve. Everything else
// is a write or an unsupported method, and the surface refuses it uniformly.
func isReadMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// writeRefusal is the C-12 answer to a write method on a page read route. It
// names the page's declared read-only reason when there is one; otherwise it
// points the caller at page actions or the domain API (KOSD-C-89).
func (a *API) writeRefusal(id string) *controlplane.Error {
	message := "页面读取面只读，写操作经页面动作或对应领域接口提交"
	if id != "" {
		if page, found := a.service.declaredPage(id); found {
			if page.ReadOnly && strings.TrimSpace(page.ReadOnlyReason) != "" {
				message = "页面 " + id + " 只读：" + page.ReadOnlyReason
			} else {
				message = "页面 " + id + " 的读取面只读，写操作经页面动作或对应领域接口提交"
			}
		}
	}
	return &controlplane.Error{
		Code:               controlplane.CodePageReadOnly,
		Message:            message,
		Stage:              controlplane.StageValidate,
		RequiresUserAction: true,
		SideEffects:        controlplane.SideEffectsNone,
	}
}

// list returns the pages the caller's role may see (C-22).
func (a *API) list(w http.ResponseWriter, r *http.Request) {
	session, ok := identity.FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": a.service.List(session)})
}

// read renders one page, trimmed to the caller's role (C-22, C-53).
func (a *API) read(w http.ResponseWriter, r *http.Request) {
	session, ok := identity.FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	page, failure := a.service.Read(r.Context(), session, r.PathValue("id"))
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// unauthenticated is the answer to a request without a session (C-63).
func unauthenticated() *controlplane.Error {
	return &controlplane.Error{
		Code:               controlplane.CodeUnauthenticated,
		Message:            "会话无效或已过期，请重新登录",
		Stage:              controlplane.StageAuthorize,
		RequiresUserAction: true,
		SideEffects:        controlplane.SideEffectsNone,
	}
}

// writeJSON writes a JSON body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
