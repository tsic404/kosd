package controlplane

import (
	"fmt"
	"net/http"
)

// Stage names the step a request failed at (C-12).
type Stage string

const (
	// StageValidate means the request body or parameters were rejected.
	StageValidate Stage = "validate"
	// StageAuthorize means the caller may not perform the request.
	StageAuthorize Stage = "authorize"
	// StageCapability means the object class needs an extension this control
	// plane has not loaded.
	StageCapability Stage = "capability"
	// StagePlan means computing a plan failed.
	StagePlan Stage = "plan"
	// StageApply means the cluster refused or failed a write.
	StageApply Stage = "apply"
	// StageRead means reading the cluster failed.
	StageRead Stage = "read"
	// StageConnect means establishing or driving a terminal session failed
	// after the caller was authorized (KOSD-C-90, KOSD-C-93).
	StageConnect Stage = "connect"
)

// SideEffects reports whether a failed request left objects behind (C-12).
type SideEffects string

const (
	// SideEffectsNone means nothing was written.
	SideEffectsNone SideEffects = "none"
	// SideEffectsApplied means objects were written before the failure; Applied
	// lists them so the caller knows what to inspect.
	SideEffectsApplied SideEffects = "applied"
)

// Error codes are the stable part of a failure: clients branch on the code,
// never on the message (C-12). A code never changes meaning within a major
// version.
const (
	// CodeInvalidRequest marks a malformed request.
	CodeInvalidRequest = "invalid_request"
	// CodeInvalidField marks a field the model does not define or a value it
	// rejects.
	CodeInvalidField = "invalid_field"
	// CodeNotFound marks a missing object or resource.
	CodeNotFound = "not_found"
	// CodePermissionDenied marks a request the caller may not make (C-59).
	CodePermissionDenied = "permission_denied"
	// CodeCapabilityMissing marks an object class whose extension is not
	// installed, so the control plane cannot serve it (C-19, C-21).
	CodeCapabilityMissing = "capability_missing"
	// CodeObjectExists marks a create whose name is taken by a different
	// desired state.
	CodeObjectExists = "object_exists"
	// CodeConflict marks a write whose precondition no longer holds.
	CodeConflict = "conflict"
	// CodeWriteFailed marks a write the cluster refused or failed.
	CodeWriteFailed = "write_failed"
	// CodeInternal marks a failure on this side of the API.
	CodeInternal = "internal"
	// CodeUnauthenticated marks a request that carries no usable session
	// (C-63).
	CodeUnauthenticated = "unauthenticated"
	// CodeLoginFailed marks a login that names no usable account. The three
	// ways to fail a login share this one code (C-63).
	CodeLoginFailed = "login_failed"
	// CodeAccountLocked marks a login name inside its lock window (C-63).
	CodeAccountLocked = "account_locked"
	// CodePasswordChangeRequired marks a first-login session that has not
	// changed its one-time password yet (C-63).
	CodePasswordChangeRequired = "password_change_required"
	// CodeDatabaseUnavailable marks a request the control-plane database
	// cannot serve right now (C-61).
	CodeDatabaseUnavailable = "database_unavailable"
	// CodeAccountExists marks a create whose login name is taken.
	CodeAccountExists = "account_exists"
	// CodeLastOwner marks a change that would leave no usable Owner (C-63).
	CodeLastOwner = "last_owner"
	// CodeAccountHasObjects marks a delete that still owns objects (C-60,
	// C-63); OwnedObjects lists them.
	CodeAccountHasObjects = "account_has_objects"
	// CodeOwnershipCheckFailed marks a delete whose ownership check could not
	// run, so the account is kept rather than stranding objects (C-60).
	CodeOwnershipCheckFailed = "ownership_check_failed"
	// CodeClusterUnavailable marks a read the cluster API could not answer, so
	// an empty result would claim there was nothing to read (C-64).
	CodeClusterUnavailable = "cluster_unavailable"
	// CodeClusterCredentialsInvalid marks a read the cluster API refused because
	// the control plane's credential is not usable (C-64).
	CodeClusterCredentialsInvalid = "cluster_credentials_invalid"
	// CodeBootstrapClosed marks a first-Owner claim the window does not serve:
	// none was opened, the password was already delivered, or the deadline
	// passed (C-68).
	CodeBootstrapClosed = "bootstrap_closed"
	// CodeReadOnly marks a write refused because the cluster lost its write
	// quorum and serves read-only (C-56).
	CodeReadOnly = "read_only"
	// CodePageReadOnly marks a write refused on a page read route: the page
	// surface itself is read-only (KOSD-C-89).
	CodePageReadOnly = "page_read_only"
	// CodeStorageFull marks a volume refused because the backing disk is full
	// (C-56).
	CodeStorageFull = "storage_full"
	// CodeTerminalUnauthenticated marks a terminal request without a usable
	// console session or terminal token (KOSD-C-91).
	CodeTerminalUnauthenticated = "terminal_unauthenticated"
	// CodeTerminalForbidden marks a terminal request whose token binding does
	// not match, or a target list and audit read the caller may not make
	// (KOSD-C-91, KOSD-C-95, KOSD-C-96).
	CodeTerminalForbidden = "terminal_forbidden"
	// CodeTerminalTargetUnavailable marks a terminal target the caller may not
	// reach or that does not exist; the two cases answer alike (KOSD-C-93).
	CodeTerminalTargetUnavailable = "terminal_target_unavailable"
	// CodeTerminalNodeUnreachable marks an authorized node whose execution
	// service cannot be reached (KOSD-C-93).
	CodeTerminalNodeUnreachable = "terminal_node_unreachable"
	// CodeTerminalExecutorUnavailable marks an authorized target whose executor
	// is not available (KOSD-C-93).
	CodeTerminalExecutorUnavailable = "terminal_executor_unavailable"
	// CodeTerminalTargetEnded marks an authorized target that already ended
	// (KOSD-C-93).
	CodeTerminalTargetEnded = "terminal_target_ended"
	// CodeTerminalSessionLimit marks a create beyond the account or target
	// bound (KOSD-C-94).
	CodeTerminalSessionLimit = "terminal_session_limit"
	// CodeTerminalCredentialBoundaryUnavailable marks a target whose credential
	// boundary cannot be guaranteed (KOSD-C-93, ADR-043).
	CodeTerminalCredentialBoundaryUnavailable = "terminal_credential_boundary_unavailable"
	// CodeTerminalAuditUnavailable marks the terminal audit trail being
	// unavailable for a write or a read (KOSD-C-92, KOSD-C-96).
	CodeTerminalAuditUnavailable = "terminal_audit_unavailable"
	// CodeTerminalTargetSourceUnavailable marks the node inventory source
	// being unavailable, so an empty list would misstate the cluster
	// (KOSD-C-95).
	CodeTerminalTargetSourceUnavailable = "terminal_target_source_unavailable"
)

// Error is the failure envelope every non-2xx response carries (C-12). The
// fields answer what failed, where, what it touched, and what the caller can do
// about it.
type Error struct {
	// Code is stable across the major version.
	Code string `json:"code"`
	// Message is written for a user, not for a log.
	Message string `json:"message"`
	// Stage is the step that failed.
	Stage Stage `json:"stage"`
	// Object names the object the failure is about, when there is one.
	Object *ObjectRef `json:"object,omitempty"`
	// Retryable tells whether repeating the same request can succeed.
	Retryable bool `json:"retryable"`
	// RequiresUserAction tells whether the failure waits on a decision or a
	// repair only a user can make.
	RequiresUserAction bool `json:"requiresUserAction"`
	// SideEffects tells whether the failure left objects behind.
	SideEffects SideEffects `json:"sideEffects"`
	// Applied lists the objects written before a failed write stopped.
	Applied []ObjectRef `json:"applied,omitempty"`
	// Current is the object as the cluster holds it now; a conflict carries it so
	// the caller can reconcile instead of guessing (C-05).
	Current map[string]any `json:"current,omitempty"`
	// RetryAfterSeconds is how long a caller must wait before repeating a
	// request that is locked or unavailable (C-63).
	RetryAfterSeconds int64 `json:"retryAfterSeconds,omitempty"`
	// OwnedObjects lists the objects that keep an account from being deleted
	// (C-60, C-63).
	OwnedObjects []ObjectRef `json:"ownedObjects,omitempty"`
}

// Error renders the code and message for wrapping in other errors.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// status maps a stable code to the HTTP status the response carries.
func (e *Error) status() int {
	switch e.Code {
	case CodeInvalidRequest, CodeInvalidField:
		return http.StatusBadRequest
	case CodeUnauthenticated, CodeLoginFailed, CodeTerminalUnauthenticated:
		return http.StatusUnauthorized
	case CodeNotFound:
		return http.StatusNotFound
	case CodePermissionDenied, CodePasswordChangeRequired, CodeBootstrapClosed,
		CodeTerminalForbidden, CodeTerminalTargetUnavailable:
		return http.StatusForbidden
	case CodeAccountLocked:
		return http.StatusLocked
	case CodeTerminalSessionLimit:
		return http.StatusTooManyRequests
	case CodePageReadOnly:
		return http.StatusMethodNotAllowed
	case CodeCapabilityMissing:
		return http.StatusUnprocessableEntity
	case CodeObjectExists, CodeConflict, CodeAccountExists, CodeLastOwner, CodeAccountHasObjects,
		CodeTerminalTargetEnded:
		return http.StatusConflict
	case CodeWriteFailed, CodeClusterUnavailable, CodeClusterCredentialsInvalid:
		return http.StatusBadGateway
	case CodeDatabaseUnavailable, CodeOwnershipCheckFailed, CodeReadOnly, CodeStorageFull,
		CodeTerminalNodeUnreachable, CodeTerminalExecutorUnavailable,
		CodeTerminalCredentialBoundaryUnavailable, CodeTerminalAuditUnavailable,
		CodeTerminalTargetSourceUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// terminalFailure builds the terminal failure envelope (KOSD-C-12,
// KOSD-C-93). A terminal failure is retryable only when repeating the request
// can help; a capacity, authorization or boundary decision is not.
func terminalFailure(code string, stage Stage, message string) *Error {
	return &Error{
		Code:        code,
		Message:     message,
		Stage:       stage,
		SideEffects: SideEffectsNone,
	}
}

// about attaches the object the failure concerns.
func (e *Error) about(ref ObjectRef) *Error {
	e.Object = &ref
	return e
}

// invalid requests a corrected request body; the caller can fix it alone.
func invalid(message string) *Error {
	return &Error{
		Code:               CodeInvalidRequest,
		Message:            message,
		Stage:              StageValidate,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}
}

// invalidField reports a field the model does not define or a value it rejects.
func invalidField(message string) *Error {
	return &Error{
		Code:               CodeInvalidField,
		Message:            message,
		Stage:              StageValidate,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}
}

// notFound reports a missing object; repeating the request cannot help.
func notFound(message string) *Error {
	return &Error{
		Code:               CodeNotFound,
		Message:            message,
		Stage:              StageValidate,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}
}

// conflict reports a write whose precondition no longer holds and carries the
// object as it stands now (C-05).
func conflict(ref ObjectRef, message string, current map[string]any) *Error {
	return &Error{
		Code:        CodeConflict,
		Message:     message,
		Stage:       StageApply,
		Object:      new(ref),
		Retryable:   true,
		SideEffects: SideEffectsNone,
		Current:     current,
	}
}

// internal reports a failure on this side; repeating the request may work.
func internal(stage Stage, message string) *Error {
	return &Error{
		Code:        CodeInternal,
		Message:     message,
		Stage:       stage,
		Retryable:   true,
		SideEffects: SideEffectsNone,
	}
}

// clusterUnavailable reports a cluster read the API could not answer, so a
// caller never reads an empty result as "there is nothing" (C-64).
func clusterUnavailable(message string) *Error {
	return &Error{
		Code:        CodeClusterUnavailable,
		Message:     message,
		Stage:       StageRead,
		Retryable:   true,
		SideEffects: SideEffectsNone,
	}
}

// readOnlyFailure reports a write refused because the cluster lost its write
// quorum: reads continue from cache, writes wait (C-56).
func readOnlyFailure(reason string) *Error {
	message := "集群处于只读状态，写操作被拒绝"
	if reason != "" {
		message += "：" + reason
	}
	return &Error{
		Code:              CodeReadOnly,
		Message:           message,
		Stage:             StageApply,
		Retryable:         true,
		RetryAfterSeconds: 30,
		SideEffects:       SideEffectsNone,
	}
}

// storageFullFailure reports a volume refused because the backing disk is full;
// a retry alone cannot fix it (C-56).
func storageFullFailure(reason string) *Error {
	message := "磁盘已满，不能新建卷"
	if reason != "" {
		message += "：" + reason
	}
	return &Error{
		Code:               CodeStorageFull,
		Message:            message,
		Stage:              StageValidate,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}
}

// clusterCredentialsInvalid reports a cluster read the API refused because this
// control plane's credential is unusable; a retry alone cannot fix it (C-64).
func clusterCredentialsInvalid(message string) *Error {
	return &Error{
		Code:               CodeClusterCredentialsInvalid,
		Message:            message,
		Stage:              StageRead,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}
}
