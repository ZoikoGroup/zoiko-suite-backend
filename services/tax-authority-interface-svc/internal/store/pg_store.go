package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (p *PgStore) CreateInterface(ctx context.Context, tf *domain.TaxInterface) error {
	tenantID := middleware.GetTenantID(ctx)
	if tf.InterfaceID == "" {
		tf.InterfaceID = uuid.New().String()
	}
	now := time.Now().UTC()
	tf.CreatedAt = now
	tf.UpdatedAt = now
	tf.TenantID = tenantID

	query := `
		INSERT INTO tax_interfaces (interface_id, tenant_id, legal_entity_id, jurisdiction, authority_name, protocol, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := p.pool.Exec(ctx, query, tf.InterfaceID, tf.TenantID, tf.LegalEntityID, tf.Jurisdiction, tf.AuthorityName, tf.Protocol, tf.Status, tf.CreatedAt, tf.UpdatedAt)
	return err
}

func (p *PgStore) GetInterfaceByID(ctx context.Context, id string) (*domain.TaxInterface, error) {
	query := `
		SELECT interface_id, tenant_id, legal_entity_id, jurisdiction, authority_name, protocol, status, created_at, updated_at
		FROM tax_interfaces
		WHERE interface_id = $1
	`
	var tf domain.TaxInterface
	err := p.pool.QueryRow(ctx, query, id).Scan(&tf.InterfaceID, &tf.TenantID, &tf.LegalEntityID, &tf.Jurisdiction, &tf.AuthorityName, &tf.Protocol, &tf.Status, &tf.CreatedAt, &tf.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrInterfaceNotFound
		}
		return nil, err
	}
	return &tf, nil
}

func (p *PgStore) ListInterfaces(ctx context.Context, legalEntityID string) ([]domain.TaxInterface, error) {
	query := `
		SELECT interface_id, tenant_id, legal_entity_id, jurisdiction, authority_name, protocol, status, created_at, updated_at
		FROM tax_interfaces
		WHERE ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	rows, err := p.pool.Query(ctx, query, legalEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make([]domain.TaxInterface, 0)
	for rows.Next() {
		var tf domain.TaxInterface
		if err := rows.Scan(&tf.InterfaceID, &tf.TenantID, &tf.LegalEntityID, &tf.Jurisdiction, &tf.AuthorityName, &tf.Protocol, &tf.Status, &tf.CreatedAt, &tf.UpdatedAt); err != nil {
			return nil, err
		}
		res = append(res, tf)
	}
	return res, nil
}

func (p *PgStore) CreateSubmission(ctx context.Context, sub *domain.TaxFilingSubmission) error {
	tenantID := middleware.GetTenantID(ctx)
	if sub.SubmissionID == "" {
		sub.SubmissionID = uuid.New().String()
	}
	sub.SubmittedAt = time.Now().UTC()
	sub.TenantID = tenantID

	query := `
		INSERT INTO tax_filing_submissions (submission_id, interface_id, tenant_id, tax_period, filing_type, tax_amount, status, ack_reference, submitted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := p.pool.Exec(ctx, query, sub.SubmissionID, sub.InterfaceID, sub.TenantID, sub.TaxPeriod, sub.FilingType, sub.TaxAmount, sub.Status, sub.AckReference, sub.SubmittedAt)
	return err
}

func (p *PgStore) GetSubmissionByID(ctx context.Context, id string) (*domain.TaxFilingSubmission, error) {
	query := `
		SELECT submission_id, interface_id, tenant_id, tax_period, filing_type, tax_amount, status, COALESCE(ack_reference, ''), submitted_at
		FROM tax_filing_submissions
		WHERE submission_id = $1
	`
	var sub domain.TaxFilingSubmission
	err := p.pool.QueryRow(ctx, query, id).Scan(&sub.SubmissionID, &sub.InterfaceID, &sub.TenantID, &sub.TaxPeriod, &sub.FilingType, &sub.TaxAmount, &sub.Status, &sub.AckReference, &sub.SubmittedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrFilingNotFound
		}
		return nil, err
	}
	return &sub, nil
}

func (p *PgStore) ListSubmissions(ctx context.Context, interfaceID string) ([]domain.TaxFilingSubmission, error) {
	query := `
		SELECT submission_id, interface_id, tenant_id, tax_period, filing_type, tax_amount, status, COALESCE(ack_reference, ''), submitted_at
		FROM tax_filing_submissions
		WHERE ($1 = '' OR interface_id = $1)
		ORDER BY submitted_at DESC
	`
	rows, err := p.pool.Query(ctx, query, interfaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make([]domain.TaxFilingSubmission, 0)
	for rows.Next() {
		var sub domain.TaxFilingSubmission
		if err := rows.Scan(&sub.SubmissionID, &sub.InterfaceID, &sub.TenantID, &sub.TaxPeriod, &sub.FilingType, &sub.TaxAmount, &sub.Status, &sub.AckReference, &sub.SubmittedAt); err != nil {
			return nil, err
		}
		res = append(res, sub)
	}
	return res, nil
}
