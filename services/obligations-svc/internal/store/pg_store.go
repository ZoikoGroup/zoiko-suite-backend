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
	UpdateObligationWorkflow(ctx context.Context, obligationID, workflowID string) (*domain.Obligation, error)

	// CreateFilingRequirement inserts a new filing requirement under an
	// obligation. Fails with domain.ErrObligationNotFound if obligation_id
	// does not exist.
	CreateFilingRequirement(ctx context.Context, params domain.CreateFilingRequirementParams) (*domain.FilingRequirement, error)

	// ListFilingRequirements returns all filing requirements for an
	// obligation. Fails with domain.ErrObligationNotFound if obligation_id
	// does not exist.
	ListFilingRequirements(ctx context.Context, obligationID string) ([]*domain.FilingRequirement, error)

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
	CreateApplicabilityDecision(ctx context.Context, params domain.CreateApplicabilityDecisionParams) (*domain.ApplicabilityDecision, error)
	ListApplicabilityDecisions(ctx context.Context, obligationID, jurisdictionCode, entityRef string) ([]*domain.ApplicabilityDecision, error)
	FindCurrentApplicability(ctx context.Context, obligationID, jurisdictionCode, entityRef string) (*domain.CurrentApplicability, error)
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
	version`

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
			severity_level, responsible_function, source_reference, created_by_principal_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
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
		var scanErr error
		o, scanErr = scanObligation(tx.QueryRow(ctx, query,
			params.ObligationID, tenantID, params.LegalEntityID, params.JurisdictionID, params.WorkflowID, params.ObligationSourceType,
			params.ObligationSourceID, params.ObligationCode, params.ObligationType, params.DueDate,
			params.SeverityLevel, params.ResponsibleFunction, params.SourceReference, params.CreatedByPrincipalID,
		))
		if scanErr == nil {
			created = true
			return insertOutboxEventTx(ctx, tx, tenantID, "obligation.created", params.CorrelationID,
				o.LegalEntityID, o.JurisdictionID, o.CreatedByPrincipalID, map[string]any{
					"obligation_id":           o.ObligationID,
					"legal_entity_id":         o.LegalEntityID,
					"jurisdiction_id":         o.JurisdictionID,
					"obligation_code":         o.ObligationCode,
					"obligation_type":         o.ObligationType,
					"due_date":                o.DueDate,
					"severity_level":          o.SeverityLevel,
					"source_reference":        o.SourceReference,
					"created_by_principal_id": o.CreatedByPrincipalID,
					"created_at":              o.CreatedAt,
				})
		}
		if !errors.Is(scanErr, pgx.ErrNoRows) {
			if errors.Is(mapPgError(scanErr), domain.ErrInvalidIdentifier) {
				return domain.ErrInvalidIdentifier
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
		o.ResponsibleFunction != params.ResponsibleFunction {
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
// An empty workflowID unlinks the obligation.
func (s *PgStore) UpdateObligationWorkflow(ctx context.Context, obligationID, workflowID string) (*domain.Obligation, error) {
	var updated *domain.Obligation

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		const query = `
			UPDATE obligations
			SET workflow_id = $1,
			    updated_at = NOW()
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
		return nil
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
	created_at`

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
	)
	return f, err
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
func (s *PgStore) ListFilingRequirements(ctx context.Context, obligationID string) ([]*domain.FilingRequirement, error) {
	if _, err := s.FindObligationByID(ctx, obligationID); err != nil {
		return nil, err
	}

	const query = `
		SELECT ` + filingRequirementColumns + `
		FROM filing_requirements
		WHERE obligation_id = $1 AND tenant_id = $2
		ORDER BY created_at DESC, filing_requirement_id DESC;`

	var results []*domain.FilingRequirement
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, qErr := tx.Query(ctx, query, obligationID, tenantID)
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

// MarkOutboxEventFailed marks an outbox event as failed and increments attempts.
func (s *PgStore) MarkOutboxEventFailed(ctx context.Context, outboxID string, errMsg string) error {
	const query = `
		UPDATE event_outbox
		SET status = CASE WHEN attempts + 1 >= 5 THEN 'FAILED' ELSE 'PENDING' END,
		    attempts = attempts + 1,
		    last_error = $2
		WHERE outbox_id = $1;`
	return s.withOutboxRelayTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, query, outboxID, errMsg); err != nil {
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

	var results []*domain.Obligation
	rows, err := s.pool.Query(ctx, crossTenantQuery, now)
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

// GetTenantIDsWithObligations returns all tenant IDs that have at least one obligation.
func (s *PgStore) GetTenantIDsWithObligations(ctx context.Context) ([]string, error) {
	const query = `
		SELECT DISTINCT tenant_id::text
		FROM obligations;`

	rows, err := s.pool.Query(ctx, query)
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
