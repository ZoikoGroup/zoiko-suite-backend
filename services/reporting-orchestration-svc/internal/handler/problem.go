package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"zoiko.io/reporting-orchestration-svc/internal/authz"
	"zoiko.io/reporting-orchestration-svc/internal/domain"
)

// Problem is an RFC 9457 "problem details" body. Code is a stable,
// machine-readable reporting error code (Reporting spec §3 "Errors").
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Code     string `json:"code"`
	Instance string `json:"instance,omitempty"`
}

// Stable problem codes returned by this service.
const (
	CodeValidationFailed      = "VALIDATION_FAILED"
	CodeMalformedJSON         = "MALFORMED_JSON"
	CodeInvalidID             = "INVALID_ID"
	CodeInvalidQuery          = "INVALID_QUERY"
	CodeInvalidIfMatch        = "INVALID_IF_MATCH"
	CodePayloadTooLarge       = "PAYLOAD_TOO_LARGE"
	CodeTenantRequired        = "TENANT_REQUIRED"
	CodePrincipalRequired     = "PRINCIPAL_REQUIRED"
	CodeTenantMismatch        = "TENANT_MISMATCH"
	CodeLegalEntityMismatch   = "LEGAL_ENTITY_MISMATCH"
	CodeAuthorizationDenied   = "AUTHORIZATION_DENIED"
	CodeSelfApproval          = "SELF_APPROVAL_FORBIDDEN"
	CodeNotFound              = "NOT_FOUND"
	CodeIllegalTransition     = "ILLEGAL_TRANSITION"
	CodeDefinitionNotActive   = "DEFINITION_NOT_ACTIVE"
	CodeExportInvalidState    = "EXPORT_INVALID_STATE"
	CodeExportSealedImmutable = "EXPORT_SEALED_IMMUTABLE"
	CodeBuildInProgress       = "BUILD_IN_PROGRESS"
	CodeManifestRequired      = "MANIFEST_REQUIRED"
	CodeLegalHoldActive       = "LEGAL_HOLD_ACTIVE"
	CodeIdempotencyKeyReused  = "IDEMPOTENCY_KEY_REUSED"
	CodeIdempotencyInFlight   = "IDEMPOTENCY_IN_FLIGHT"
	CodeVersionMismatch       = "VERSION_MISMATCH"
	CodeArchiveUnavailable    = "ARCHIVE_UNAVAILABLE"
	CodeArchiveDigestInvalid  = "ARCHIVE_DIGEST_INVALID"
	CodeAuthzUnavailable      = "AUTHZ_UNAVAILABLE"
	CodeRetentionUnavailable  = "RETENTION_UNAVAILABLE"
	CodeInternalError         = "INTERNAL_ERROR"
)

// writeProblem writes an application/problem+json response.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	problem := Problem{
		Type:     "https://errors.zoiko.io/reporting/" + strings.ToLower(strings.ReplaceAll(code, "_", "-")),
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   detail,
		Code:     code,
		Instance: r.URL.Path,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

// writeJSON writes a successful JSON response.
func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// domainErrorMapping maps each domain error to its HTTP status and code.
var domainErrorMapping = []struct {
	err    error
	status int
	code   string
}{
	{domain.ErrDefinitionNotFound, http.StatusNotFound, CodeNotFound},
	{domain.ErrRunNotFound, http.StatusNotFound, CodeNotFound},
	{domain.ErrExportNotFound, http.StatusNotFound, CodeNotFound},
	{domain.ErrIllegalTransition, http.StatusConflict, CodeIllegalTransition},
	{domain.ErrDefinitionNotActive, http.StatusConflict, CodeDefinitionNotActive},
	{domain.ErrExportInvalidState, http.StatusConflict, CodeExportInvalidState},
	{domain.ErrBuildInProgress, http.StatusConflict, CodeBuildInProgress},
	{domain.ErrExportNotRedactable, http.StatusConflict, CodeExportSealedImmutable},
	{domain.ErrManifestRequired, http.StatusConflict, CodeManifestRequired},
	{domain.ErrLegalHoldActive, http.StatusConflict, CodeLegalHoldActive},
	{domain.ErrIdempotencyMismatch, http.StatusUnprocessableEntity, CodeIdempotencyKeyReused},
	{domain.ErrIdempotencyInFlight, http.StatusConflict, CodeIdempotencyInFlight},
	{domain.ErrVersionMismatch, http.StatusPreconditionFailed, CodeVersionMismatch},
	{domain.ErrExportSelfApproval, http.StatusForbidden, CodeSelfApproval},
	{domain.ErrArchiveFetchFailed, http.StatusFailedDependency, CodeArchiveUnavailable},
	{domain.ErrArchiveDigestInvalid, http.StatusFailedDependency, CodeArchiveDigestInvalid},
	{domain.ErrRetentionServiceDown, http.StatusServiceUnavailable, CodeRetentionUnavailable},
}

// writeDomainError maps err to a problem response. Unknown errors are logged
// and returned as a generic 500 that never leaks internals.
func (h *Handler) writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	for _, mapping := range domainErrorMapping {
		if errors.Is(err, mapping.err) {
			writeProblem(w, r, mapping.status, mapping.code, mapping.err.Error())
			return
		}
	}
	h.logger.Error("unexpected error", zap.String("path", r.URL.Path), zap.Error(err))
	writeProblem(w, r, http.StatusInternalServerError, CodeInternalError, "an unexpected error occurred")
}

// writeAuthzError maps an authz.CheckAllowed error. A denial is 403; any
// other failure (including authorization-svc being unreachable) is 503 —
// fail closed, never allow silently.
func (h *Handler) writeAuthzError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeProblem(w, r, http.StatusForbidden, CodeAuthorizationDenied, "authorization denied")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	writeProblem(w, r, http.StatusServiceUnavailable, CodeAuthzUnavailable, "authorization service unavailable")
}
