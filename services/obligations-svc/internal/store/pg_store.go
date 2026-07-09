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
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
)

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
	const query = `
		SELECT ` + obligationColumns + `
		FROM obligations
		WHERE obligation_id = $1;`

	row := s.pool.QueryRow(ctx, query, obligationID)
	o, err := scanObligation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
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

	const query = `
		INSERT INTO obligations (
			obligation_id, legal_entity_id, jurisdiction_id, obligation_source_type,
			obligation_source_id, obligation_code, obligation_type, due_date,
			severity_level, responsible_function, source_reference, created_by_principal_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (obligation_code)
		DO NOTHING
		RETURNING ` + obligationColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.ObligationID, params.LegalEntityID, params.JurisdictionID, params.ObligationSourceType,
		params.ObligationSourceID, params.ObligationCode, params.ObligationType, params.DueDate,
		params.SeverityLevel, params.ResponsibleFunction, params.SourceReference, params.CreatedByPrincipalID,
	)

	o, err := scanObligation(row)
	if err == nil {
		return o, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreateObligation failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Conflict occurred on obligation_code. Lookup existing record.
	const lookupQuery = `
		SELECT ` + obligationColumns + `
		FROM obligations
		WHERE obligation_code = $1;`

	row = s.pool.QueryRow(ctx, lookupQuery, params.ObligationCode)
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
	query := `SELECT ` + obligationColumns + ` FROM obligations WHERE 1=1`
	var args []any

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
	query += " ORDER BY created_at DESC;"

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
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
	current, err := s.FindObligationByID(ctx, obligationID)
	if err != nil {
		return nil, false, err
	}
	if current.ObligationStatus == newStatus {
		s.log.Debug("obligation status transition idempotent no-op",
			zap.String("obligation_id", obligationID),
			zap.String("status", newStatus),
		)
		return current, false, nil
	}

	allowed := legalTransitions[current.ObligationStatus]
	legal := false
	for _, candidate := range allowed {
		if candidate == newStatus {
			legal = true
			break
		}
	}
	if !legal {
		return nil, false, domain.ErrInvalidTransition
	}

	const query = `
		UPDATE obligations
		SET obligation_status = $1,
		    updated_at = NOW(),
		    closed_at = CASE WHEN $1::VARCHAR = 'CLOSED' THEN NOW() ELSE closed_at END
		WHERE obligation_id = $2
		RETURNING ` + obligationColumns + `;`

	row := s.pool.QueryRow(ctx, query, newStatus, obligationID)
	updated, err := scanObligation(row)
	if err != nil {
		s.log.Error("pg UpdateObligationStatus failed",
			zap.String("obligation_id", obligationID),
			zap.String("new_status", newStatus),
			zap.Error(err),
		)
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return updated, true, nil
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

	const query = `
		INSERT INTO filing_requirements (
			filing_requirement_id, obligation_id, filing_type, filing_authority,
			submission_channel, filing_status
		) VALUES ($1, $2, $3, $4, $5, 'PENDING')
		RETURNING ` + filingRequirementColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.FilingRequirementID, params.ObligationID, params.FilingType,
		params.FilingAuthority, params.SubmissionChannel,
	)

	f, err := scanFilingRequirement(row)
	if err != nil {
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

	const query = `
		SELECT ` + filingRequirementColumns + `
		FROM filing_requirements
		WHERE obligation_id = $1
		ORDER BY created_at DESC;`

	rows, err := s.pool.Query(ctx, query, obligationID)
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
