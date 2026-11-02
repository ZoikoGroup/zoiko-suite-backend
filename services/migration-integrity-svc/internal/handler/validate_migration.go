package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// ValidateMigration handles POST /v1/migrations/validate.
//
// It validates one migration batch against the source manifest's control
// totals and stores the decided run (COMPLETED or FAILED) with its integrity
// checks and quarantine entries.
//
// Validation, in order:
//   - X-Principal-Id present                                        401
//   - Idempotency-Key replay: same request → stored 201; reuse → 409
//   - body is well-formed JSON with no unknown fields               400
//   - body tenant_id (optional) equals X-Tenant-Id                  403
//   - body legal_entity_id equals X-Legal-Entity-Id when both given 403
//   - required fields, records (1..10000), control_totals           400
//   - exception_owner_id is not the submitter (SoD)                 422
//   - MIGRATION_JOB_VALIDATE at the legal entity                    403 / 503
//
// Writes: migration_jobs, migration_integrity_checks,
// migration_audit_entries, migration_idempotency_keys — one transaction.
// Events: migration.integrity_validated, plus
// migration.integrity_violations_detected when anything was quarantined.
// Success: 201 with the job (ETag = version, Location = job URL).
func (h *Handler) ValidateMigration(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireCaller(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	claim := newIdempotencyClaim(r, caller, operationValidate, body)
	if h.replayIfSeen(w, r, caller, claim) {
		return
	}

	var req domain.ValidateMigrationRequest
	if !decodeStrict(w, r, body, &req) {
		return
	}
	if !requireBodyTenantMatches(w, r, req.TenantID, caller.TenantID) {
		return
	}
	if !resolveLegalEntity(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeBadRequest(w, r, err.Error())
		return
	}
	if err := domain.CheckSubmissionRules(&req, caller.PrincipalID); err != nil {
		h.writeError(w, r, err)
		return
	}
	if !h.authorize(w, r, caller, req.LegalEntityID, ActionMigrationJobValidate) {
		return
	}

	job, outcome := h.buildDecidedJob(&req, caller)
	err := h.store.CreateJob(r.Context(), caller.TenantID, job, outcome.Checks, outcome.Entries,
		caller.PrincipalID, r.Header.Get(headerCorrelationID), claim)
	if errors.Is(err, store.ErrIdempotencyKeyInUse) {
		h.handleConcurrentReplay(w, r, caller, claim)
		return
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	w.Header().Set("ETag", etagFor(job.Version))
	w.Header().Set("Location", "/v1/migrations/"+job.ID)
	h.writeJSON(w, http.StatusCreated, job)
}

// resolveLegalEntity reconciles the body's legal_entity_id with the
// X-Legal-Entity-Id header: the body may omit it (the header is used) or
// repeat it, but may not name a different entity.
func resolveLegalEntity(w http.ResponseWriter, r *http.Request, req *domain.ValidateMigrationRequest) bool {
	headerEntity := strings.TrimSpace(r.Header.Get(headerLegalEntityID))
	bodyEntity := strings.TrimSpace(req.LegalEntityID)
	if bodyEntity == "" {
		req.LegalEntityID = headerEntity
		return true
	}
	if headerEntity != "" && headerEntity != bodyEntity {
		writeForbidden(w, r, "legal-entity-mismatch", "legal_entity_id in the body does not match X-Legal-Entity-Id")
		return false
	}
	req.LegalEntityID = bodyEntity
	return true
}

// buildDecidedJob runs the deterministic validation and assembles the job
// record it decided.
func (h *Handler) buildDecidedJob(req *domain.ValidateMigrationRequest, caller callerIdentity) (*domain.MigrationJob, domain.ValidationOutcome) {
	jobID := uuid.NewString()
	now := time.Now().UTC()
	outcome := domain.ValidateBatch(req, jobID, caller.TenantID, h.integrityScoreThreshold)
	totals := outcome.ControlTotals

	job := &domain.MigrationJob{
		ID:                      jobID,
		LegalEntityID:           req.LegalEntityID,
		MigrationName:           strings.TrimSpace(req.MigrationName),
		SourceSystem:            strings.TrimSpace(req.SourceSystem),
		TargetService:           strings.TrimSpace(req.TargetService),
		RequestedBy:             caller.PrincipalID,
		TotalRecordsCount:       len(req.Records),
		ValidRecordsCount:       outcome.ValidCount,
		InvalidRecordsCount:     outcome.InvalidCount,
		IntegrityScore:          outcome.Score,
		IntegrityScoreThreshold: h.integrityScoreThreshold,
		BlockerCount:            outcome.BlockerCount,
		Status:                  outcome.Status,
		ControlTotals:           &totals,
		StartedAt:               &now,
		CompletedAt:             &now,
	}
	return job, outcome
}
