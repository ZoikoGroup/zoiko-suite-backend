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
	CreateException(ctx context.Context, c *domain.ExceptionCase) error
	GetExceptionByID(ctx context.Context, id string) (*domain.ExceptionCase, error)
	ListExceptions(ctx context.Context, legalEntityID, caseStatus, severityLevel, exceptionType string) ([]domain.ExceptionCase, error)
	EscalateException(ctx context.Context, id string, req *domain.EscalateCaseRequest) (*domain.EscalationRecord, *domain.ExceptionCase, error)
	ResolveException(ctx context.Context, id string, req *domain.ResolveCaseRequest) (*domain.ExceptionCase, error)
	ListEscalations(ctx context.Context, role, status string) ([]domain.EscalationRecord, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID)
	return err
}

func (s *PgStore) CreateException(ctx context.Context, c *domain.ExceptionCase) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if c.ExceptionCaseID == "" {
		c.ExceptionCaseID = "excase-" + uuid.New().String()
	}
	c.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if c.CaseStatus == "" {
		c.CaseStatus = domain.CaseOpen
	}
	if c.SeverityLevel == "" {
		c.SeverityLevel = domain.SeverityMedium
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO exception_cases
			(exception_case_id, tenant_id, legal_entity_id, jurisdiction_id, exception_type,
			 severity_level, linked_object_type, linked_object_id, description, case_status,
			 assigned_to_role, assigned_to_user, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		c.ExceptionCaseID, c.TenantID, c.LegalEntityID, c.JurisdictionID, c.ExceptionType,
		string(c.SeverityLevel), c.LinkedObjectType, c.LinkedObjectID, c.Description, string(c.CaseStatus),
		c.AssignedToRole, c.AssignedToUser, c.CreatedBy, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert exception case: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) GetExceptionByID(ctx context.Context, id string) (*domain.ExceptionCase, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var c domain.ExceptionCase
	var sevStr, statStr string
	err = tx.QueryRow(ctx, `
		SELECT exception_case_id, tenant_id, legal_entity_id, jurisdiction_id, exception_type,
		       severity_level, linked_object_type, linked_object_id, description, case_status,
		       assigned_to_role, assigned_to_user, escalated_at, closed_at, closed_by,
		       closure_reason, created_by, created_at, updated_at
		FROM exception_cases WHERE exception_case_id = $1`, id,
	).Scan(
		&c.ExceptionCaseID, &c.TenantID, &c.LegalEntityID, &c.JurisdictionID, &c.ExceptionType,
		&sevStr, &c.LinkedObjectType, &c.LinkedObjectID, &c.Description, &statStr,
		&c.AssignedToRole, &c.AssignedToUser, &c.EscalatedAt, &c.ClosedAt, &c.ClosedBy,
		&c.ClosureReason, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrExceptionCaseNotFound
		}
		return nil, err
	}
	c.SeverityLevel = domain.SeverityLevel(sevStr)
	c.CaseStatus = domain.CaseStatus(statStr)

	// Fetch escalations
	rows, err := tx.Query(ctx, `
		SELECT escalation_record_id, tenant_id, exception_case_id, escalated_to_role,
		       escalated_to_user, escalation_reason, escalation_status, escalated_by,
		       escalated_at, resolved_at, response_notes, created_at, updated_at
		FROM escalation_records WHERE exception_case_id = $1 ORDER BY escalated_at ASC`, id,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var e domain.EscalationRecord
			var escStatStr string
			if err := rows.Scan(
				&e.EscalationRecordID, &e.TenantID, &e.ExceptionCaseID, &e.EscalatedToRole,
				&e.EscalatedToUser, &e.EscalationReason, &escStatStr, &e.EscalatedBy,
				&e.EscalatedAt, &e.ResolvedAt, &e.ResponseNotes, &e.CreatedAt, &e.UpdatedAt,
			); err == nil {
				e.EscalationStatus = domain.EscalationStatus(escStatStr)
				c.Escalations = append(c.Escalations, e)
			}
		}
	}

	_ = tx.Commit(ctx)
	return &c, nil
}

func (s *PgStore) ListExceptions(ctx context.Context, legalEntityID, caseStatus, severityLevel, exceptionType string) ([]domain.ExceptionCase, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT exception_case_id, tenant_id, legal_entity_id, jurisdiction_id, exception_type,
		       severity_level, linked_object_type, linked_object_id, description, case_status,
		       assigned_to_role, assigned_to_user, escalated_at, closed_at, closed_by,
		       closure_reason, created_by, created_at, updated_at
		FROM exception_cases
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR case_status = $2)
		  AND ($3 = '' OR severity_level = $3)
		  AND ($4 = '' OR exception_type = $4)
		ORDER BY created_at DESC`,
		legalEntityID, caseStatus, severityLevel, exceptionType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ExceptionCase
	for rows.Next() {
		var c domain.ExceptionCase
		var sevStr, statStr string
		if err := rows.Scan(
			&c.ExceptionCaseID, &c.TenantID, &c.LegalEntityID, &c.JurisdictionID, &c.ExceptionType,
			&sevStr, &c.LinkedObjectType, &c.LinkedObjectID, &c.Description, &statStr,
			&c.AssignedToRole, &c.AssignedToUser, &c.EscalatedAt, &c.ClosedAt, &c.ClosedBy,
			&c.ClosureReason, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		c.SeverityLevel = domain.SeverityLevel(sevStr)
		c.CaseStatus = domain.CaseStatus(statStr)
		out = append(out, c)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) EscalateException(ctx context.Context, id string, req *domain.EscalateCaseRequest) (*domain.EscalationRecord, *domain.ExceptionCase, error) {
	c, err := s.GetExceptionByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if c.CaseStatus == domain.CaseClosed || c.CaseStatus == domain.CaseResolved {
		return nil, nil, domain.ErrCaseAlreadyClosed
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	escRecord := domain.EscalationRecord{
		EscalationRecordID: "escrec-" + uuid.New().String(),
		TenantID:           middleware.GetTenantID(ctx),
		ExceptionCaseID:    id,
		EscalatedToRole:    req.EscalatedToRole,
		EscalatedToUser:    req.EscalatedToUser,
		EscalationReason:   req.EscalationReason,
		EscalationStatus:   domain.EscalationPending,
		EscalatedBy:        req.EscalatedBy,
		EscalatedAt:        now,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO escalation_records
			(escalation_record_id, tenant_id, exception_case_id, escalated_to_role,
			 escalated_to_user, escalation_reason, escalation_status, escalated_by,
			 escalated_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		escRecord.EscalationRecordID, escRecord.TenantID, escRecord.ExceptionCaseID,
		escRecord.EscalatedToRole, escRecord.EscalatedToUser, escRecord.EscalationReason,
		string(escRecord.EscalationStatus), escRecord.EscalatedBy, escRecord.EscalatedAt,
		escRecord.CreatedAt, escRecord.UpdatedAt,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("insert escalation record: %w", err)
	}

	c.CaseStatus = domain.CaseEscalated
	c.EscalatedAt = &now
	c.AssignedToRole = req.EscalatedToRole
	if req.EscalatedToUser != "" {
		c.AssignedToUser = req.EscalatedToUser
	}
	c.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE exception_cases
		SET case_status=$1, escalated_at=$2, assigned_to_role=$3, assigned_to_user=$4, updated_at=$5
		WHERE exception_case_id=$6`,
		string(c.CaseStatus), c.EscalatedAt, c.AssignedToRole, c.AssignedToUser, c.UpdatedAt, id,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("update exception case on escalation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return &escRecord, c, nil
}

func (s *PgStore) ResolveException(ctx context.Context, id string, req *domain.ResolveCaseRequest) (*domain.ExceptionCase, error) {
	c, err := s.GetExceptionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.CaseStatus == domain.CaseClosed || c.CaseStatus == domain.CaseResolved {
		return nil, domain.ErrCaseAlreadyClosed
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	c.CaseStatus = domain.CaseClosed
	c.ClosedAt = &now
	c.ClosedBy = req.ClosedBy
	c.ClosureReason = req.ClosureReason
	c.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE exception_cases
		SET case_status=$1, closed_at=$2, closed_by=$3, closure_reason=$4, updated_at=$5
		WHERE exception_case_id=$6`,
		string(c.CaseStatus), c.ClosedAt, c.ClosedBy, c.ClosureReason, c.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve exception case: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *PgStore) ListEscalations(ctx context.Context, role, status string) ([]domain.EscalationRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT escalation_record_id, tenant_id, exception_case_id, escalated_to_role,
		       escalated_to_user, escalation_reason, escalation_status, escalated_by,
		       escalated_at, resolved_at, response_notes, created_at, updated_at
		FROM escalation_records
		WHERE ($1 = '' OR escalated_to_role = $1)
		  AND ($2 = '' OR escalation_status = $2)
		ORDER BY escalated_at DESC`,
		role, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.EscalationRecord
	for rows.Next() {
		var e domain.EscalationRecord
		var statStr string
		if err := rows.Scan(
			&e.EscalationRecordID, &e.TenantID, &e.ExceptionCaseID, &e.EscalatedToRole,
			&e.EscalatedToUser, &e.EscalationReason, &statStr, &e.EscalatedBy,
			&e.EscalatedAt, &e.ResolvedAt, &e.ResponseNotes, &e.CreatedAt, &e.UpdatedAt,
		); err != nil {
			return nil, err
		}
		e.EscalationStatus = domain.EscalationStatus(statStr)
		out = append(out, e)
	}
	_ = tx.Commit(ctx)
	return out, nil
}
