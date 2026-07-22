package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/filing-tracker-svc/internal/domain"
	"zoiko.io/filing-tracker-svc/internal/middleware"
)

type Store interface {
	Create(ctx context.Context, f *domain.FilingRequirement) error
	GetByID(ctx context.Context, id string) (*domain.FilingRequirement, error)
	List(ctx context.Context, legalEntityID, jurisdictionID, filingAuthority, status string) ([]domain.FilingRequirement, error)
	Update(ctx context.Context, f *domain.FilingRequirement) error
	Submit(ctx context.Context, id string, req *domain.SubmitFilingRequest) (*domain.FilingRequirement, error)
	Confirm(ctx context.Context, id string, req *domain.ConfirmFilingRequest) (*domain.FilingRequirement, error)
	MarkOverdue(ctx context.Context, id, todayStr string) (*domain.FilingRequirement, error)
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

func (s *PgStore) Create(ctx context.Context, f *domain.FilingRequirement) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if f.FilingID == "" {
		f.FilingID = "ftrk-" + uuid.New().String()
	}
	f.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	f.CreatedAt = now
	f.UpdatedAt = now
	if f.Status == "" {
		f.Status = domain.StatusScheduled
	}
	if f.FilingType == "" {
		f.FilingType = "VAT"
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO filing_requirements
			(filing_id, tenant_id, legal_entity_id, jurisdiction_id, filing_authority,
			 filing_type, period_key, due_date, status, rejection_reason,
			 notes, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		f.FilingID, f.TenantID, f.LegalEntityID, f.JurisdictionID, f.FilingAuthority,
		f.FilingType, f.PeriodKey, f.DueDate, string(f.Status), f.RejectionReason,
		f.Notes, f.CreatedBy, f.CreatedAt, f.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert filing requirement: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) GetByID(ctx context.Context, id string) (*domain.FilingRequirement, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var f domain.FilingRequirement
	var statusStr string
	err = tx.QueryRow(ctx, `
		SELECT filing_id, tenant_id, legal_entity_id, jurisdiction_id, filing_authority,
		       filing_type, period_key, due_date, status, submission_reference,
		       submitted_at, submitted_by, confirmation_reference, confirmed_at,
		       rejection_reason, notes, created_by, created_at, updated_at
		FROM filing_requirements WHERE filing_id = $1`, id,
	).Scan(
		&f.FilingID, &f.TenantID, &f.LegalEntityID, &f.JurisdictionID, &f.FilingAuthority,
		&f.FilingType, &f.PeriodKey, &f.DueDate, &statusStr, &f.SubmissionReference,
		&f.SubmittedAt, &f.SubmittedBy, &f.ConfirmationReference, &f.ConfirmedAt,
		&f.RejectionReason, &f.Notes, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrRequirementNotFound
		}
		return nil, err
	}
	f.Status = domain.FilingStatus(statusStr)
	_ = tx.Commit(ctx)
	return &f, nil
}

func (s *PgStore) List(ctx context.Context, legalEntityID, jurisdictionID, filingAuthority, status string) ([]domain.FilingRequirement, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT filing_id, tenant_id, legal_entity_id, jurisdiction_id, filing_authority,
		       filing_type, period_key, due_date, status, submission_reference,
		       submitted_at, submitted_by, confirmation_reference, confirmed_at,
		       rejection_reason, notes, created_by, created_at, updated_at
		FROM filing_requirements
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR jurisdiction_id = $2)
		  AND ($3 = '' OR filing_authority = $3)
		  AND ($4 = '' OR status = $4)
		ORDER BY due_date ASC, created_at DESC`,
		legalEntityID, jurisdictionID, filingAuthority, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.FilingRequirement
	for rows.Next() {
		var f domain.FilingRequirement
		var statusStr string
		if err := rows.Scan(
			&f.FilingID, &f.TenantID, &f.LegalEntityID, &f.JurisdictionID, &f.FilingAuthority,
			&f.FilingType, &f.PeriodKey, &f.DueDate, &statusStr, &f.SubmissionReference,
			&f.SubmittedAt, &f.SubmittedBy, &f.ConfirmationReference, &f.ConfirmedAt,
			&f.RejectionReason, &f.Notes, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt,
		); err != nil {
			return nil, err
		}
		f.Status = domain.FilingStatus(statusStr)
		out = append(out, f)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) Update(ctx context.Context, f *domain.FilingRequirement) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}
	f.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE filing_requirements
		SET status=$1, notes=$2, updated_at=$3
		WHERE filing_id=$4`,
		string(f.Status), f.Notes, f.UpdatedAt, f.FilingID,
	)
	if err != nil {
		return fmt.Errorf("update filing requirement: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) Submit(ctx context.Context, id string, req *domain.SubmitFilingRequest) (*domain.FilingRequirement, error) {
	f, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if f.Status == domain.StatusSubmitted || f.Status == domain.StatusConfirmed {
		return nil, domain.ErrAlreadySubmitted
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
	f.Status = domain.StatusSubmitted
	f.SubmissionReference = &req.SubmissionReference
	f.SubmittedAt = &now
	f.SubmittedBy = &req.SubmittedBy
	if req.Notes != "" {
		f.Notes = req.Notes
	}
	f.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE filing_requirements
		SET status=$1, submission_reference=$2, submitted_at=$3, submitted_by=$4, notes=$5, updated_at=$6
		WHERE filing_id=$7`,
		string(f.Status), f.SubmissionReference, f.SubmittedAt, f.SubmittedBy, f.Notes, f.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("submit filing requirement: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *PgStore) Confirm(ctx context.Context, id string, req *domain.ConfirmFilingRequest) (*domain.FilingRequirement, error) {
	f, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if f.Status == domain.StatusConfirmed {
		return nil, domain.ErrAlreadyConfirmed
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
	f.Status = domain.StatusConfirmed
	f.ConfirmationReference = &req.ConfirmationReference
	f.ConfirmedAt = &now
	if req.Notes != "" {
		f.Notes = req.Notes
	}
	f.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE filing_requirements
		SET status=$1, confirmation_reference=$2, confirmed_at=$3, notes=$4, updated_at=$5
		WHERE filing_id=$6`,
		string(f.Status), f.ConfirmationReference, f.ConfirmedAt, f.Notes, f.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("confirm filing requirement: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *PgStore) MarkOverdue(ctx context.Context, id, todayStr string) (*domain.FilingRequirement, error) {
	f, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if f.Status == domain.StatusOverdue {
		return nil, domain.ErrAlreadyOverdue
	}
	if f.Status == domain.StatusSubmitted || f.Status == domain.StatusConfirmed {
		return f, nil
	}

	f.CheckOverdue(todayStr)
	f.UpdatedAt = time.Now().UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx, `
		UPDATE filing_requirements
		SET status=$1, updated_at=$2
		WHERE filing_id=$3`,
		string(f.Status), f.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("mark filing overdue: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return f, nil
}
