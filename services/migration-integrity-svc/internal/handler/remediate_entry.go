package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// RemediateEntry handles POST /v1/migrations/{id}/audit/{entryId}/remediate.
//
// It records the disposition of one quarantine item: QUARANTINED →
// REMEDIATED (default) or QUARANTINED → WAIVED (time-limited exception),
// with mandatory reason and evidence reference, and records who decided and
// when (remediated_by = X-Principal-Id).
//
// Validation, in order:
//   - X-Principal-Id present                                       401
//   - {id} and {entryId} are UUIDs                                 400
//   - Idempotency-Key replay: same request → stored 200; reuse → 409
//   - body well-formed, no unknown fields                          400
//   - body tenant_id (optional) equals X-Tenant-Id                 403
//   - disposition, reason, evidence ref, waiver expiry shape       400
//   - If-Match (optional) is a version number                      400
//   - job exists in the tenant                                     404 (also cross-tenant)
//   - X-Legal-Entity-Id equals the job's legal entity              403
//   - AUDIT_ENTRY_REMEDIATE at the job's legal entity              403 / 503
//   - entry exists on this job                                     404
//   - job not archived; entry still QUARANTINED                    409
//   - submitter of the run cannot disposition it (SoD)             403
//   - MQ0 only by its owner                                        403
//   - MQ0 cannot be WAIVED; waiver expiry must be in the future    422
//   - If-Match equals the job's current version                    412
//
// Writes: the entry's decision columns, the job's version,
// migration_idempotency_keys — one transaction.
// Events: migration.audit_entry_remediated or migration.audit_entry_waived.
// Success: 200 with the entry; ETag = the job's new version.
func (h *Handler) RemediateEntry(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireCaller(w, r)
	if !ok {
		return
	}
	jobID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}
	entryID, ok := requireUUIDParam(w, r, "entryId")
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	claim := newIdempotencyClaim(r, caller, operationRemediate, body)
	if h.replayIfSeen(w, r, caller, claim) {
		return
	}

	var req domain.RemediateRequest
	if !decodeStrict(w, r, body, &req) {
		return
	}
	if !requireBodyTenantMatches(w, r, req.TenantID, caller.TenantID) {
		return
	}
	if err := req.Validate(); err != nil {
		writeBadRequest(w, r, err.Error())
		return
	}
	expectedVersion, ok := parseIfMatch(w, r)
	if !ok {
		return
	}

	job, err := h.store.GetJobByID(r.Context(), caller.TenantID, jobID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if !requireLegalEntityHeaderMatches(w, r, job.LegalEntityID) {
		return
	}
	if !h.authorize(w, r, caller, job.LegalEntityID, ActionAuditEntryRemediate) {
		return
	}
	entry := job.FindEntry(entryID)
	if entry == nil {
		h.writeError(w, r, store.ErrEntryNotFound)
		return
	}
	if err := domain.CheckDispositionRules(job, entry, caller.PrincipalID, &req, time.Now()); err != nil {
		h.writeError(w, r, err)
		return
	}

	command := store.DispositionCommand{
		JobID:           jobID,
		EntryID:         entryID,
		TargetStatus:    req.TargetStatus(),
		Reason:          strings.TrimSpace(req.RemediationReason),
		EvidenceRef:     strings.TrimSpace(req.RemediationEvidenceRef),
		WaiverExpiresAt: req.WaiverExpiresAt,
		ExpectedVersion: expectedVersion,
		ActorID:         caller.PrincipalID,
		CorrelationID:   r.Header.Get(headerCorrelationID),
	}
	decided, newVersion, err := h.store.DispositionEntry(r.Context(), caller.TenantID, command, claim)
	if errors.Is(err, store.ErrIdempotencyKeyInUse) {
		h.handleConcurrentReplay(w, r, caller, claim)
		return
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	w.Header().Set("ETag", etagFor(newVersion))
	h.writeJSON(w, http.StatusOK, decided)
}
