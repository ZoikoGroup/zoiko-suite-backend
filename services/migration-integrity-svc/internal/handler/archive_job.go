package handler

import (
	"errors"
	"net/http"

	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// ArchiveJob handles DELETE /v1/migrations/{id}.
//
// DELETE is a SOFT delete: the job moves to ARCHIVED and records
// archived_by / archived_at; its checks and quarantine entries are kept and
// stay readable through GET (ZS-MIG-001 §24 immutable issue history,
// MIG-INV-21 no destructive edits, MIG-INV-23 evidence retention). The
// database refuses physical deletes of migration evidence by trigger.
//
// Validation, in order:
//   - X-Principal-Id present                                       401
//   - {id} is a UUID                                               400
//   - Idempotency-Key replay: same request → stored 200; reuse → 409
//   - If-Match (optional) is a version number                      400
//   - job exists in the tenant                                     404 (also cross-tenant)
//   - X-Legal-Entity-Id equals the job's legal entity              403
//   - MIGRATION_JOB_ARCHIVE at the job's legal entity              403 / 503
//   - job is not already ARCHIVED                                  409
//   - If-Match equals the job's current version                    412
//
// Writes: migration_jobs (status, archived_at/by, version),
// migration_idempotency_keys. Event: migration.job_archived.
// Success: 200 with the archived job summary; ETag = its new version.
func (h *Handler) ArchiveJob(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireCaller(w, r)
	if !ok {
		return
	}
	jobID, ok := requireUUIDParam(w, r, "id")
	if !ok {
		return
	}
	claim := newIdempotencyClaim(r, caller, operationArchive, nil)
	if h.replayIfSeen(w, r, caller, claim) {
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
	if !h.authorize(w, r, caller, job.LegalEntityID, ActionMigrationJobArchive) {
		return
	}
	if err := domain.CanArchive(job.Status); err != nil {
		h.writeError(w, r, err)
		return
	}

	command := store.ArchiveCommand{
		JobID:           jobID,
		ExpectedVersion: expectedVersion,
		ActorID:         caller.PrincipalID,
		CorrelationID:   r.Header.Get(headerCorrelationID),
	}
	archived, err := h.store.ArchiveJob(r.Context(), caller.TenantID, command, claim)
	if errors.Is(err, store.ErrIdempotencyKeyInUse) {
		h.handleConcurrentReplay(w, r, caller, claim)
		return
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	w.Header().Set("ETag", etagFor(archived.Version))
	h.writeJSON(w, http.StatusOK, archived)
}
