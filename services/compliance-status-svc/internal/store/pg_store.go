package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/compliance-status-svc/internal/domain"
	"zoiko.io/compliance-status-svc/internal/middleware"
)

type Store interface {
	CreateComplianceStatus(ctx context.Context, params domain.CreateComplianceStatusParams) (*domain.ComplianceStatus, error)
	FindComplianceStatusByID(ctx context.Context, complianceStatusID string) (*domain.ComplianceStatus, error)
	ListComplianceStatuses(ctx context.Context, filter domain.ListComplianceStatusFilter) ([]*domain.ComplianceStatus, error)
	UpdateComplianceStatus(ctx context.Context, complianceStatusID string, params domain.UpdateComplianceStatusParams, actorID, correlationID string) (*domain.ComplianceStatus, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	if tenantID == "" {
		return domain.ErrTenantMissing
	}
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}

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
	complianceStatusID := params.ComplianceStatusID
	if complianceStatusID == "" {
		complianceStatusID = uuid.New().String()
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO compliance_status (
			compliance_status_id, tenant_id, legal_entity_id, domain_code, status_code,
			status_reason, evaluated_at, evidence_sufficiency_state,
			created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		complianceStatusID, params.TenantID, params.LegalEntityID, params.DomainCode,
		params.StatusCode, params.StatusReason, params.EvaluatedAt,
		params.EvidenceSufficiencyState, params.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, fmt.Errorf("insert compliance status: %w", err)
	}

	return s.FindComplianceStatusByID(ctx, complianceStatusID)
}

func (s *PgStore) FindComplianceStatusByID(ctx context.Context, complianceStatusID string) (*domain.ComplianceStatus, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var cs *domain.ComplianceStatus
	err = tx.QueryRow(ctx, `
		SELECT `+complianceStatusColumns+`
		FROM compliance_status WHERE compliance_status_id = $1 AND tenant_id = $2`,
		complianceStatusID, middleware.GetTenantID(ctx),
	).Scan(
		&cs.ComplianceStatusID, &cs.TenantID, &cs.LegalEntityID, &cs.DomainCode,
		&cs.StatusCode, &cs.StatusReason, &cs.EvaluatedAt, &cs.EvidenceSufficiencyState,
		&cs.CreatedAt, &cs.CreatedByPrincipalID, &cs.UpdatedAt, &cs.Version,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrComplianceStatusNotFound
		}
		return nil, err
	}
	_ = tx.Commit(ctx)
	return cs, nil
}

func (s *PgStore) ListComplianceStatuses(ctx context.Context, filter domain.ListComplianceStatusFilter) ([]*domain.ComplianceStatus, error) {
	tenantID := middleware.GetTenantID(ctx)
	if filter.TenantID != "" && filter.TenantID != tenantID {
		return nil, domain.ErrTenantMissing
	}

	query := `SELECT ` + complianceStatusColumns + ` FROM compliance_status WHERE tenant_id = $1`
	args := []any{tenantID}
	argIdx := 1

	addFilter := func(col, val string) {
		if val != "" {
			argIdx++
			query += fmt.Sprintf(" AND %s = $%d", col, argIdx)
			args = append(args, val)
		}
	}

	addFilter("legal_entity_id", filter.LegalEntityID)
	addFilter("domain_code", string(filter.DomainCode))
	addFilter("status_code", string(filter.StatusCode))

	query += " ORDER BY evaluated_at DESC, compliance_status_id DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	argIdx++
	query += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)

	argIdx++
	query += fmt.Sprintf(" OFFSET $%d", argIdx)
	args = append(args, filter.Offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*domain.ComplianceStatus
	for rows.Next() {
		cs, err := scanComplianceStatus(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, cs)
	}
	return results, rows.Err()
}

func (s *PgStore) UpdateComplianceStatus(ctx context.Context, complianceStatusID string, params domain.UpdateComplianceStatusParams, actorID, correlationID string) (*domain.ComplianceStatus, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	// Check current version for optimistic locking
	var currentVersion int
	err = tx.QueryRow(ctx, `
		SELECT version FROM compliance_status
		WHERE compliance_status_id = $1 AND tenant_id = $2 FOR UPDATE`,
		complianceStatusID, middleware.GetTenantID(ctx),
	).Scan(&currentVersion)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrComplianceStatusNotFound
		}
		return nil, err
	}

	if params.ExpectedVersion > 0 && currentVersion != params.ExpectedVersion {
		return nil, fmt.Errorf("version mismatch: expected %d, got %d", params.ExpectedVersion, currentVersion)
	}

	_, err = tx.Exec(ctx, `
		UPDATE compliance_status
		SET status_code = $1,
		    status_reason = $2,
		    evidence_sufficiency_state = $3,
		    updated_at = NOW(),
		    version = version + 1
		WHERE compliance_status_id = $4 AND tenant_id = $5`,
		params.StatusCode, params.StatusReason, params.EvidenceSufficiencyState,
		complianceStatusID, middleware.GetTenantID(ctx),
	)
	if err != nil {
		return nil, fmt.Errorf("update compliance status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return s.FindComplianceStatusByID(ctx, complianceStatusID)
}