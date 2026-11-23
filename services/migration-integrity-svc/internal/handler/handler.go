// Package handler is the HTTP layer of migration-integrity-svc. One file per
// route (validate_migration.go, get_job.go, list_jobs.go, remediate_entry.go,
// archive_job.go); shared request plumbing lives in request.go and the error
// mapping in errors.go. ROUTES.md at the service root documents the contract.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"
	"zoiko.io/migration-integrity-svc/internal/authz"
	"zoiko.io/migration-integrity-svc/internal/glclient"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// Authorization actions checked against authorization-svc, always at the
// legal entity that owns the job.
const (
	ActionMigrationJobValidate = "MIGRATION_JOB_VALIDATE"
	ActionMigrationJobArchive  = "MIGRATION_JOB_ARCHIVE"
	ActionMigrationJobView     = "MIGRATION_JOB_VIEW"
	ActionAuditEntryRemediate  = "AUDIT_ENTRY_REMEDIATE"

	// Control-plane actions (batch ref / crosswalk / certificate). Separate
	// from the field-level actions above because promoting a batch into
	// general-ledger-svc and issuing a certificate are materially bigger
	// authorities than running or reading a validation job.
	ActionMigrationBatchPromote     = "MIGRATION_BATCH_PROMOTE"
	ActionMigrationBatchView        = "MIGRATION_BATCH_VIEW"
	ActionMigrationCrosswalkWrite   = "MIGRATION_CROSSWALK_WRITE"
	ActionMigrationCrosswalkView    = "MIGRATION_CROSSWALK_VIEW"
	ActionMigrationCertificateIssue = "MIGRATION_CERTIFICATE_ISSUE"
	ActionMigrationCertificateView  = "MIGRATION_CERTIFICATE_VIEW"

	// Program and source-extract actions. Program actions are tenant-scoped
	// (checked against the tenant, same as MIGRATION_CROSSWALK_*: a program
	// is not attached to one legal entity -- the batches it aggregates each
	// have their own). Extract actions are per-legal-entity, matching
	// batch-ref/job scoping, since an extract names exactly one
	// legal_entity_id.
	ActionMigrationProgramWrite = "MIGRATION_PROGRAM_WRITE"
	ActionMigrationProgramView  = "MIGRATION_PROGRAM_VIEW"
	ActionMigrationExtractWrite = "MIGRATION_EXTRACT_REGISTER"
	ActionMigrationExtractView  = "MIGRATION_EXTRACT_VIEW"
)

// Idempotency operations: the stored replay is bound to one operation, so a
// key reused on a different route is a conflict, not a replay.
const (
	operationValidate    = "migration.validate"
	operationRemediate   = "migration.audit_entry.disposition"
	operationArchive     = "migration.archive"
	headerPrincipalID    = "X-Principal-Id"
	headerLegalEntityID  = "X-Legal-Entity-Id"
	headerCorrelationID  = "X-Correlation-ID"
	headerIdempotencyKey = "Idempotency-Key"
	headerIfMatch        = "If-Match"
	headerReplayed       = "Idempotent-Replayed"
)

// Handler serves the /v1/migrations routes.
type Handler struct {
	store                   store.Store
	authz                   *authz.Client
	logger                  *zap.Logger
	integrityScoreThreshold float64

	// cp/gl are set by WithControlPlane. Left nil, the control-plane routes
	// (promote/crosswalk/certificate) answer 501 rather than panic -- the
	// same "not configured" shape general-ledger-svc's own kernel handler
	// uses for a repo that does not implement MigrationRepo.
	cp store.ControlPlaneStore
	gl *glclient.Client
}

// NewHandler wires the handler. integrityScoreThreshold is the service-level
// acceptance bar for validation results (config.IntegrityScoreThreshold): a
// job scoring below it is FAILED, and the bar is stored on every job it
// decides so the acceptance stays auditable after the default changes
// (SPEC_DEVIATIONS A11).
func NewHandler(s store.Store, a *authz.Client, l *zap.Logger, integrityScoreThreshold float64) *Handler {
	return &Handler{store: s, authz: a, logger: l, integrityScoreThreshold: integrityScoreThreshold}
}

// WithControlPlane wires the batch-ref/crosswalk/certificate store and the
// general-ledger-svc client. Mirrors general-ledger-svc handler's own
// With*(...) *Handler chaining style (see handler.New(...).WithEvidenceGate(...)).
func (h *Handler) WithControlPlane(cp store.ControlPlaneStore, gl *glclient.Client) *Handler {
	h.cp = cp
	h.gl = gl
	return h
}

// ─── Control-plane helpers ────────────────────────────────────────────────────
//
// control_plane.go (batch ref, crosswalk, certificate, program, extract) keeps
// the {"error": msg} body shape it shipped with; the job routes in this
// package answer application/problem+json through errors.go.

func (h *Handler) okJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *Handler) errJSON(w http.ResponseWriter, code int, msg string) {
	h.okJSON(w, code, map[string]string{"error": msg})
}

// requirePrincipal extracts the caller's principal ID from the
// X-Principal-Id request header. If missing, it writes a 401 response and
// returns ok=false so the caller can abort the request.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get(headerPrincipalID)
	if principalID == "" {
		h.errJSON(w, http.StatusUnauthorized, "missing "+headerPrincipalID+" header")
		return "", false
	}
	return principalID, true
}

// checkAllowed enforces a fail-closed per-legal-entity authorization check
// against authorization-svc before a read or write proceeds. It writes the
// appropriate error response and returns false when the action must not
// proceed.
func (h *Handler) checkAllowed(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			h.errJSON(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.logger.Error("authorization check failed", zap.Error(err))
		h.errJSON(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}
