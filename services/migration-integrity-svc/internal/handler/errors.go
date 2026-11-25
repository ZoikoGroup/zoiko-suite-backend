package handler

import (
	"errors"
	"net/http"

	"go.uber.org/zap"
	"zoiko.io/migration-integrity-svc/internal/authz"
	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/problem"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// errorMapping is one row of the error → HTTP status table.
type errorMapping struct {
	target error
	status int
	code   string
	title  string
}

// errorTable maps every sentinel error the domain, store and authz layers
// return to exactly one RFC 9457 response. Anything not listed is a 500 and
// is logged; a client error must never reach that branch.
var errorTable = []errorMapping{
	{store.ErrJobNotFound, http.StatusNotFound, "job-not-found", "Migration job not found"},
	{store.ErrEntryNotFound, http.StatusNotFound, "audit-entry-not-found", "Audit entry not found"},
	{store.ErrVersionMismatch, http.StatusPreconditionFailed, "version-mismatch", "If-Match precondition failed"},
	{domain.ErrJobArchived, http.StatusConflict, "job-archived", "Migration job is archived"},
	{domain.ErrJobAlreadyArchived, http.StatusConflict, "job-already-archived", "Migration job is already archived"},
	{domain.ErrEntryAlreadyDecided, http.StatusConflict, "audit-entry-already-decided", "Audit entry already dispositioned"},
	{domain.ErrSubmitterCannotDisposition, http.StatusForbidden, "segregation-of-duties", "Segregation of duties"},
	{domain.ErrBlockerOwnerOnly, http.StatusForbidden, "blocker-owner-only", "Only the owner may resolve an MQ0 blocker"},
	{domain.ErrOwnerIsSubmitter, http.StatusUnprocessableEntity, "segregation-of-duties", "Segregation of duties"},
	{domain.ErrBlockerCannotBeWaived, http.StatusUnprocessableEntity, "blocker-not-waivable", "MQ0 blockers cannot be waived"},
	{domain.ErrWaiverExpiryNotInFuture, http.StatusUnprocessableEntity, "waiver-expiry-invalid", "Waiver expiry must be in the future"},
	{authz.ErrAuthorizationDenied, http.StatusForbidden, "not-authorized", "Not authorized"},
	{authz.ErrTenantMissing, http.StatusUnauthorized, "missing-tenant-scope", "Missing tenant scope"},
}

// writeError renders err as a problem response using errorTable. Errors that
// come from the authorization client but are not a decision (network, bad
// response) fail closed with 503.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	for _, mapping := range errorTable {
		if errors.Is(err, mapping.target) {
			problem.Write(w, r, mapping.status, mapping.code, mapping.title, err.Error())
			return
		}
	}
	if errors.Is(err, authz.ErrAuthzServiceUnavailable) {
		h.logger.Error("authorization check failed", zap.Error(err))
		problem.Write(w, r, http.StatusServiceUnavailable, "authorization-unavailable", "Authorization service unavailable",
			"the authorization decision could not be obtained; the request was not performed (fail closed)")
		return
	}
	h.logger.Error("unexpected error", zap.String("path", r.URL.Path), zap.Error(err))
	problem.Write(w, r, http.StatusInternalServerError, "internal-error", "Internal error",
		"the request could not be completed; see service logs with the request id")
}

// writeBadRequest renders a 400 for a malformed or incomplete request.
func writeBadRequest(w http.ResponseWriter, r *http.Request, detail string) {
	problem.Write(w, r, http.StatusBadRequest, "invalid-request", "Invalid request", detail)
}

// writeForbidden renders a 403 for a scope disagreement (tenant or legal
// entity named in the body or header disagrees with the authoritative one).
func writeForbidden(w http.ResponseWriter, r *http.Request, code, detail string) {
	problem.Write(w, r, http.StatusForbidden, code, "Scope mismatch", detail)
}
