package upgrade

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/identity"
)

// maxBodyBytes bounds one request body, matching the control plane's limit.
const maxBodyBytes = 1 << 20

// API is the HTTP surface of the upgrade preflight record domain (C-88). It
// expects the identity middleware in front of it: every request carries a
// resolved session (C-63).
type API struct {
	service *Service
}

// NewAPI builds the HTTP surface over the domain.
func NewAPI(service *Service) *API { return &API{service: service} }

// Handler serves the record routes: the preflight write, the upgrade write, and
// the two reads the update page and a case use (C-88).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/upgrade-preflights", a.list)
	mux.HandleFunc("POST /api/v1/upgrade-preflights", a.preflight)
	mux.HandleFunc("GET /api/v1/upgrade-preflights/{id}", a.record)
	mux.HandleFunc("POST /api/v1/upgrade-requests", a.upgrade)
	return mux
}

// list returns every record, newest first, with the freshness marker C-13
// fixes computed at read time.
func (a *API) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := identity.FromContext(r.Context()); !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	records, err := a.service.Records(r.Context())
	if err != nil {
		controlplane.WriteError(w, databaseUnavailable())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": records})
}

// record returns one record by identity, so a superseded record stays readable
// (C-88).
func (a *API) record(w http.ResponseWriter, r *http.Request) {
	if _, ok := identity.FromContext(r.Context()); !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	id := r.PathValue("id")
	records, err := a.service.Records(r.Context())
	if err != nil {
		controlplane.WriteError(w, databaseUnavailable())
		return
	}
	for _, record := range records {
		if record.ID == id {
			writeJSON(w, http.StatusOK, record)
			return
		}
	}
	controlplane.WriteError(w, recordNotFound(id))
}

// preflight runs the preflight and stores the request with its result (C-88).
func (a *API) preflight(w http.ResponseWriter, r *http.Request) {
	session, ok := identity.FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	var request PreflightRequest
	if failure := decodeBody(r, &request); failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	record, failure := a.service.Preflight(r.Context(), session, request)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"accepted": true, "record": record})
}

// upgrade records one upgrade that references a stored preflight record
// (C-88).
func (a *API) upgrade(w http.ResponseWriter, r *http.Request) {
	session, ok := identity.FromContext(r.Context())
	if !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	var request UpgradeRequest
	if failure := decodeBody(r, &request); failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	record, failure := a.service.Upgrade(r.Context(), session, request)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "record": record})
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

// decodeBody reads a bounded, strict JSON body.
func decodeBody(r *http.Request, target any) *controlplane.Error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return requestInvalid("读取请求体失败")
	}
	if len(body) > maxBodyBytes {
		return requestInvalid("请求体超过 1 MiB")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return requestInvalid("请求体不能为空")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return requestInvalid("请求体不是合法的 JSON 对象")
	}
	return nil
}

// requestInvalid rejects a body the caller can correct.
func requestInvalid(message string) *controlplane.Error {
	return &controlplane.Error{
		Code:               controlplane.CodeInvalidRequest,
		Message:            message,
		Stage:              controlplane.StageValidate,
		RequiresUserAction: true,
		SideEffects:        controlplane.SideEffectsNone,
	}
}

// writeJSON writes one success response body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
