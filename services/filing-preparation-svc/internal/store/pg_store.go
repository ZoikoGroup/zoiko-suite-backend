package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/filing-preparation-svc/internal/domain"
	"zoiko.io/filing-preparation-svc/internal/middleware"
)

type Store interface {
	Create(ctx context.Context, d *domain.FilingDraft) error
	GetByID(ctx context.Context, id string) (*domain.FilingDraft, error)
	List(ctx context.Context, legalEntityID, jurisdictionID, filingType, status string) ([]domain.FilingDraft, error)
	Update(ctx context.Context, d *domain.FilingDraft) error
	Validate(ctx context.Context, id string, req *domain.ValidateDraftRequest) (*domain.FilingDraft, error)
	Finalize(ctx context.Context, id string, req *domain.FinalizeDraftRequest) (*domain.FilingDraft, error)
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

func (s *PgStore) Create(ctx context.Context, d *domain.FilingDraft) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if d.DraftID == "" {
		d.DraftID = "fprep-" + uuid.New().String()
	}
	d.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	d.CreatedAt = now
	d.UpdatedAt = now
	if d.ValidationStatus == "" {
		d.ValidationStatus = domain.StatusDraft
	}
	if d.FilingType == "" {
		d.FilingType = "VAT"
	}
	if d.PayloadData == "" {
		d.PayloadData = "{}"
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO filing_drafts
			(draft_id, tenant_id, legal_entity_id, jurisdiction_id, filing_type,
			 period_key, due_date, payload_data, evidence_manifest_ref,
			 validation_status, block_reasons, notes, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		d.DraftID, d.TenantID, d.LegalEntityID, d.JurisdictionID, d.FilingType,
		d.PeriodKey, d.DueDate, d.PayloadData, d.EvidenceManifestRef,
		string(d.ValidationStatus), d.BlockReasons, d.Notes, d.CreatedBy, d.CreatedAt, d.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert filing draft: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) GetByID(ctx context.Context, id string) (*domain.FilingDraft, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var d domain.FilingDraft
	var statusStr string
	err = tx.QueryRow(ctx, `
		SELECT draft_id, tenant_id, legal_entity_id, jurisdiction_id, filing_type,
		       period_key, due_date, payload_data, evidence_manifest_ref,
		       validation_status, block_reasons, notes, created_by, created_at, updated_at
		FROM filing_drafts WHERE draft_id = $1`, id,
	).Scan(
		&d.DraftID, &d.TenantID, &d.LegalEntityID, &d.JurisdictionID, &d.FilingType,
		&d.PeriodKey, &d.DueDate, &d.PayloadData, &d.EvidenceManifestRef,
		&statusStr, &d.BlockReasons, &d.Notes, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrDraftNotFound
		}
		return nil, err
	}
	d.ValidationStatus = domain.ValidationStatus(statusStr)
	_ = tx.Commit(ctx)
	return &d, nil
}

func (s *PgStore) List(ctx context.Context, legalEntityID, jurisdictionID, filingType, status string) ([]domain.FilingDraft, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT draft_id, tenant_id, legal_entity_id, jurisdiction_id, filing_type,
		       period_key, due_date, payload_data, evidence_manifest_ref,
		       validation_status, block_reasons, notes, created_by, created_at, updated_at
		FROM filing_drafts
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR jurisdiction_id = $2)
		  AND ($3 = '' OR filing_type = $3)
		  AND ($4 = '' OR validation_status = $4)
		ORDER BY due_date ASC, created_at DESC`,
		legalEntityID, jurisdictionID, filingType, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.FilingDraft
	for rows.Next() {
		var d domain.FilingDraft
		var statusStr string
		if err := rows.Scan(
			&d.DraftID, &d.TenantID, &d.LegalEntityID, &d.JurisdictionID, &d.FilingType,
			&d.PeriodKey, &d.DueDate, &d.PayloadData, &d.EvidenceManifestRef,
			&statusStr, &d.BlockReasons, &d.Notes, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			return nil, err
		}
		d.ValidationStatus = domain.ValidationStatus(statusStr)
		out = append(out, d)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) Update(ctx context.Context, d *domain.FilingDraft) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}
	d.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE filing_drafts
		SET payload_data=$1, evidence_manifest_ref=$2, validation_status=$3,
		    block_reasons=$4, notes=$5, updated_at=$6
		WHERE draft_id=$7`,
		d.PayloadData, d.EvidenceManifestRef, string(d.ValidationStatus),
		d.BlockReasons, d.Notes, d.UpdatedAt, d.DraftID,
	)
	if err != nil {
		return fmt.Errorf("update filing draft: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) Validate(ctx context.Context, id string, req *domain.ValidateDraftRequest) (*domain.FilingDraft, error) {
	d, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.ValidationStatus == domain.StatusReadyForSubmission {
		return nil, domain.ErrDraftAlreadyFinal
	}

	d.ValidateEvidence(req.RequiredDocumentTypes)
	d.UpdatedAt = time.Now().UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx, `
		UPDATE filing_drafts
		SET validation_status=$1, block_reasons=$2, updated_at=$3
		WHERE draft_id=$4`,
		string(d.ValidationStatus), d.BlockReasons, d.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("validate filing draft: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *PgStore) Finalize(ctx context.Context, id string, req *domain.FinalizeDraftRequest) (*domain.FilingDraft, error) {
	d, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.ValidationStatus == domain.StatusBlocked {
		return nil, domain.ErrValidationBlocked
	}
	if d.ValidationStatus == domain.StatusReadyForSubmission {
		return nil, domain.ErrDraftAlreadyFinal
	}

	d.ValidationStatus = domain.StatusReadyForSubmission
	if req.Notes != "" {
		d.Notes = req.Notes
	}
	d.UpdatedAt = time.Now().UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx, `
		UPDATE filing_drafts
		SET validation_status=$1, notes=$2, updated_at=$3
		WHERE draft_id=$4`,
		string(d.ValidationStatus), d.Notes, d.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("finalize filing draft: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}
