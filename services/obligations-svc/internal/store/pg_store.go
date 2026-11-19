// Package store provides the PostgreSQL implementation of the obligations
// read and write model.
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers or domain packages.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/middleware"
)

// mapPgError turns the Postgres failures that are really caller mistakes into
// domain errors, so they stop arriving at the handler as "the store is
// unavailable".
//
// obligation_id, legal_entity_id and jurisdiction_id are all uuid columns, so a
// mistyped one dies inside the driver as SQLSTATE 22P02 before any row is
// examined — and used to answer 503, an outage status for a typo in a URL.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return domain.ErrInvalidIdentifier
	}
	return err
}

// tenantOf reads the caller's tenant and refuses an unscoped call.
//
// Before migration 000003 this service had no tenant dimension at all, so every
// query below ran across every tenant's obligations. The predicate is explicit
// on each statement rather than left to row-level security, because these
// services connect as the database OWNER and Postgres exempts the owner from
// RLS unless the table is FORCEd — a policy alone would be a control that reads
// as present and does nothing at runtime.
func tenantOf(ctx context.Context) (string, error) {
	t := middleware.TenantFromContext(ctx)
	if t == "" {
		return "", domain.ErrTenantMissing
	}
	return t, nil
}

// withTenantTx runs fn inside a transaction with app.tenant_id installed, so
// the row-level security policy has something to compare against.
//
// set_config with a bound parameter, never a formatted string: the tenant id
// arrives on a request header, and board-resolutions-svc shipped
// `SET LOCAL app.tenant_id = '<header>'` — raw caller input interpolated into
// the one statement whose job is enforcing tenant isolation.
func (s *PgStore) withTenantTx(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, mapPgError(err))
	}
	if err := fn(tx, tenantID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) withOutboxRelayTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// insertOutboxEventTx persists the same canonical envelope the Kafka
// publisher emits, inside the caller's business transaction. This is the
// D10 guarantee: the obligation mutation and its event commit together.
func insertOutboxEventTx(ctx context.Context, tx pgx.Tx, tenantID, eventType, correlationID, legalEntityID, jurisdiction, actorID string, payload map[string]any) error {
	env := map[string]any{
		"event_id":        "evt-" + uuid.New().String(),
		"event_type":      eventType,
		"event_version":   "1.0",
		"emitted_at":      time.Now().UTC().Format(time.RFC3339),
		"schema_version":  "1.0",
		"source_service":  "obligations-svc",
		"tenant_id":       tenantID,
		"legal_entity_id": legalEntityID,
		"jurisdiction":    jurisdiction,
		"actor_id":        actorID,
		"correlation_id":  correlationID,
		"payload":         payload,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}
	const query = `
		INSERT INTO event_outbox (tenant_id, event_type, event_payload, correlation_id)
		VALUES ($1, $2, $3, $4);`
	if _, err := tx.Exec(ctx, query, tenantID, eventType, raw, correlationID); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// CheckIdempotencyKey checks if an idempotency key has been processed before.
// Returns (alreadyProcessed bool, existingResourceID string, existingResourceType string, err error).
// If alreadyProcessed is true, the caller can return the existing resource.
// Verifies that the request hash matches to detect IDEMPOTENCY_MISMATCH.
func (s *PgStore) CheckIdempotencyKey(ctx context.Context, idempotencyKey, requestHash string) (bool, string, string, error) {
	const query = `
		SELECT resource_id, resource_type, request_hash
		FROM idempotency_keys
		WHERE tenant_id = $1 AND idempotency_key = $2;`

	var existingResourceID, existingResourceType, existingRequestHash string
	err := s.withTenantTx(ctx, func(tx pgx.Tx, txTenantID string) error {
		return tx.QueryRow(ctx, query, txTenantID, idempotencyKey).Scan(&existingResourceID, &existingResourceType, &existingRequestHash)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "", "", nil
		}
		return false, "", "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Verify the request hash matches to detect IDEMPOTENCY_MISMATCH (API standard L381).
	if existingRequestHash != requestHash {
		return false, "", "", domain.ErrIdempotencyKeyConflict
	}

	return true, existingResourceID, existingResourceType, nil
}

// StoreIdempotencyKey records an idempotency key with its associated resource.
func (s *PgStore) StoreIdempotencyKey(ctx context.Context, idempotencyKey, requestHash, resourceID, resourceType string) error {
	const query = `
		INSERT INTO idempotency_keys (tenant_id, idempotency_key, request_hash, resource_id, resource_type)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING;`

	return s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		if _, err := tx.Exec(ctx, query, tenantID, idempotencyKey, requestHash, resourceID, resourceType); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return nil
	})
}

// ComputeRequestHash computes a SHA256 hash of the request body for idempotency verification.
func ComputeRequestHash(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// Store is the interface consumed by the handler for obligation/filing CRUD.
type Store interface {
	// CreateObligation inserts a new obligation idempotently.
	CreateObligation(ctx context.Context, params domain.CreateObligationParams) (*domain.Obligation, bool, error)

	// FindObligationByID returns the Obligation with the given obligation_id.
	FindObligationByID(ctx context.Context, obligationID string) (*domain.Obligation, error)

	// ListObligations returns obligations matching the given filter.
	ListObligations(ctx context.Context, filter domain.ListObligationsFilter) ([]*domain.Obligation, error)

	// UpdateObligationStatus transitions an obligation's status, enforcing
	// the legal state machine. Returns the updated obligation and whether
	// this call actually performed a transition (false = idempotent no-op,
	// the obligation was already in the requested status).
	// If expectedVersion > 0, the update will only succeed if the current
	// version matches (optimistic locking via If-Match).
	UpdateObligationStatus(ctx context.Context, obligationID, newStatus string, expectedVersion int, actorID, correlationID string) (*domain.Obligation, bool, error)

	// UpdateObligationWorkflow links or unlinks an obligation to/from a workflow instance.
	// Returns the updated obligation.
	UpdateObligationWorkflow(ctx context.Context, obligationID, workflowID, actorID, correlationID string) (*domain.Obligation, error)

	// CreateFilingRequirement inserts a new filing requirement under an
	// obligation. Fails with domain.ErrObligationNotFound if obligation_id
	// does not exist.
	CreateFilingRequirement(ctx context.Context, params domain.CreateFilingRequirementParams) (*domain.FilingRequirement, error)

	// ListFilingRequirements returns an obligation's filing requirements,
	// newest first, matching the filter. Fails with domain.ErrObligationNotFound if
	// obligation_id does not exist.
	ListFilingRequirements(ctx context.Context, filter domain.ListFilingRequirementsFilter) ([]*domain.FilingRequirement, error)

	// FindOverdueObligations returns obligations that are OPEN or IN_PROGRESS
	// and whose due_date has passed. Used by the overdue scheduler.
	// If tenantID is provided, only returns obligations for that tenant.
	// If empty, returns all obligations across all tenants (for scheduler).
	FindOverdueObligations(ctx context.Context, tenantID string) ([]*domain.Obligation, error)

	// GetTenantIDsWithObligations returns all tenant IDs that have at least one obligation.
	// Used by the overdue scheduler to iterate tenants.
	GetTenantIDsWithObligations(ctx context.Context) ([]string, error)

	// TransitionOverdue transitions an obligation to OVERDUE status.
	// Returns the updated obligation and whether a transition occurred.
	TransitionOverdue(ctx context.Context, obligationID string) (*domain.Obligation, bool, error)

	// Event outbox methods for reliable event publishing (D10 fix)
	AddEventToOutbox(ctx context.Context, eventType, correlationID, legalEntityID, jurisdiction, actorID, key string, payload map[string]any) error
	GetPendingOutboxEvents(ctx context.Context, limit int) ([]OutboxEvent, error)
	MarkOutboxEventPublished(ctx context.Context, outboxID string) error
	MarkOutboxEventFailed(ctx context.Context, outboxID string, errMsg string) error

	// ── Chunk 10: applicability decisions (doc7 §E2) ────────────────────────
	CreateApplicabilityDecision(ctx context.Context, params domain.CreateApplicabilityDecisionParams) (*domain.ApplicabilityDecision, bool, error)
	ListApplicabilityDecisions(ctx context.Context, obligationID, jurisdictionCode, entityRef string, limit int) ([]*domain.ApplicabilityDecision, error)
	FindCurrentApplicability(ctx context.Context, obligationID, jurisdictionCode, entityRef string) (*domain.CurrentApplicability, error)

	// ── Compliance Status ─────────────────────────────────────────────────────
	CreateComplianceStatus(ctx context.Context, params domain.CreateComplianceStatusParams) (*domain.ComplianceStatus, error)
	FindComplianceStatusByID(ctx context.Context, complianceStatusID string) (*domain.ComplianceStatus, error)
	ListComplianceStatuses(ctx context.Context, filter domain.ListComplianceStatusFilter) ([]*domain.ComplianceStatus, error)
	UpdateComplianceStatus(ctx context.Context, complianceStatusID, statusCode, statusReason, evidenceSufficiencyState string, expectedVersion int, actorID, correlationID string) (*domain.ComplianceStatus, error)

	// ── Exception Cases ───────────────────────────────────────────────────────
	CreateExceptionCase(ctx context.Context, params domain.CreateExceptionCaseParams) (*domain.ExceptionCase, error)
	FindExceptionCaseByID(ctx context.Context, exceptionCaseID string) (*domain.ExceptionCase, error)
	ListExceptionCases(ctx context.Context, filter domain.ListExceptionCasesFilter) ([]*domain.ExceptionCase, error)
	UpdateExceptionCaseStatus(ctx context.Context, exceptionCaseID, caseStatus string, expectedVersion int, actorID, correlationID string) (*domain.ExceptionCase, error)

	// ── Escalation Records ────────────────────────────────────────────────────
	CreateEscalationRecord(ctx context.Context, params domain.CreateEscalationRecordParams) (*domain.EscalationRecord, error)
	FindEscalationRecordByID(ctx context.Context, escalationRecordID string) (*domain.EscalationRecord, error)
	ListEscalationRecords(ctx context.Context, exceptionCaseID string, limit int) ([]*domain.EscalationRecord, error)

	// ── Clock Methods ──────────────────────────────────────────────────────────
	CreateClock(ctx context.Context, params domain.CreateClockParams) (*domain.Clock, error)
	FindClockByID(ctx context.Context, clockID string) (*domain.Clock, error)
	ListClocks(ctx context.Context, filter domain.ListClocksFilter) ([]*domain.Clock, error)
	TransitionClock(ctx context.Context, params domain.ClockTransitionParams) (*domain.Clock, bool, error)
	FindActiveClocksForObligation(ctx context.Context, obligationID string) ([]*domain.Clock, error)
}

// PgStore implements Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New returns an open PgStore. Caller must call pool.Close() when done.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// ── obligations ──────────────────────────────────────────────────────────────

// defaultListLimit bounds a register read whose caller did not choose a limit.
// It matches the HTTP handler's own default so the two cannot drift.
const defaultListLimit = 100

// obligationColumns is the standard SELECT column list shared by all
// obligation queries. Order must match scanObligation exactly.
const obligationColumns = `
	obligation_id,
	legal_entity_id,
	jurisdiction_id,
	workflow_id,
	obligation_source_type,
	obligation_source_id,
	obligation_code,
	obligation_type,
	obligation_status,
	due_date,
	severity_level,
	responsible_function,
	source_reference,
	created_at,
	created_by_principal_id,
	updated_at,
	closed_at,
	version,
	obligation_definition_id,
	definition_version`

// scanObligation scans one row produced by an obligationColumns SELECT.
func scanObligation(row pgx.Row) (*domain.Obligation, error) {
	o := &domain.Obligation{}
	err := row.Scan(
		&o.ObligationID,
		&o.LegalEntityID,
		&o.JurisdictionID,
		&o.WorkflowID,
		&o.ObligationSourceType,
		&o.ObligationSourceID,
		&o.ObligationCode,
		&o.ObligationType,
		&o.ObligationStatus,
		&o.DueDate,
		&o.SeverityLevel,
		&o.ResponsibleFunction,
		&o.SourceReference,
		&o.CreatedAt,
		&o.CreatedByPrincipalID,
		&o.UpdatedAt,
		&o.ClosedAt,
		&o.Version,
		&o.ObligationDefinitionID,
		&o.DefinitionVersion,
	)
	return o, err
}

// FindObligationByID looks up an obligation by its UUID primary key.
func (s *PgStore) FindObligationByID(ctx context.Context, obligationID string) (*domain.Obligation, error) {
	// Another tenant's obligation reads as not-found — the same answer as one
	// that does not exist, deliberately, so a caller cannot probe for the
	// existence of records they may not read.
	const query = `
		SELECT ` + obligationColumns + `
		FROM obligations
		WHERE obligation_id = $1 AND tenant_id = $2;`

	var o *domain.Obligation
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		o, scanErr = scanObligation(tx.QueryRow(ctx, query, obligationID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrObligationNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			// An id that cannot be a UUID names no obligation, which is
			// exactly what "not found" means.
			return nil, domain.ErrObligationNotFound
		}
		s.log.Error("pg FindObligationByID failed", zap.String("obligation_id", obligationID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return o, nil
}

// CreateObligation inserts a new obligation or returns the existing one on
// dedup match by obligation_code.
func (s *PgStore) CreateObligation(ctx context.Context, params domain.CreateObligationParams) (*domain.Obligation, bool, error) {
	if params.ObligationID == "" {
		params.ObligationID = uuid.New().String()
	}

	// ON CONFLICT (tenant_id, obligation_code), not (obligation_code).
	//
	// The dedup key used to be global, and creation is idempotent on it —
	// conflict, then look up and return the existing row. So a second tenant
	// registering an ordinary code like "VAT-Q1-2026" did not create their
	// obligation: they received the FIRST tenant's, complete with that
	// tenant's legal entity, due date and source reference, as a 200. One
	// tenant's compliance register answering with another's record, straight
	// down the documented happy path.
	const query = `
		INSERT INTO obligations (
			obligation_id, tenant_id, legal_entity_id, jurisdiction_id, workflow_id, obligation_source_type,
			obligation_source_id, obligation_code, obligation_type, due_date,
			severity_level, responsible_function, source_reference, created_by_principal_id,
			obligation_definition_id, definition_version
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (tenant_id, obligation_code)
		DO NOTHING
		RETURNING ` + obligationColumns + `;`

	// Conflict on (tenant_id, obligation_code). Look the existing one up
	// within THIS tenant — a lookup by code alone is what made the replay
	// cross-tenant in the first place.
	const lookupQuery = `
		SELECT ` + obligationColumns + `
		FROM obligations
		WHERE obligation_code = $1 AND tenant_id = $2;`

	// The insert and the on-conflict lookup share one transaction, which is
	// also what installs app.tenant_id. Both matter: the two statements are one
	// idempotency decision and should not straddle a commit, and every
	// statement this store makes has to carry the tenant scope or the
	// row-level security policy filters it to nothing once the service stops
	// connecting as a superuser.
	var (
		o       *domain.Obligation
		created bool
	)
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var definitionID *string
		var definitionVersion *int
		if params.ObligationDefinitionID != "" {
			// Raised from a definition: pin its current version, and refuse an
			// entity the definition does not bind. Read under the same
			// transaction as the insert so a concurrent supersede cannot pin
			// a version the content was not taken from.
			v, err := s.pinDefinitionTx(ctx, tx, tenantID, params.ObligationDefinitionID, params.LegalEntityID, params.DueDate)
			if err != nil {
				return err
			}
			if params.DefinitionVersion != 0 && params.DefinitionVersion != v {
				return domain.ErrVersionMismatch
			}
			definitionID, definitionVersion = &params.ObligationDefinitionID, &v
		}

		var scanErr error
		o, scanErr = scanObligation(tx.QueryRow(ctx, query,
			params.ObligationID, tenantID, params.LegalEntityID, params.JurisdictionID, params.WorkflowID, params.ObligationSourceType,
			params.ObligationSourceID, params.ObligationCode, params.ObligationType, params.DueDate,
			params.SeverityLevel, params.ResponsibleFunction, params.SourceReference, params.CreatedByPrincipalID,
			definitionID, definitionVersion,
		))
		if scanErr == nil {
			created = true
			return insertOutboxEventTx(ctx, tx, tenantID, "obligation.created", params.CorrelationID,
				o.LegalEntityID, o.JurisdictionID, o.CreatedByPrincipalID, map[string]any{
					"obligation_id":            o.ObligationID,
					"legal_entity_id":          o.LegalEntityID,
					"jurisdiction_id":          o.JurisdictionID,
					"obligation_code":          o.ObligationCode,
					"obligation_type":          o.ObligationType,
					"due_date":                 o.DueDate,
					"severity_level":           o.SeverityLevel,
					"source_reference":         o.SourceReference,
					"created_by_principal_id":  o.CreatedByPrincipalID,
					"created_at":               o.CreatedAt,
					"obligation_definition_id": o.ObligationDefinitionID,
					"definition_version":       o.DefinitionVersion,
				})
		}
		if !errors.Is(scanErr, pgx.ErrNoRows) {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			// A caller-supplied obligation_id that is already taken is not
			// covered by ON CONFLICT (tenant_id, obligation_code); it used to
			// surface as 503. It is the caller's conflict.
			var pgErr *pgconn.PgError
			if errors.As(scanErr, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "obligations_pkey" {
				return domain.ErrObligationIDConflict
			}
			s.log.Error("pg CreateObligation failed", zap.Error(scanErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		o, scanErr = scanObligation(tx.QueryRow(ctx, lookupQuery, params.ObligationCode, tenantID))
		if scanErr != nil {
			s.log.Error("pg CreateObligation lookup existing failed", zap.Error(scanErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if created {
		return o, true, nil
	}

	if o.LegalEntityID != params.LegalEntityID ||
		o.JurisdictionID != params.JurisdictionID ||
		o.ObligationType != params.ObligationType ||
		!o.DueDate.Equal(params.DueDate) ||
		o.SeverityLevel != params.SeverityLevel ||
		o.SourceReference != params.SourceReference ||
		o.ResponsibleFunction != params.ResponsibleFunction ||
		(o.ObligationDefinitionID == nil) != (params.ObligationDefinitionID == "") ||
		(o.ObligationDefinitionID != nil && *o.ObligationDefinitionID != params.ObligationDefinitionID) {
		s.log.Warn("obligation dedup match but attribute mismatch (409 conflict)",
			zap.String("existing_id", o.ObligationID),
			zap.String("req_id", params.ObligationID),
		)
		return nil, false, domain.ErrConflict
	}

	return o, false, nil
}

// ListObligations returns obligations matching the given filter, newest
// (by created_at) first. Filters are applied only where set on the filter.
func (s *PgStore) ListObligations(ctx context.Context, filter domain.ListObligationsFilter) ([]*domain.Obligation, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	// A filter may narrow a read, never widen it to another tenant.
	if filter.TenantID != "" && filter.TenantID != tenantID {
		return nil, domain.ErrTenantMissing
	}

	query := `SELECT ` + obligationColumns + ` FROM obligations WHERE tenant_id = $1`
	args := []any{tenantID}

	addFilter := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}

	if filter.LegalEntityID != "" {
		addFilter("legal_entity_id =", filter.LegalEntityID)
	}
	if filter.JurisdictionID != "" {
		addFilter("jurisdiction_id =", filter.JurisdictionID)
	}
	if filter.ObligationType != "" {
		addFilter("obligation_type =", filter.ObligationType)
	}
	if filter.Status != "" {
		addFilter("obligation_status =", filter.Status)
	}
	if filter.DueBefore != nil {
		addFilter("due_date <=", *filter.DueBefore)
	}
	if filter.DueAfter != nil {
		addFilter("due_date >=", *filter.DueAfter)
	}
	// Keyset continuation: rows strictly after the previous page's last row in
	// (created_at DESC, obligation_id DESC) order. Unlike OFFSET it neither
	// skips nor repeats a row when obligations are added between pages.
	if filter.AfterCreatedAt != nil && filter.AfterID != "" {
		args = append(args, *filter.AfterCreatedAt, filter.AfterID)
		query += fmt.Sprintf(" AND (created_at, obligation_id) < ($%d, $%d::uuid)", len(args)-1, len(args))
	}
	// created_at alone is not a total order — two obligations recorded in the
	// same transaction share a timestamp and Postgres may return them in
	// either order, so a paged read could show one row twice and skip another.
	// The primary key breaks the tie.
	query += " ORDER BY created_at DESC, obligation_id DESC"
	// A non-positive limit means "the caller did not choose one", not "return
	// nothing". The HTTP handler defaults this to 100 and rejects anything
	// outside 1..500, but the store must not depend on that: a zero-value
	// filter from any other caller became LIMIT 0, and an empty answer on a
	// statutory compliance register reads as "this tenant owes nothing".
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	args = append(args, filter.Offset)
	query += fmt.Sprintf(" OFFSET $%d;", len(args))

	// Executed inside the tenant-scoped transaction. The explicit
	// `tenant_id = $1` predicate above is what isolates tenants today; this is
	// what lets the row-level security policy do it as well, instead of the
	// policy being filtered out of existence the moment the service connects as
	// something other than a superuser.
	var results []*domain.Obligation
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, args...)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListObligations failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			o, scanErr := scanObligation(rows)
			if scanErr != nil {
				s.log.Error("pg ListObligations scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, o)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			s.log.Error("pg ListObligations rows error", zap.Error(rowsErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, rowsErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// legalTransitions enumerates the obligation_status state machine.
// CLOSED is terminal — never a key in this map. Enforced here in
// application code (like policy-svc's version_status transitions), not a
// DB CHECK constraint.
var legalTransitions = map[string][]string{
	"OPEN":        {"IN_PROGRESS", "OVERDUE", "CLOSED"},
	"IN_PROGRESS": {"OVERDUE", "CLOSED"},
	"OVERDUE":     {"CLOSED"},
}

// UpdateObligationStatus transitions an obligation's status. Idempotent:
// requesting the status the obligation is already in returns the unchanged
// record with transitioned=false, not an error — mirrors
// policy-svc's ActivateVersion idempotency contract.
// If expectedVersion > 0, the update will only succeed if the current
// version matches (optimistic locking via If-Match).
func (s *PgStore) UpdateObligationStatus(ctx context.Context, obligationID, newStatus string, expectedVersion int, actorID, correlationID string) (*domain.Obligation, bool, error) {
	var updated *domain.Obligation
	transitioned := false

	// The status check and the write share ONE transaction and ONE row lock.
	//
	// They used to be two statements with no lock between them: read the
	// current status, decide the transition was legal, then UPDATE. Two
	// concurrent requests both read OPEN, both decided CLOSED was legal, and
	// both wrote — so an obligation could be closed twice, with the second
	// close overwriting closed_at and the state machine never noticing. On a
	// statutory compliance register, "when was this closed" is the whole point
	// of the record.
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `
			SELECT ` + obligationColumns + `
			FROM obligations
			WHERE obligation_id = $1 AND tenant_id = $2
			FOR UPDATE;`

		current, err := scanObligation(tx.QueryRow(ctx, lockQuery, obligationID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrObligationNotFound
			}
			if mapped := mapPgError(err); errors.Is(mapped, domain.ErrInvalidIdentifier) {
				return domain.ErrObligationNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		// Optimistic locking: if expectedVersion > 0, verify it matches.
		if expectedVersion > 0 && current.Version != expectedVersion {
			return domain.ErrVersionMismatch
		}

		// Requesting the status it already holds is a no-op, not an error —
		// the same idempotency contract as policy-svc's ActivateVersion.
		if current.ObligationStatus == newStatus {
			updated = current
			return nil
		}

		legal := false
		for _, candidate := range legalTransitions[current.ObligationStatus] {
			if candidate == newStatus {
				legal = true
				break
			}
		}
		if !legal {
			return domain.ErrInvalidTransition
		}

		const updateQuery = `
			UPDATE obligations
			SET obligation_status = $1,
			    updated_at = NOW(),
			    closed_at = CASE WHEN $1::VARCHAR = 'CLOSED' THEN NOW() ELSE closed_at END,
			    version = version + 1
			WHERE obligation_id = $2 AND tenant_id = $3
			RETURNING ` + obligationColumns + `;`

		updated, err = scanObligation(tx.QueryRow(ctx, updateQuery, newStatus, obligationID, tenantID))
		if err != nil {
			s.log.Error("pg UpdateObligationStatus failed",
				zap.String("obligation_id", obligationID),
				zap.String("new_status", newStatus),
				zap.Error(err),
			)
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		transitioned = true
		updatedEvent := map[string]any{
			"obligation_id":     updated.ObligationID,
			"obligation_status": updated.ObligationStatus,
			"previous_status":   current.ObligationStatus,
			"changed_fields":    []string{"obligation_status"},
			"version":           updated.Version,
			"updated_at":        updated.UpdatedAt,
		}
		if err := insertOutboxEventTx(ctx, tx, tenantID, "obligation.updated", correlationID,
			updated.LegalEntityID, updated.JurisdictionID, actorID, updatedEvent); err != nil {
			return err
		}
		switch updated.ObligationStatus {
		case "OVERDUE":
			if err := insertOutboxEventTx(ctx, tx, tenantID, "obligation.overdue", correlationID,
				updated.LegalEntityID, updated.JurisdictionID, actorID, map[string]any{
					"obligation_id": updated.ObligationID,
					"due_date":      updated.DueDate,
				}); err != nil {
				return err
			}
		case "CLOSED":
			if err := insertOutboxEventTx(ctx, tx, tenantID, "obligation.closed", correlationID,
				updated.LegalEntityID, updated.JurisdictionID, actorID, map[string]any{
					"obligation_id": updated.ObligationID,
					"closed_at":     updated.ClosedAt,
				}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return updated, transitioned, nil
}

// UpdateObligationWorkflow links or unlinks an obligation to/from a workflow instance.
// An empty workflowID unlinks the obligation. The obligation.updated event is
// written to the outbox in the same transaction as the link change.
func (s *PgStore) UpdateObligationWorkflow(ctx context.Context, obligationID, workflowID, actorID, correlationID string) (*domain.Obligation, error) {
	var updated *domain.Obligation

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const query = `
			UPDATE obligations
			SET workflow_id = $1,
			    updated_at = NOW(),
			    version = version + 1
			WHERE obligation_id = $2 AND tenant_id = $3
			RETURNING ` + obligationColumns + `;`

		var wfID *string
		if workflowID != "" {
			wfID = &workflowID
		}

		var err error
		updated, err = scanObligation(tx.QueryRow(ctx, query, wfID, obligationID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrObligationNotFound
			}
			if mapped := mapPgError(err); errors.Is(mapped, domain.ErrInvalidIdentifier) {
				return domain.ErrObligationNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return insertOutboxEventTx(ctx, tx, tenantID, "obligation.updated", correlationID,
			updated.LegalEntityID, updated.JurisdictionID, actorID, map[string]any{
				"obligation_id":     updated.ObligationID,
				"obligation_status": updated.ObligationStatus,
				"workflow_id":       updated.WorkflowID,
				"changed_fields":    []string{"workflow_id"},
				"version":           updated.Version,
				"updated_at":        updated.UpdatedAt,
			})
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// ── filing_requirements ──────────────────────────────────────────────────────

const filingRequirementColumns = `
	filing_requirement_id,
	obligation_id,
	filing_type,
	filing_authority,
	submission_channel,
	filing_status,
	created_at,
	updated_at,
	row_version`

func scanFilingRequirement(row pgx.Row) (*domain.FilingRequirement, error) {
	f := &domain.FilingRequirement{}
	err := row.Scan(
		&f.FilingRequirementID,
		&f.ObligationID,
		&f.FilingType,
		&f.FilingAuthority,
		&f.SubmissionChannel,
		&f.FilingStatus,
		&f.CreatedAt,
		&f.UpdatedAt,
		&f.RowVersion,
	)
	return f, err
}

// ── clocks ─────────────────────────────────────────────────────────────────────

const clockColumns = `
	clock_id,
	obligation_id,
	tenant_id,
	legal_entity_id,
	clock_class,
	clock_state,
	calendar_code,
	calendar_rule_id,
	calendar_rule_version,
	computed_deadline,
	due_soon_threshold,
	escalation_ladder,
	satisfied_at,
	satisfied_by_principal_id,
	satisfied_evidence_ids,
	waived_at,
	waived_by_principal_id,
	waived_reason,
	waived_evidence_ids,
	not_applicable_at,
	not_applicable_by_principal_id,
	not_applicable_reason,
	superseded_at,
	superseded_by_clock_id,
	created_at,
	updated_at,
	version`

func scanClock(row pgx.Row) (*domain.Clock, error) {
	c := &domain.Clock{}
	err := row.Scan(
		&c.ClockID,
		&c.ObligationID,
		&c.TenantID,
		&c.LegalEntityID,
		&c.ClockClass,
		&c.ClockState,
		&c.CalendarCode,
		&c.CalendarRuleID,
		&c.CalendarRuleVersion,
		&c.ComputedDeadline,
		&c.DueSoonThreshold,
		&c.EscalationLadder,
		&c.SatisfiedAt,
		&c.SatisfiedByPrincipal,
		&c.SatisfiedEvidenceIDs,
		&c.WaivedAt,
		&c.WaivedByPrincipal,
		&c.WaivedReason,
		&c.WaivedEvidenceIDs,
		&c.NotApplicableAt,
		&c.NotApplicableByPrincipal,
		&c.NotApplicableReason,
		&c.SupersededAt,
		&c.SupersededByClockID,
		&c.CreatedAt,
		&c.UpdatedAt,
		&c.Version,
	)
	return c, err
}

// CreateFilingRequirement inserts a new filing requirement under an
// obligation. Validates the parent obligation exists first — mirrors
// policy-svc's CreatePolicyVersion validating its parent policy exists,
// rather than relying on a bare FK-violation error to surface as a
// misleading 503.
//
// Idempotent on (tenant_id, obligation_id, filing_type, filing_authority, submission_channel).
// A repeat request with identical attributes returns the existing record.
func (s *PgStore) CreateFilingRequirement(ctx context.Context, params domain.CreateFilingRequirementParams) (*domain.FilingRequirement, error) {
	if _, err := s.FindObligationByID(ctx, params.ObligationID); err != nil {
		return nil, err
	}

	if params.FilingRequirementID == "" {
		params.FilingRequirementID = uuid.New().String()
	}

	// Dedup key: (tenant_id, obligation_id, filing_type, filing_authority, submission_channel)
	const query = `
		INSERT INTO filing_requirements (
			filing_requirement_id, tenant_id, obligation_id, filing_type, filing_authority,
			submission_channel, filing_status
		) VALUES ($1, $2, $3, $4, $5, $6, 'PENDING')
		ON CONFLICT (tenant_id, obligation_id, filing_type, filing_authority, submission_channel)
		DO NOTHING
		RETURNING ` + filingRequirementColumns + `;`

	const lookupQuery = `
		SELECT ` + filingRequirementColumns + `
		FROM filing_requirements
		WHERE obligation_id = $1 AND tenant_id = $2
		  AND filing_type = $3 AND filing_authority = $4 AND submission_channel = $5;`

	var f *domain.FilingRequirement
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		f, scanErr = scanFilingRequirement(tx.QueryRow(ctx, query,
			params.FilingRequirementID, tenantID, params.ObligationID, params.FilingType,
			params.FilingAuthority, params.SubmissionChannel,
		))
		if scanErr == nil {
			return nil
		}
		if !errors.Is(scanErr, pgx.ErrNoRows) {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg CreateFilingRequirement failed", zap.Error(scanErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}

		f, scanErr = scanFilingRequirement(tx.QueryRow(ctx, lookupQuery,
			params.ObligationID, tenantID, params.FilingType, params.FilingAuthority, params.SubmissionChannel))
		if scanErr != nil {
			s.log.Error("pg CreateFilingRequirement lookup existing failed", zap.Error(scanErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

// ListFilingRequirements returns all filing requirements for an obligation.
func (s *PgStore) ListFilingRequirements(ctx context.Context, filter domain.ListFilingRequirementsFilter) ([]*domain.FilingRequirement, error) {
	obligationID := filter.ObligationID
	if _, err := s.FindObligationByID(ctx, obligationID); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	offset := filter.Offset

	var queryBuilder strings.Builder
	queryBuilder.WriteString(`
		SELECT ` + filingRequirementColumns + `
		FROM filing_requirements
		WHERE obligation_id = $1 AND tenant_id = $2`)
	
	args := []any{obligationID}
	argIdx := 3
	
	if filter.AfterCreatedAt != nil {
		queryBuilder.WriteString(fmt.Sprintf(" AND created_at < $%d", argIdx))
		args = append(args, *filter.AfterCreatedAt)
		argIdx++
	}
	if filter.AfterID != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND filing_requirement_id < $%d", argIdx))
		args = append(args, filter.AfterID)
		argIdx++
	}
	
	queryBuilder.WriteString(fmt.Sprintf(" ORDER BY created_at DESC, filing_requirement_id DESC LIMIT $%d OFFSET $%d;", argIdx, argIdx+1))
	args = append(args, limit, offset)

	var results []*domain.FilingRequirement
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		argsWithTenant := append([]any{tenantID}, args[1:]...)
		rows, qErr := tx.Query(ctx, queryBuilder.String(), argsWithTenant...)
		if qErr != nil {
			s.log.Error("pg ListFilingRequirements failed", zap.String("obligation_id", obligationID), zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			f, scanErr := scanFilingRequirement(rows)
			if scanErr != nil {
				s.log.Error("pg ListFilingRequirements scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, f)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			s.log.Error("pg ListFilingRequirements rows error", zap.Error(rowsErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, rowsErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// TransitionOverdue transitions an obligation to OVERDUE status.
// Returns the updated obligation and whether a transition occurred.
// Uses expectedVersion=0 (no optimistic locking) as this is a system-driven transition.
func (s *PgStore) TransitionOverdue(ctx context.Context, obligationID string) (*domain.Obligation, bool, error) {
	return s.UpdateObligationStatus(ctx, obligationID, "OVERDUE", 0, "system:obligations-svc", "overdue-sweep:"+obligationID)
}

// OutboxEvent represents an event in the transactional outbox.
type OutboxEvent struct {
	OutboxID      string
	TenantID      string
	EventType     string
	EventPayload  []byte
	CorrelationID string
	Status        string
	Attempts      int
	CreatedAt     time.Time
}

// AddEventToOutbox adds an event to the outbox within the current tenant transaction.
// This should be called within the same transaction as the business data write.
func (s *PgStore) AddEventToOutbox(ctx context.Context, eventType, correlationID, legalEntityID, jurisdiction, actorID, key string, payload map[string]any) error {
	return s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		return insertOutboxEventTx(ctx, tx, tenantID, eventType, correlationID,
			legalEntityID, jurisdiction, actorID, payload)
	})
}

// GetPendingOutboxEvents retrieves pending events from the outbox for publishing.
func (s *PgStore) GetPendingOutboxEvents(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}

	const query = `
		SELECT outbox_id, tenant_id, event_type, event_payload, correlation_id, status, attempts, created_at
		FROM event_outbox
		WHERE tenant_id = $1 AND status = 'PENDING'
		ORDER BY created_at ASC
		LIMIT $2;`

	var events []OutboxEvent
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, qErr := tx.Query(ctx, query, tenantID, limit)
		if qErr != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()
		for rows.Next() {
			var e OutboxEvent
			if scanErr := rows.Scan(&e.OutboxID, &e.TenantID, &e.EventType, &e.EventPayload, &e.CorrelationID, &e.Status, &e.Attempts, &e.CreatedAt); scanErr != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			events = append(events, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// GetPendingOutboxEventsAllTenants is the relay's system read. It is
// separate from the tenant-scoped method so a tenant-facing caller can
// never accidentally receive another tenant's events.
func (s *PgStore) GetPendingOutboxEventsAllTenants(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	const query = `
		SELECT outbox_id, tenant_id, event_type, event_payload, correlation_id, status, attempts, created_at
		FROM event_outbox
		WHERE status = 'PENDING'
		ORDER BY created_at ASC
		LIMIT $1;`
	rows, err := tx.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	var events []OutboxEvent
	for rows.Next() {
		var e OutboxEvent
		if scanErr := rows.Scan(&e.OutboxID, &e.TenantID, &e.EventType, &e.EventPayload, &e.CorrelationID, &e.Status, &e.Attempts, &e.CreatedAt); scanErr != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return events, tx.Commit(ctx)
}

// OutboxMaxAttempts is how many failed publishes park an event as FAILED. With
// the backoff below (2^n seconds, capped at 5 minutes) that is about 16 hours
// of continuous broker failure — long enough that a parked row means a real
// incident, which the failed gauge alerts on, not a blip.
const OutboxMaxAttempts = 200

// RelayOutbox claims up to limit due events, hands each to publish, and records
// the outcome in the same transaction. FOR UPDATE SKIP LOCKED means two relay
// replicas never claim the same row, so an event is not published twice by
// concurrent workers (at-least-once still holds across a crash between the
// publish and the commit). A failed publish is retried with exponential
// backoff; only OutboxMaxAttempts consecutive failures park it.
func (s *PgStore) RelayOutbox(ctx context.Context, limit int, publish func(ctx context.Context, e OutboxEvent) error) (published, failed int, err error) {
	if limit <= 0 {
		limit = 100
	}
	err = s.withOutboxRelayTx(ctx, func(tx pgx.Tx) error {
		const claim = `
			SELECT outbox_id, tenant_id, event_type, event_payload, correlation_id, status, attempts, created_at
			FROM event_outbox
			WHERE status = 'PENDING' AND next_attempt_at <= NOW()
			ORDER BY created_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED;`
		rows, qErr := tx.Query(ctx, claim, limit)
		if qErr != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		var batch []OutboxEvent
		for rows.Next() {
			var e OutboxEvent
			if scanErr := rows.Scan(&e.OutboxID, &e.TenantID, &e.EventType, &e.EventPayload, &e.CorrelationID, &e.Status, &e.Attempts, &e.CreatedAt); scanErr != nil {
				rows.Close()
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			batch = append(batch, e)
		}
		rows.Close()
		if rErr := rows.Err(); rErr != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, rErr)
		}

		const markPublished = `UPDATE event_outbox SET status = 'PUBLISHED', published_at = NOW(), last_error = NULL WHERE outbox_id = $1;`
		const markFailed = `
			UPDATE event_outbox
			SET attempts        = attempts + 1,
			    last_error      = $2,
			    next_attempt_at = NOW() + LEAST(POWER(2, LEAST(attempts, 16)), 300) * INTERVAL '1 second',
			    status          = CASE WHEN attempts + 1 >= $3 THEN 'FAILED' ELSE 'PENDING' END,
			    failed_at       = CASE WHEN attempts + 1 >= $3 THEN NOW() ELSE failed_at END
			WHERE outbox_id = $1;`
		for _, e := range batch {
			if pubErr := publish(ctx, e); pubErr != nil {
				failed++
				if _, uErr := tx.Exec(ctx, markFailed, e.OutboxID, pubErr.Error(), OutboxMaxAttempts); uErr != nil {
					return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, uErr)
				}
				continue
			}
			published++
			if _, uErr := tx.Exec(ctx, markPublished, e.OutboxID); uErr != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, uErr)
			}
		}
		return nil
	})
	return published, failed, err
}

// OutboxStats is what the alerting gauges read: undelivered events, parked
// events, and the age of the oldest undelivered one.
type OutboxStats struct {
	Pending             int
	Failed              int
	OldestPendingSecond float64
}

// GetOutboxStats reads OutboxStats across tenants (relay policy).
func (s *PgStore) GetOutboxStats(ctx context.Context) (OutboxStats, error) {
	var st OutboxStats
	err := s.withOutboxRelayTx(ctx, func(tx pgx.Tx) error {
		const q = `
			SELECT COUNT(*) FILTER (WHERE status = 'PENDING'),
			       COUNT(*) FILTER (WHERE status = 'FAILED'),
			       COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(created_at) FILTER (WHERE status = 'PENDING')), 0)
			FROM event_outbox;`
		if qErr := tx.QueryRow(ctx, q).Scan(&st.Pending, &st.Failed, &st.OldestPendingSecond); qErr != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		return nil
	})
	return st, err
}

// RequeueFailedOutbox puts parked events back on the relay, after the
// incident that parked them is resolved. Returns how many were requeued.
func (s *PgStore) RequeueFailedOutbox(ctx context.Context) (int64, error) {
	var n int64
	err := s.withOutboxRelayTx(ctx, func(tx pgx.Tx) error {
		tag, uErr := tx.Exec(ctx, `UPDATE event_outbox SET status = 'PENDING', attempts = 0, failed_at = NULL, next_attempt_at = NOW() WHERE status = 'FAILED';`)
		if uErr != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, uErr)
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}

// MarkOutboxEventPublished marks an outbox event as successfully published.
func (s *PgStore) MarkOutboxEventPublished(ctx context.Context, outboxID string) error {
	const query = `
		UPDATE event_outbox
		SET status = 'PUBLISHED', published_at = NOW()
		WHERE outbox_id = $1;`
	return s.withOutboxRelayTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, query, outboxID); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return nil
	})
}

// MarkOutboxEventFailed records a failed publish and schedules the retry with
// the same backoff and parking threshold as RelayOutbox.
func (s *PgStore) MarkOutboxEventFailed(ctx context.Context, outboxID string, errMsg string) error {
	const query = `
		UPDATE event_outbox
		SET attempts        = attempts + 1,
		    last_error      = $2,
		    next_attempt_at = NOW() + LEAST(POWER(2, LEAST(attempts, 16)), 300) * INTERVAL '1 second',
		    status          = CASE WHEN attempts + 1 >= $3 THEN 'FAILED' ELSE 'PENDING' END,
		    failed_at       = CASE WHEN attempts + 1 >= $3 THEN NOW() ELSE failed_at END
		WHERE outbox_id = $1;`
	return s.withOutboxRelayTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, query, outboxID, errMsg, OutboxMaxAttempts); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		return nil
	})
}

// FindOverdueObligations returns obligations that are OPEN or IN_PROGRESS
// and whose due_date has passed. If tenantID is empty, returns all
// obligations across all tenants (for scheduler).
func (s *PgStore) FindOverdueObligations(ctx context.Context, tenantID string) ([]*domain.Obligation, error) {
	now := time.Now().UTC()

	if tenantID != "" {
		// Use tenant-scoped transaction for single tenant
		var results []*domain.Obligation
		err := s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
			const q = `
				SELECT ` + obligationColumns + `
				FROM obligations
				WHERE tenant_id = $1
				  AND obligation_status IN ('OPEN', 'IN_PROGRESS')
				  AND due_date < $2
				ORDER BY due_date ASC;`
			rows, qErr := tx.Query(ctx, q, tenantID, now)
			if qErr != nil {
				s.log.Error("pg FindOverdueObligations failed", zap.Error(qErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
			}
			defer rows.Close()

			for rows.Next() {
				o, scanErr := scanObligation(rows)
				if scanErr != nil {
					s.log.Error("pg FindOverdueObligations scan failed", zap.Error(scanErr))
					return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
				}
				results = append(results, o)
			}
			return rows.Err()
		})
	if err != nil {
		return nil, err
	}
	return results, nil
}

	// Cross-tenant query (for scheduler) - no tenant scoping
	const crossTenantQuery = `
		SELECT ` + obligationColumns + `
		FROM obligations
		WHERE obligation_status IN ('OPEN', 'IN_PROGRESS')
		  AND due_date < $1
		ORDER BY tenant_id, due_date ASC;`

	// Under the overdue_sweep_select policy: a bare pool read sees nothing
	// under FORCE RLS (see GetTenantIDsWithObligations).
	var results []*domain.Obligation
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.overdue_sweep', 'true', true)"); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	rows, err := tx.Query(ctx, crossTenantQuery, now)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	for rows.Next() {
		o, scanErr := scanObligation(rows)
		if scanErr != nil {
			s.log.Error("pg FindOverdueObligations cross-tenant scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, o)
	}
	return results, rows.Err()
}

// ListClocks returns clocks matching the given filter.
func (s *PgStore) ListClocks(ctx context.Context, filter domain.ListClocksFilter) ([]*domain.Clock, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	offset := filter.Offset

	var queryBuilder strings.Builder
	queryBuilder.WriteString(`
		SELECT ` + clockColumns + `
		FROM clocks
		WHERE tenant_id = $1`)
	
	args := []any{tenantID}
	argIdx := 2
	
	if filter.ObligationID != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND obligation_id = $%d", argIdx))
		args = append(args, filter.ObligationID)
		argIdx++
	}
	if filter.LegalEntityID != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND legal_entity_id = $%d", argIdx))
		args = append(args, filter.LegalEntityID)
		argIdx++
	}
	if filter.ClockClass != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND clock_class = $%d", argIdx))
		args = append(args, filter.ClockClass)
		argIdx++
	}
	if filter.ClockState != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND clock_state = $%d", argIdx))
		args = append(args, filter.ClockState)
		argIdx++
	}
	if filter.AfterCreatedAt != nil {
		queryBuilder.WriteString(fmt.Sprintf(" AND created_at < $%d", argIdx))
		args = append(args, *filter.AfterCreatedAt)
		argIdx++
	}
	if filter.AfterID != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND clock_id < $%d", argIdx))
		args = append(args, filter.AfterID)
		argIdx++
	}
	
	queryBuilder.WriteString(fmt.Sprintf(" ORDER BY created_at DESC, clock_id DESC LIMIT $%d OFFSET $%d;", argIdx, argIdx+1))
	args = append(args, limit, offset)

	var results []*domain.Clock
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		argsWithTenant := append([]any{}, args...)
		rows, qErr := tx.Query(ctx, queryBuilder.String(), argsWithTenant...)
		if qErr != nil {
			s.log.Error("pg ListClocks failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			c, scanErr := scanClock(rows)
			if scanErr != nil {
				s.log.Error("pg ListClocks scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// TransitionClock transitions a clock's state with optimistic locking.
// Returns the updated clock, whether a transition occurred, and any error.
func (s *PgStore) TransitionClock(ctx context.Context, params domain.ClockTransitionParams) (*domain.Clock, bool, error) {
	var updated *domain.Clock
	var transitioned bool

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		// Lock the clock row
		const lockQuery = `
			SELECT ` + clockColumns + `
			FROM clocks
			WHERE clock_id = $1 AND tenant_id = $2
			FOR UPDATE;`

		current, err := scanClock(tx.QueryRow(ctx, lockQuery, params.ClockID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrClockNotFound
			}
			if mapped := mapPgError(err); errors.Is(mapped, domain.ErrInvalidIdentifier) {
				return domain.ErrClockNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if params.ExpectedVersion > 0 && current.Version != params.ExpectedVersion {
			return domain.ErrClockVersionMismatch
		}

		// Check if transition is valid
		allowedTransitions := domain.ClockStateTransitions[current.ClockState]
		valid := false
		for _, s := range allowedTransitions {
			if s == params.NewState {
				valid = true
				break
			}
		}
		if !valid {
			return domain.ErrInvalidClockTransition
		}

		// If already in the target state, return current clock without transition
		if current.ClockState == params.NewState {
			updated = current
			return nil
		}

		// Prepare update fields
		now := time.Now().UTC()
		setClauses := []string{
			"clock_state = $1",
			"version = version + 1",
			"updated_at = NOW()",
		}
		args := []any{params.NewState}
		argIdx := 2

		// Add state-specific fields
		switch params.NewState {
		case domain.ClockStateFulfilled:
			if len(params.EvidenceEvaluationIDs) == 0 {
				return domain.ErrInvalidClockTransition // fulfilled requires evidence
			}
			setClauses = append(setClauses, "satisfied_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			setClauses = append(setClauses, "satisfied_by_principal_id = $"+strconv.Itoa(argIdx))
			args = append(args, params.ActorPrincipalID)
			argIdx++
			setClauses = append(setClauses, "satisfied_evidence_ids = $"+strconv.Itoa(argIdx))
			args = append(args, params.EvidenceEvaluationIDs)
			argIdx++
		case domain.ClockStateWaived:
			if params.Reason == nil || *params.Reason == "" {
				return domain.ErrWaiverAuthorityRequired
			}
			if params.AuthorityPrincipalID == nil || *params.AuthorityPrincipalID == "" {
				return domain.ErrWaiverAuthorityRequired
			}
			setClauses = append(setClauses, "waived_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			setClauses = append(setClauses, "waived_by_principal_id = $"+strconv.Itoa(argIdx))
			args = append(args, params.ActorPrincipalID)
			argIdx++
			setClauses = append(setClauses, "waived_reason = $"+strconv.Itoa(argIdx))
			args = append(args, *params.Reason)
			argIdx++
			setClauses = append(setClauses, "waived_authority_principal_id = $"+strconv.Itoa(argIdx))
			args = append(args, *params.AuthorityPrincipalID)
			argIdx++
		case domain.ClockStateNotApplicable:
			if params.NotApplicableReason == nil || *params.NotApplicableReason == "" {
				return domain.ErrNotApplicableAuthorityRequired
			}
			if params.NotApplicableAuthorityPrincipalID == nil || *params.NotApplicableAuthorityPrincipalID == "" {
				return domain.ErrNotApplicableAuthorityRequired
			}
			setClauses = append(setClauses, "not_applicable_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			setClauses = append(setClauses, "not_applicable_by_principal_id = $"+strconv.Itoa(argIdx))
			args = append(args, params.ActorPrincipalID)
			argIdx++
			setClauses = append(setClauses, "not_applicable_reason = $"+strconv.Itoa(argIdx))
			args = append(args, *params.NotApplicableReason)
			argIdx++
			setClauses = append(setClauses, "not_applicable_authority_principal_id = $"+strconv.Itoa(argIdx))
			args = append(args, *params.NotApplicableAuthorityPrincipalID)
			argIdx++
		case domain.ClockStateSuperseded:
			setClauses = append(setClauses, "superseded_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			if params.SupersededByClockID != nil && *params.SupersededByClockID != "" {
				setClauses = append(setClauses, "superseded_by_clock_id = $"+strconv.Itoa(argIdx))
				args = append(args, *params.SupersededByClockID)
				argIdx++
			}
		}

		// Add correlation_id
		if params.CorrelationID != "" {
			setClauses = append(setClauses, "correlation_id = $"+strconv.Itoa(argIdx))
			args = append(args, params.CorrelationID)
			argIdx++
		}

		// Build and execute update query
		updateQuery := fmt.Sprintf(`
			UPDATE clocks
			SET %s
			WHERE clock_id = $%d AND tenant_id = $%d
			RETURNING `+clockColumns+";", strings.Join(setClauses, ", "), argIdx, argIdx+1)
		
		args = append(args, params.ClockID, tenantID)

		row := tx.QueryRow(ctx, updateQuery, args...)
		updated, err = scanClock(row)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		transitioned = true
		return nil
	})

	if err != nil {
		return nil, false, err
	}
return updated, transitioned, nil
}

// CreateClock inserts a new clock on an obligation.
// Returns the created clock.
func (s *PgStore) CreateClock(ctx context.Context, params domain.CreateClockParams) (*domain.Clock, error) {
	var created *domain.Clock

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		// Verify obligation exists and get its legal_entity_id
		obligation, err := s.FindObligationByID(ctx, params.ObligationID)
		if err != nil {
			return err
		}

		// Verify the obligation's legal_entity matches
		if obligation.LegalEntityID != params.ObligationID {
			// Actually we need to check the obligation exists for this tenant
			// The FindObligationByID already does tenant scoping
		}

		const query = `
			INSERT INTO clocks (
				clock_id, obligation_id, tenant_id, legal_entity_id,
				clock_class, clock_state, calendar_code, calendar_rule_id,
				calendar_rule_version, computed_deadline, due_soon_threshold,
				escalation_ladder, created_by_principal_id, correlation_id,
				created_at, updated_at, version
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
				NOW(), NOW(), 1
			) RETURNING ` + clockColumns + `;`

		clockID := uuid.New().String()
		row := tx.QueryRow(ctx, query,
			clockID,
			params.ObligationID,
			tenantID,
			obligation.LegalEntityID,
			params.ClockClass,
			domain.ClockStatePlanned, // initial state is PLANNED
			params.CalendarCode,
			params.CalendarRuleID,
			params.CalendarRuleVersion,
			params.ComputedDeadline,
			params.DueSoonThreshold,
			params.EscalationLadder,
			params.CreatedByPrincipalID,
			params.CorrelationID,
		)

		created, err = scanClock(row)
		return err
	})

	if err != nil {
		return nil, err
	}
	return created, nil
}

// FindClockByID looks up a clock by its UUID primary key.
func (s *PgStore) FindClockByID(ctx context.Context, clockID string) (*domain.Clock, error) {
	var c *domain.Clock
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const query = `
			SELECT ` + clockColumns + `
			FROM clocks
			WHERE clock_id = $1 AND tenant_id = $2;`
		var scanErr error
		c, scanErr = scanClock(tx.QueryRow(ctx, query, clockID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrClockNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrClockNotFound
		}
		s.log.Error("pg FindClockByID failed", zap.String("clock_id", clockID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

// FindActiveClocksForObligation returns active clocks for an obligation.
// Active clocks are those in PLANNED, ACTIVE, DUE_SOON, DUE, or OVERDUE states.
func (s *PgStore) FindActiveClocksForObligation(ctx context.Context, obligationID string) ([]*domain.Clock, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	const query = `
		SELECT ` + clockColumns + `
		FROM clocks
		WHERE obligation_id = $1
		  AND tenant_id = $2
		  AND clock_state IN ('PLANNED', 'ACTIVE', 'DUE_SOON', 'DUE', 'OVERDUE')
		ORDER BY created_at DESC, clock_id DESC;`

	var results []*domain.Clock
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, obligationID, tenantID)
		if qErr != nil {
			s.log.Error("pg FindActiveClocksForObligation failed", zap.String("obligation_id", obligationID), zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			c, scanErr := scanClock(rows)
			if scanErr != nil {
				s.log.Error("pg FindActiveClocksForObligation scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// GetTenantIDsWithObligations returns the tenants that have at least one
// OPEN or IN_PROGRESS obligation past its due date — the tenants the overdue
// sweep has work in.
//
// obligations is under FORCE RLS and the runtime role does not bypass it, so a
// bare pool read with no app.tenant_id sees no rows at all: the sweep used to
// find no tenants and transition nothing (it worked only as a superuser). The
// read runs under the SELECT-only overdue_sweep_select policy (000012); the
// transitions themselves run per tenant under the ordinary tenant policy.
func (s *PgStore) GetTenantIDsWithObligations(ctx context.Context) ([]string, error) {
	const query = `
		SELECT DISTINCT tenant_id::text
		FROM obligations
		WHERE obligation_status IN ('OPEN', 'IN_PROGRESS') AND due_date < NOW();`

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.overdue_sweep', 'true', true)"); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var tenantIDs []string
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		tenantIDs = append(tenantIDs, tid)
	}
	return tenantIDs, rows.Err()
}

// ── Compliance Status ────────────────────────────────────────────────────────

const complianceStatusColumns = `
	compliance_status_id,
	tenant_id,
	legal_entity_id,
	domain_code,
	status_code,
	status_reason,
	evaluated_at,
	evidence_sufficiency_state,
	created_at,
	created_by_principal_id,
	updated_at,
	version`

func scanComplianceStatus(row pgx.Row) (*domain.ComplianceStatus, error) {
	cs := &domain.ComplianceStatus{}
	err := row.Scan(
		&cs.ComplianceStatusID,
		&cs.TenantID,
		&cs.LegalEntityID,
		&cs.DomainCode,
		&cs.StatusCode,
		&cs.StatusReason,
		&cs.EvaluatedAt,
		&cs.EvidenceSufficiencyState,
		&cs.CreatedAt,
		&cs.CreatedByPrincipalID,
		&cs.UpdatedAt,
		&cs.Version,
	)
	return cs, err
}

func (s *PgStore) CreateComplianceStatus(ctx context.Context, params domain.CreateComplianceStatusParams) (*domain.ComplianceStatus, error) {
	if params.ComplianceStatusID == "" {
		params.ComplianceStatusID = uuid.New().String()
	}

	const query = `
		INSERT INTO compliance_status (
			compliance_status_id, tenant_id, legal_entity_id, domain_code, status_code,
			status_reason, evaluated_at, evidence_sufficiency_state, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING ` + complianceStatusColumns + `;`

	var cs *domain.ComplianceStatus
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		cs, scanErr = scanComplianceStatus(tx.QueryRow(ctx, query,
			params.ComplianceStatusID, tenantID, params.LegalEntityID, params.DomainCode,
			params.StatusCode, params.StatusReason, params.EvaluatedAt,
			params.EvidenceSufficiencyState, params.CreatedByPrincipalID))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg CreateComplianceStatus failed", zap.Error(scanErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		return insertOutboxEventTx(ctx, tx, tenantID, "compliance.status.changed", params.CorrelationID,
			params.LegalEntityID, "", params.CreatedByPrincipalID, map[string]any{
				"compliance_status_id": cs.ComplianceStatusID,
				"legal_entity_id":      cs.LegalEntityID,
				"domain_code":          cs.DomainCode,
				"status_code":          cs.StatusCode,
				"evaluated_at":         cs.EvaluatedAt,
			})
	})
	if err != nil {
		return nil, err
	}
	return cs, nil
}

func (s *PgStore) FindComplianceStatusByID(ctx context.Context, complianceStatusID string) (*domain.ComplianceStatus, error) {
	const query = `
		SELECT ` + complianceStatusColumns + `
		FROM compliance_status
		WHERE compliance_status_id = $1 AND tenant_id = $2;`

	var cs *domain.ComplianceStatus
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		cs, scanErr = scanComplianceStatus(tx.QueryRow(ctx, query, complianceStatusID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrComplianceStatusNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrComplianceStatusNotFound
		}
		s.log.Error("pg FindComplianceStatusByID failed", zap.String("compliance_status_id", complianceStatusID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return cs, nil
}

func (s *PgStore) ListComplianceStatuses(ctx context.Context, filter domain.ListComplianceStatusFilter) ([]*domain.ComplianceStatus, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if filter.TenantID != "" && filter.TenantID != tenantID {
		return nil, domain.ErrTenantMissing
	}

	query := `SELECT ` + complianceStatusColumns + ` FROM compliance_status WHERE tenant_id = $1`
	args := []any{tenantID}

	addFilter := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}

	if filter.LegalEntityID != "" {
		addFilter("legal_entity_id =", filter.LegalEntityID)
	}
	if filter.DomainCode != "" {
		addFilter("domain_code =", filter.DomainCode)
	}
	if filter.StatusCode != "" {
		addFilter("status_code =", filter.StatusCode)
	}

	query += " ORDER BY evaluated_at DESC, compliance_status_id DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	args = append(args, filter.Offset)
	query += fmt.Sprintf(" OFFSET $%d;", len(args))

	var results []*domain.ComplianceStatus
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, args...)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListComplianceStatuses failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			cs, scanErr := scanComplianceStatus(rows)
			if scanErr != nil {
				s.log.Error("pg ListComplianceStatuses scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, cs)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *PgStore) UpdateComplianceStatus(ctx context.Context, complianceStatusID, statusCode, statusReason, evidenceSufficiencyState string, expectedVersion int, actorID, correlationID string) (*domain.ComplianceStatus, error) {
	var updated *domain.ComplianceStatus

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `
			SELECT ` + complianceStatusColumns + `
			FROM compliance_status
			WHERE compliance_status_id = $1 AND tenant_id = $2
			FOR UPDATE;`

		current, err := scanComplianceStatus(tx.QueryRow(ctx, lockQuery, complianceStatusID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrComplianceStatusNotFound
			}
			if mapped := mapPgError(err); errors.Is(mapped, domain.ErrInvalidIdentifier) {
				return domain.ErrComplianceStatusNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if expectedVersion > 0 && current.Version != expectedVersion {
			return domain.ErrVersionMismatch
		}

		if current.StatusCode == statusCode && current.StatusReason == statusReason && current.EvidenceSufficiencyState == evidenceSufficiencyState {
			updated = current
			return nil
		}

		const updateQuery = `
			UPDATE compliance_status
			SET status_code = $1,
			    status_reason = $2,
			    evidence_sufficiency_state = $3,
			    updated_at = NOW(),
			    version = version + 1
			WHERE compliance_status_id = $4 AND tenant_id = $5
			RETURNING ` + complianceStatusColumns + `;`

		updated, err = scanComplianceStatus(tx.QueryRow(ctx, updateQuery, statusCode, statusReason, evidenceSufficiencyState, complianceStatusID, tenantID))
		if err != nil {
			s.log.Error("pg UpdateComplianceStatus failed",
				zap.String("compliance_status_id", complianceStatusID),
				zap.Error(err))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		return insertOutboxEventTx(ctx, tx, tenantID, "compliance.status.changed", correlationID,
			updated.LegalEntityID, "", actorID, map[string]any{
				"compliance_status_id":       updated.ComplianceStatusID,
				"previous_status_code":       current.StatusCode,
				"new_status_code":            updated.StatusCode,
				"evidence_sufficiency_state": updated.EvidenceSufficiencyState,
				"version":                    updated.Version,
			})
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// ── Exception Cases ──────────────────────────────────────────────────────────

const exceptionCaseColumns = `
	exception_case_id,
	tenant_id,
	legal_entity_id,
	exception_type,
	severity_level,
	linked_object_type,
	linked_object_id,
	case_status,
	escalated_at,
	created_at,
	created_by_principal_id,
	updated_at,
	version`

func scanExceptionCase(row pgx.Row) (*domain.ExceptionCase, error) {
	ec := &domain.ExceptionCase{}
	err := row.Scan(
		&ec.ExceptionCaseID,
		&ec.TenantID,
		&ec.LegalEntityID,
		&ec.ExceptionType,
		&ec.SeverityLevel,
		&ec.LinkedObjectType,
		&ec.LinkedObjectID,
		&ec.CaseStatus,
		&ec.EscalatedAt,
		&ec.CreatedAt,
		&ec.CreatedByPrincipalID,
		&ec.UpdatedAt,
		&ec.Version,
	)
	return ec, err
}

func (s *PgStore) CreateExceptionCase(ctx context.Context, params domain.CreateExceptionCaseParams) (*domain.ExceptionCase, error) {
	if params.ExceptionCaseID == "" {
		params.ExceptionCaseID = uuid.New().String()
	}

	const query = `
		INSERT INTO exception_cases (
			exception_case_id, tenant_id, legal_entity_id, exception_type, severity_level,
			linked_object_type, linked_object_id, case_status, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 'OPEN', $8)
		RETURNING ` + exceptionCaseColumns + `;`

	var ec *domain.ExceptionCase
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		ec, scanErr = scanExceptionCase(tx.QueryRow(ctx, query,
			params.ExceptionCaseID, tenantID, params.LegalEntityID, params.ExceptionType,
			params.SeverityLevel, params.LinkedObjectType, params.LinkedObjectID,
			params.CreatedByPrincipalID))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg CreateExceptionCase failed", zap.Error(scanErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		return insertOutboxEventTx(ctx, tx, tenantID, "exception.created", params.CorrelationID,
			params.LegalEntityID, "", params.CreatedByPrincipalID, map[string]any{
				"exception_case_id": ec.ExceptionCaseID,
				"exception_type":    ec.ExceptionType,
				"severity_level":    ec.SeverityLevel,
				"linked_object_type": ec.LinkedObjectType,
				"linked_object_id":   ec.LinkedObjectID,
			})
	})
	if err != nil {
		return nil, err
	}
	return ec, nil
}

func (s *PgStore) FindExceptionCaseByID(ctx context.Context, exceptionCaseID string) (*domain.ExceptionCase, error) {
	const query = `
		SELECT ` + exceptionCaseColumns + `
		FROM exception_cases
		WHERE exception_case_id = $1 AND tenant_id = $2;`

	var ec *domain.ExceptionCase
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		ec, scanErr = scanExceptionCase(tx.QueryRow(ctx, query, exceptionCaseID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrExceptionCaseNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrExceptionCaseNotFound
		}
		s.log.Error("pg FindExceptionCaseByID failed", zap.String("exception_case_id", exceptionCaseID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return ec, nil
}

func (s *PgStore) ListExceptionCases(ctx context.Context, filter domain.ListExceptionCasesFilter) ([]*domain.ExceptionCase, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if filter.TenantID != "" && filter.TenantID != tenantID {
		return nil, domain.ErrTenantMissing
	}

	query := `SELECT ` + exceptionCaseColumns + ` FROM exception_cases WHERE tenant_id = $1`
	args := []any{tenantID}

	addFilter := func(clause string, value any) {
		args = append(args, value)
		query += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}

	if filter.LegalEntityID != "" {
		addFilter("legal_entity_id =", filter.LegalEntityID)
	}
	if filter.ExceptionType != "" {
		addFilter("exception_type =", filter.ExceptionType)
	}
	if filter.CaseStatus != "" {
		addFilter("case_status =", filter.CaseStatus)
	}

	query += " ORDER BY created_at DESC, exception_case_id DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	args = append(args, filter.Offset)
	query += fmt.Sprintf(" OFFSET $%d;", len(args))

	var results []*domain.ExceptionCase
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, args...)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListExceptionCases failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			ec, scanErr := scanExceptionCase(rows)
			if scanErr != nil {
				s.log.Error("pg ListExceptionCases scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, ec)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *PgStore) UpdateExceptionCaseStatus(ctx context.Context, exceptionCaseID, caseStatus string, expectedVersion int, actorID, correlationID string) (*domain.ExceptionCase, error) {
	var updated *domain.ExceptionCase

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const lockQuery = `
			SELECT ` + exceptionCaseColumns + `
			FROM exception_cases
			WHERE exception_case_id = $1 AND tenant_id = $2
			FOR UPDATE;`

		current, err := scanExceptionCase(tx.QueryRow(ctx, lockQuery, exceptionCaseID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrExceptionCaseNotFound
			}
			if mapped := mapPgError(err); errors.Is(mapped, domain.ErrInvalidIdentifier) {
				return domain.ErrExceptionCaseNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if expectedVersion > 0 && current.Version != expectedVersion {
			return domain.ErrVersionMismatch
		}

		if current.CaseStatus == caseStatus {
			updated = current
			return nil
		}

		escalatedAt := current.EscalatedAt
		if caseStatus == "ESCALATED" && current.EscalatedAt == nil {
			now := time.Now().UTC()
			escalatedAt = &now
		}

		const updateQuery = `
			UPDATE exception_cases
			SET case_status = $1,
			    escalated_at = $2,
			    updated_at = NOW(),
			    version = version + 1
			WHERE exception_case_id = $3 AND tenant_id = $4
			RETURNING ` + exceptionCaseColumns + `;`

		updated, err = scanExceptionCase(tx.QueryRow(ctx, updateQuery, caseStatus, escalatedAt, exceptionCaseID, tenantID))
		if err != nil {
			s.log.Error("pg UpdateExceptionCaseStatus failed",
				zap.String("exception_case_id", exceptionCaseID),
				zap.Error(err))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		eventType := "exception.updated"
		if caseStatus == "ESCALATED" {
			eventType = "exception.escalated"
		} else if caseStatus == "CLOSED" || caseStatus == "RESOLVED" {
			eventType = "exception.closed"
		}

		return insertOutboxEventTx(ctx, tx, tenantID, eventType, correlationID,
			updated.LegalEntityID, "", actorID, map[string]any{
				"exception_case_id": updated.ExceptionCaseID,
				"previous_status":   current.CaseStatus,
				"new_status":        updated.CaseStatus,
				"version":           updated.Version,
			})
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// ── Escalation Records ───────────────────────────────────────────────────────

const escalationRecordColumns = `
	escalation_record_id,
	exception_case_id,
	escalated_to_role,
	escalation_reason,
	escalation_status,
	escalated_at,
	created_at,
	created_by_principal_id`

func scanEscalationRecord(row pgx.Row) (*domain.EscalationRecord, error) {
	er := &domain.EscalationRecord{}
	err := row.Scan(
		&er.EscalationRecordID,
		&er.ExceptionCaseID,
		&er.EscalatedToRole,
		&er.EscalationReason,
		&er.EscalationStatus,
		&er.EscalatedAt,
		&er.CreatedAt,
		&er.CreatedByPrincipalID,
	)
	return er, err
}

func (s *PgStore) CreateEscalationRecord(ctx context.Context, params domain.CreateEscalationRecordParams) (*domain.EscalationRecord, error) {
	if params.EscalationRecordID == "" {
		params.EscalationRecordID = uuid.New().String()
	}

	const query = `
		INSERT INTO escalation_records (
			escalation_record_id, exception_case_id, escalated_to_role, escalation_reason,
			escalation_status, escalated_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, 'PENDING', NOW(), $5)
		RETURNING ` + escalationRecordColumns + `;`

	var er *domain.EscalationRecord
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		er, scanErr = scanEscalationRecord(tx.QueryRow(ctx, query,
			params.EscalationRecordID, params.ExceptionCaseID, params.EscalatedToRole,
			params.EscalationReason, params.CreatedByPrincipalID))
		if scanErr != nil {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg CreateEscalationRecord failed", zap.Error(scanErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		return insertOutboxEventTx(ctx, tx, tenantID, "escalation.created", params.CorrelationID,
			"", "", params.CreatedByPrincipalID, map[string]any{
				"escalation_record_id": er.EscalationRecordID,
				"exception_case_id":    er.ExceptionCaseID,
				"escalated_to_role":    er.EscalatedToRole,
			})
	})
	if err != nil {
		return nil, err
	}
	return er, nil
}

func (s *PgStore) FindEscalationRecordByID(ctx context.Context, escalationRecordID string) (*domain.EscalationRecord, error) {
	const query = `
		SELECT ` + escalationRecordColumns + `
		FROM escalation_records
		WHERE escalation_record_id = $1 AND tenant_id = $2;`

	var er *domain.EscalationRecord
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		var scanErr error
		er, scanErr = scanEscalationRecord(tx.QueryRow(ctx, query, escalationRecordID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrEscalationRecordNotFound
		}
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrEscalationRecordNotFound
		}
		s.log.Error("pg FindEscalationRecordByID failed", zap.String("escalation_record_id", escalationRecordID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return er, nil
}

func (s *PgStore) ListEscalationRecords(ctx context.Context, exceptionCaseID string, limit int) ([]*domain.EscalationRecord, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	if limit <= 0 {
		limit = defaultListLimit
	}

	const query = `
		SELECT ` + escalationRecordColumns + `
		FROM escalation_records
		WHERE exception_case_id = $1 AND tenant_id = $2
		ORDER BY escalated_at DESC, escalation_record_id DESC
		LIMIT $3;`

	var results []*domain.EscalationRecord
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		rows, qErr := tx.Query(ctx, query, exceptionCaseID, tenantID, limit)
		if qErr != nil {
			if errors.Is(mapPgError(qErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
			}
			s.log.Error("pg ListEscalationRecords failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			er, scanErr := scanEscalationRecord(rows)
			if scanErr != nil {
				s.log.Error("pg ListEscalationRecords scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, er)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

