// Package apps projects the window applications installed extensions declare
// (KOSD-C-18, KOSD-C-73). The projection is read-only: installing and removing
// an application happens through its system component (KOSD-C-17), and the
// desktop reads this list to render the launcher and the dock.
package apps

import (
	"encoding/json"
	"net/http"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/identity"
)

// Declarations reports the window applications the active components declare.
type Declarations interface {
	// Apps returns every installed application. A disabled or uninstalled
	// component contributes none.
	Apps() []extension.InstalledApp
}

// Service is the application projection over one declarations source.
type Service struct {
	declarations Declarations
}

// New builds the projection.
func New(declarations Declarations) *Service {
	return &Service{declarations: declarations}
}

// List returns the applications a signed-in session reads (C-59). Every role
// reads the installed set: the projection carries no write path, and a Guest is
// read-only by construction.
func (s *Service) List(session identity.Session) []extension.InstalledApp {
	if s.declarations == nil || session.Username == "" {
		return nil
	}
	return s.declarations.Apps()
}

// API is the HTTP surface of the application projection.
type API struct {
	service *Service
}

// NewAPI builds the HTTP surface over the projection.
func NewAPI(service *Service) *API {
	return &API{service: service}
}

// Handler serves the application routes. It expects the identity middleware in
// front of it: every request carries a resolved session (C-63).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/apps", a.list)
	return mux
}

// list returns the installed applications with their component, phase, digest
// and manifest (C-18, KOSD-C-73).
func (a *API) list(w http.ResponseWriter, r *http.Request) {
	session, ok := identity.FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": a.service.List(session)})
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
