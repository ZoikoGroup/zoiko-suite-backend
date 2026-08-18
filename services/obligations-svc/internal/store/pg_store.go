// Package store provides the PostgreSQL implementation of the obligations
// read and write model.
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers or domain packages.
package store

import (
	"context"
	"errors"
	"fmt"

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
	UpdateObligationStatus(ctx context.Context, obligationID, newStatus string) (*domain.Obligation, bool, error)

	// CreateFilingRequirement inserts a new filing requirement under an
	// obligation. Fails with domain.ErrObligationNotFound if obligation_id
	// does not exist.
	CreateFilingRequirement(ctx context.Context, params domain.CreateFilingRequirementParams) (*domain.FilingRequirement, error)

	// ListFilingRequirements returns all filing requirements for an
	// obligation. Fails with domain.ErrObligationNotFound if obligation_id
	// does not exist.
	ListFilingRequirements(ctx context.Context, obligationID string) ([]*domain.FilingRequirement, error)

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
	closed_at`

// scanObligation scans one row produced by an obligationColumns SELECT.
func scanObligation(row pgx.Row) (*domain.Obligation, error) {
	o := &domain.Obligation{}
	err := row.Scan(
		&o.ObligationID,
		&o.LegalEntityID,
		&o.JurisdictionID,
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
	)
	return o, err
}

// FindObligationByID looks up an obligation by its UUID primary key.
func (s *PgStore) FindObligationByID(ctx context.Context, obligationID string) (*domain.Obligation, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	// Another tenant's obligation reads as not-found — the same answer as one
	// that does not exist, deliberately, so a caller cannot probe for the
	// existence of records they may not read.
	const query = `
		SELECT ` + obligationColumns + `
		FROM obligations
		WHERE obligation_id = $1 AND tenant_id = $2;`

	row := s.pool.QueryRow(ctx, query, obligationID, tenantID)
	o, err := scanObligation(row)
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

	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, false, err
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
			obligation_id, tenant_id, legal_entity_id, jurisdiction_id, obligation_source_type,
			obligation_source_id, obligation_code, obligation_type, due_date,
			severity_level, responsible_function, source_reference, created_by_principal_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (tenant_id, obligation_code)
		DO NOTHING
		RETURNING ` + obligationColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.ObligationID, tenantID, params.LegalEntityID, params.JurisdictionID, params.ObligationSourceType,
		params.ObligationSourceID, params.ObligationCode, params.ObligationType, params.DueDate,
		params.SeverityLevel, params.ResponsibleFunction, params.SourceReference, params.CreatedByPrincipalID,
	)

	o, err := scanObligation(row)
	if err == nil {
		return o, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, false, domain.ErrInvalidIdentifier
		}
		s.log.Error("pg CreateObligation failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Conflict on (tenant_id, obligation_code). Look the existing one up
	// within THIS tenant — a lookup by code alone is what made the replay
	// cross-tenant in the first place.
	const lookupQuery = `
		SELECT ` + obligationColumns + `
		FROM obligations
		WHERE obligation_code = $1 AND tenant_id = $2;`

	row = s.pool.QueryRow(ctx, lookupQuery, params.ObligationCode, tenantID)
	o, err = scanObligation(row)
	if err != nil {
		s.log.Error("pg CreateObligation lookup existing failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if o.LegalEntityID != params.LegalEntityID ||
		o.JurisdictionID != params.JurisdictionID ||
		o.ObligationType != params.ObligationType ||
		!o.DueDate.Equal(params.DueDate) {
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

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrInvalidIdentifier
		}
		s.log.Error("pg ListObligations failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.Obligation
	for rows.Next() {
		o, scanErr := scanObligation(rows)
		if scanErr != nil {
			s.log.Error("pg ListObligations scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, o)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg ListObligations rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
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
func (s *PgStore) UpdateObligationStatus(ctx context.Context, obligationID, newStatus string) (*domain.Obligation, bool, error) {
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
			    closed_at = CASE WHEN $1::VARCHAR = 'CLOSED' THEN NOW() ELSE closed_at END
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
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return updated, transitioned, nil
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
func (s *PgStore) CreateFilingRequirement(ctx context.Context, params domain.CreateFilingRequirementParams) (*domain.FilingRequirement, error) {
	if _, err := s.FindObligationByID(ctx, params.ObligationID); err != nil {
		return nil, err
	}

	if params.FilingRequirementID == "" {
		params.FilingRequirementID = uuid.New().String()
	}

	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	// tenant_id is stored on the row rather than reached through the parent
	// obligation, so row-level security applies to a bare SELECT here. A
	// policy that has to join to find its tenant is a policy that does not run.
	const query = `
		INSERT INTO filing_requirements (
			filing_requirement_id, tenant_id, obligation_id, filing_type, filing_authority,
			submission_channel, filing_status
		) VALUES ($1, $2, $3, $4, $5, $6, 'PENDING')
		RETURNING ` + filingRequirementColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.FilingRequirementID, tenantID, params.ObligationID, params.FilingType,
		params.FilingAuthority, params.SubmissionChannel,
	)

	f, err := scanFilingRequirement(row)
	if err != nil {
		if errors.Is(mapPgError(err), domain.ErrInvalidIdentifier) {
			return nil, domain.ErrInvalidIdentifier
		}
		s.log.Error("pg CreateFilingRequirement failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return f, nil
}

// ListFilingRequirements returns all filing requirements for an obligation.
func (s *PgStore) ListFilingRequirements(ctx context.Context, obligationID string) ([]*domain.FilingRequirement, error) {
	if _, err := s.FindObligationByID(ctx, obligationID); err != nil {
		return nil, err
	}

	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	const query = `
		SELECT ` + filingRequirementColumns + `
		FROM filing_requirements
		WHERE obligation_id = $1 AND tenant_id = $2
		ORDER BY created_at DESC, filing_requirement_id DESC;`

	rows, err := s.pool.Query(ctx, query, obligationID, tenantID)
	if err != nil {
		s.log.Error("pg ListFilingRequirements failed", zap.String("obligation_id", obligationID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.FilingRequirement
	for rows.Next() {
		f, scanErr := scanFilingRequirement(rows)
		if scanErr != nil {
			s.log.Error("pg ListFilingRequirements scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, f)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg ListFilingRequirements rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}
