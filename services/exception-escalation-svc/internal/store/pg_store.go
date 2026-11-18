package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/middleware"
)

type Store interface {
	CreateExceptionCase(ctx context.Context, params domain.CreateExceptionCaseParams) (*domain.ExceptionCase, error)
	FindExceptionCaseByID(ctx context.Context, exceptionCaseID string) (*domain.ExceptionCase, error)
	ListExceptionCases(ctx context.Context, filter domain.ListExceptionCasesFilter) ([]*domain.ExceptionCase, error)
	UpdateExceptionCaseStatus(ctx context.Context, exceptionCaseID string, params domain.UpdateExceptionCaseStatusParams, actorID, correlationID string) (*domain.ExceptionCase, error)

	CreateEscalationRecord(ctx context.Context, params domain.CreateEscalationRecordParams) (*domain.EscalationRecord, error)
	FindEscalationRecordByID(ctx context.Context, escalationRecordID string) (*domain.EscalationRecord, error)
	ListEscalationRecords(ctx context.Context, filter domain.ListEscalationRecordsFilter) ([]*domain.EscalationRecord, error)
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
	exceptionCaseID := params.ExceptionCaseID
	if exceptionCaseID == "" {
		exceptionCaseID = uuid.New().String()
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO exception_cases (
			exception_case_id, tenant_id, legal_entity_id, exception_type,
			severity_level, linked_object_type, linked_object_id, case_status,
			created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 'OPEN', $8)`,
		exceptionCaseID, params.TenantID, params.LegalEntityID, params.ExceptionType,
		params.SeverityLevel, params.LinkedObjectType, params.LinkedObjectID,
		params.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, fmt.Errorf("insert exception case: %w", err)
	}

	return s.FindExceptionCaseByID(ctx, exceptionCaseID)
}

func (s *PgStore) FindExceptionCaseByID(ctx context.Context, exceptionCaseID string) (*domain.ExceptionCase, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var ec *domain.ExceptionCase
	err = tx.QueryRow(ctx, `
		SELECT `+exceptionCaseColumns+`
		FROM exception_cases WHERE exception_case_id = $1 AND tenant_id = $2`,
		exceptionCaseID, middleware.GetTenantID(ctx),
	).Scan(
		&ec.ExceptionCaseID, &ec.TenantID, &ec.LegalEntityID, &ec.ExceptionType,
		&ec.SeverityLevel, &ec.LinkedObjectType, &ec.LinkedObjectID, &ec.CaseStatus,
		&ec.EscalatedAt, &ec.CreatedAt, &ec.CreatedByPrincipalID, &ec.UpdatedAt, &ec.Version,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrExceptionCaseNotFound
		}
		return nil, err
	}
	_ = tx.Commit(ctx)
	return ec, nil
}

func (s *PgStore) ListExceptionCases(ctx context.Context, filter domain.ListExceptionCasesFilter) ([]*domain.ExceptionCase, error) {
	tenantID := middleware.GetTenantID(ctx)
	if filter.TenantID != "" && filter.TenantID != tenantID {
		return nil, domain.ErrTenantMissing
	}

	query := `SELECT ` + exceptionCaseColumns + ` FROM exception_cases WHERE tenant_id = $1`
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
	addFilter("exception_type", string(filter.ExceptionType))
	addFilter("case_status", string(filter.CaseStatus))
	addFilter("severity_level", string(filter.SeverityLevel))

	query += " ORDER BY created_at DESC, exception_case_id DESC"

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

	var results []*domain.ExceptionCase
	for rows.Next() {
		ec, err := scanExceptionCase(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, ec)
	}
	return results, rows.Err()
}

func (s *PgStore) UpdateExceptionCaseStatus(ctx context.Context, exceptionCaseID string, params domain.UpdateExceptionCaseStatusParams, actorID, correlationID string) (*domain.ExceptionCase, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var currentVersion int
	var escalatedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT version, escalated_at FROM exception_cases
		WHERE exception_case_id = $1 AND tenant_id = $2 FOR UPDATE`,
		exceptionCaseID, middleware.GetTenantID(ctx),
	).Scan(&currentVersion, &escalatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrExceptionCaseNotFound
		}
		return nil, err
	}

	if params.ExpectedVersion > 0 && currentVersion != params.ExpectedVersion {
		return nil, domain.ErrVersionMismatch
	}

	newEscalatedAt := escalatedAt
	if params.CaseStatus == domain.CaseStatusEscalated && escalatedAt == nil {
		now := time.Now().UTC()
		newEscalatedAt = &now
	}

	_, err = tx.Exec(ctx, `
		UPDATE exception_cases
		SET case_status = $1,
		    escalated_at = $2,
		    updated_at = NOW(),
		    version = version + 1
		WHERE exception_case_id = $3 AND tenant_id = $4`,
		params.CaseStatus, newEscalatedAt, exceptionCaseID, middleware.GetTenantID(ctx),
	)
	if err != nil {
		return nil, fmt.Errorf("update exception case status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return s.FindExceptionCaseByID(ctx, exceptionCaseID)
}

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
	escalationRecordID := params.EscalationRecordID
	if escalationRecordID == "" {
		escalationRecordID = uuid.New().String()
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO escalation_records (
			escalation_record_id, exception_case_id, escalated_to_role, escalation_reason,
			escalation_status, escalated_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, 'PENDING', NOW(), $5)`,
		escalationRecordID, params.ExceptionCaseID, params.EscalatedToRole,
		params.EscalationReason, params.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, fmt.Errorf("insert escalation record: %w", err)
	}

	return s.FindEscalationRecordByID(ctx, escalationRecordID)
}

func (s *PgStore) FindEscalationRecordByID(ctx context.Context, escalationRecordID string) (*domain.EscalationRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var er *domain.EscalationRecord
	err = tx.QueryRow(ctx, `
		SELECT `+escalationRecordColumns+`
		FROM escalation_records WHERE escalation_record_id = $1 AND tenant_id = $2`,
		escalationRecordID, middleware.GetTenantID(ctx),
	).Scan(
		&er.EscalationRecordID, &er.ExceptionCaseID, &er.EscalatedToRole,
		&er.EscalationReason, &er.EscalationStatus, &er.EscalatedAt,
		&er.CreatedAt, &er.CreatedByPrincipalID,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrEscalationRecordNotFound
		}
		return nil, err
	}
	_ = tx.Commit(ctx)
	return er, nil
}

func (s *PgStore) ListEscalationRecords(ctx context.Context, filter domain.ListEscalationRecordsFilter) ([]*domain.EscalationRecord, error) {
	tenantID := middleware.GetTenantID(ctx)

	query := `SELECT ` + escalationRecordColumns + ` FROM escalation_records WHERE tenant_id = $1`
	args := []any{tenantID}
	argIdx := 1

	addFilter := func(col, val string) {
		if val != "" {
			argIdx++
			query += fmt.Sprintf(" AND %s = $%d", col, argIdx)
			args = append(args, val)
		}
	}

	addFilter("exception_case_id", filter.ExceptionCaseID)
	addFilter("escalated_to_role", filter.EscalatedToRole)
	addFilter("escalation_status", string(filter.EscalationStatus))

	query += " ORDER BY escalated_at DESC, escalation_record_id DESC"

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

	var results []*domain.EscalationRecord
	for rows.Next() {
		er, err := scanEscalationRecord(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, er)
	}
	return results, rows.Err()
}