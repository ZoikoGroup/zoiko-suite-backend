// Package handler is the HTTP layer of migration-integrity-svc. One file per
// route (validate_migration.go, get_job.go, list_jobs.go, remediate_entry.go,
// archive_job.go); shared request plumbing lives in request.go and the error
// mapping in errors.go. ROUTES.md at the service root documents the contract.
package handler

import (
	"go.uber.org/zap"
	"zoiko.io/migration-integrity-svc/internal/authz"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// Authorization actions checked against authorization-svc, always at the
// legal entity that owns the job.
const (
	ActionMigrationJobValidate = "MIGRATION_JOB_VALIDATE"
	ActionMigrationJobArchive  = "MIGRATION_JOB_ARCHIVE"
	ActionMigrationJobView     = "MIGRATION_JOB_VIEW"
	ActionAuditEntryRemediate  = "AUDIT_ENTRY_REMEDIATE"
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
}

// NewHandler wires the handler. integrityScoreThreshold is the service-level
// acceptance bar for validation results (config.IntegrityScoreThreshold): a
// job scoring below it is FAILED, and the bar is stored on every job it
// decides so the acceptance stays auditable after the default changes
// (SPEC_DEVIATIONS A11).
func NewHandler(s store.Store, a *authz.Client, l *zap.Logger, integrityScoreThreshold float64) *Handler {
	return &Handler{store: s, authz: a, logger: l, integrityScoreThreshold: integrityScoreThreshold}
}
