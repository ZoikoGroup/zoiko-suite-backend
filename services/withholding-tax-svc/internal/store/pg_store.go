package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/withholding-tax-svc/internal/domain"
	"zoiko.io/withholding-tax-svc/internal/middleware"
)

type Store interface {
	Create(ctx context.Context, o *domain.WithholdingTaxObligation) error
	GetByID(ctx context.Context, id string) (*domain.WithholdingTaxObligation, error)
	List(ctx context.Context, legalEntityID, jurisdictionID, counterpartyID, status string) ([]domain.WithholdingTaxObligation, error)
	Update(ctx context.Context, o *domain.WithholdingTaxObligation) error
	Remit(ctx context.Context, id string, req *domain.RemitObligationRequest) (*domain.WithholdingTaxObligation, error)
	Cancel(ctx context.Context, id string, req *domain.CancelObligationRequest) (*domain.WithholdingTaxObligation, error)
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

func (s *PgStore) Create(ctx context.Context, o *domain.WithholdingTaxObligation) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if o.ObligationID == "" {
		o.ObligationID = "whto-" + uuid.New().String()
	}
	o.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	o.CreatedAt = now
	o.UpdatedAt = now
	if o.Status == "" {
		o.Status = domain.StatusPendingRemittance
	}
	if o.Currency == "" {
		o.Currency = "USD"
	}
	if o.PaymentType == "" {
		o.PaymentType = "SERVICES"
	}
	o.Compute()

	_, err = tx.Exec(ctx, `
		INSERT INTO withholding_tax_obligations
			(obligation_id, tenant_id, legal_entity_id, jurisdiction_id, counterparty_id,
			 payment_reference, payment_type, gross_payment_amount, taxable_base_amount,
			 withholding_rate_percent, withheld_amount, currency, tax_rule_id,
			 tax_treaty_exemption, exemption_certificate_ref, status, notes,
			 effective_from, effective_to, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		o.ObligationID, o.TenantID, o.LegalEntityID, o.JurisdictionID, o.CounterpartyID,
		o.PaymentReference, o.PaymentType, o.GrossPaymentAmount, o.TaxableBaseAmount,
		o.WithholdingRatePercent, o.WithheldAmount, o.Currency, o.TaxRuleID,
		o.TaxTreatyExemption, o.ExemptionCertificateRef, string(o.Status), o.Notes,
		o.EffectiveFrom, o.EffectiveTo, o.CreatedBy, o.CreatedAt, o.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert withholding tax obligation: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) GetByID(ctx context.Context, id string) (*domain.WithholdingTaxObligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var o domain.WithholdingTaxObligation
	var statusStr string
	err = tx.QueryRow(ctx, `
		SELECT obligation_id, tenant_id, legal_entity_id, jurisdiction_id, counterparty_id,
		       payment_reference, payment_type, gross_payment_amount, taxable_base_amount,
		       withholding_rate_percent, withheld_amount, currency, tax_rule_id,
		       tax_treaty_exemption, exemption_certificate_ref, status, remittance_reference,
		       remitted_at, remitted_by, notes, effective_from, effective_to,
		       created_by, created_at, updated_at
		FROM withholding_tax_obligations WHERE obligation_id = $1`, id,
	).Scan(
		&o.ObligationID, &o.TenantID, &o.LegalEntityID, &o.JurisdictionID, &o.CounterpartyID,
		&o.PaymentReference, &o.PaymentType, &o.GrossPaymentAmount, &o.TaxableBaseAmount,
		&o.WithholdingRatePercent, &o.WithheldAmount, &o.Currency, &o.TaxRuleID,
		&o.TaxTreatyExemption, &o.ExemptionCertificateRef, &statusStr, &o.RemittanceReference,
		&o.RemittedAt, &o.RemittedBy, &o.Notes, &o.EffectiveFrom, &o.EffectiveTo,
		&o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrObligationNotFound
		}
		return nil, err
	}
	o.Status = domain.ObligationStatus(statusStr)
	_ = tx.Commit(ctx)
	return &o, nil
}

func (s *PgStore) List(ctx context.Context, legalEntityID, jurisdictionID, counterpartyID, status string) ([]domain.WithholdingTaxObligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT obligation_id, tenant_id, legal_entity_id, jurisdiction_id, counterparty_id,
		       payment_reference, payment_type, gross_payment_amount, taxable_base_amount,
		       withholding_rate_percent, withheld_amount, currency, tax_rule_id,
		       tax_treaty_exemption, exemption_certificate_ref, status, remittance_reference,
		       remitted_at, remitted_by, notes, effective_from, effective_to,
		       created_by, created_at, updated_at
		FROM withholding_tax_obligations
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR jurisdiction_id = $2)
		  AND ($3 = '' OR counterparty_id = $3)
		  AND ($4 = '' OR status = $4)
		ORDER BY created_at DESC`,
		legalEntityID, jurisdictionID, counterpartyID, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.WithholdingTaxObligation
	for rows.Next() {
		var o domain.WithholdingTaxObligation
		var statusStr string
		if err := rows.Scan(
			&o.ObligationID, &o.TenantID, &o.LegalEntityID, &o.JurisdictionID, &o.CounterpartyID,
			&o.PaymentReference, &o.PaymentType, &o.GrossPaymentAmount, &o.TaxableBaseAmount,
			&o.WithholdingRatePercent, &o.WithheldAmount, &o.Currency, &o.TaxRuleID,
			&o.TaxTreatyExemption, &o.ExemptionCertificateRef, &statusStr, &o.RemittanceReference,
			&o.RemittedAt, &o.RemittedBy, &o.Notes, &o.EffectiveFrom, &o.EffectiveTo,
			&o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, err
		}
		o.Status = domain.ObligationStatus(statusStr)
		out = append(out, o)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) Update(ctx context.Context, o *domain.WithholdingTaxObligation) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}
	o.Compute()
	o.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE withholding_tax_obligations
		SET gross_payment_amount=$1, taxable_base_amount=$2, withholding_rate_percent=$3,
		    withheld_amount=$4, tax_treaty_exemption=$5, exemption_certificate_ref=$6,
		    notes=$7, effective_to=$8, updated_at=$9
		WHERE obligation_id=$10`,
		o.GrossPaymentAmount, o.TaxableBaseAmount, o.WithholdingRatePercent,
		o.WithheldAmount, o.TaxTreatyExemption, o.ExemptionCertificateRef,
		o.Notes, o.EffectiveTo, o.UpdatedAt, o.ObligationID,
	)
	if err != nil {
		return fmt.Errorf("update withholding tax obligation: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) Remit(ctx context.Context, id string, req *domain.RemitObligationRequest) (*domain.WithholdingTaxObligation, error) {
	o, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if o.Status == domain.StatusRemitted {
		return nil, domain.ErrAlreadyRemitted
	}
	if o.Status == domain.StatusCancelled {
		return nil, domain.ErrAlreadyCancelled
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
	o.Status = domain.StatusRemitted
	o.RemittanceReference = &req.RemittanceReference
	o.RemittedAt = &now
	o.RemittedBy = &req.RemittedBy
	if req.Notes != "" {
		o.Notes = req.Notes
	}
	o.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE withholding_tax_obligations
		SET status=$1, remittance_reference=$2, remitted_at=$3, remitted_by=$4, notes=$5, updated_at=$6
		WHERE obligation_id=$7`,
		string(o.Status), o.RemittanceReference, o.RemittedAt, o.RemittedBy, o.Notes, o.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("remit withholding tax obligation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

func (s *PgStore) Cancel(ctx context.Context, id string, req *domain.CancelObligationRequest) (*domain.WithholdingTaxObligation, error) {
	o, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if o.Status == domain.StatusRemitted {
		return nil, domain.ErrAlreadyRemitted
	}
	if o.Status == domain.StatusCancelled {
		return nil, domain.ErrAlreadyCancelled
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
	o.Status = domain.StatusCancelled
	if req.Reason != "" {
		if o.Notes != "" {
			o.Notes = o.Notes + " | Cancelled: " + req.Reason
		} else {
			o.Notes = "Cancelled: " + req.Reason
		}
	}
	o.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE withholding_tax_obligations
		SET status=$1, notes=$2, updated_at=$3
		WHERE obligation_id=$4`,
		string(o.Status), o.Notes, o.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("cancel withholding tax obligation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}
