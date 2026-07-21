package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/corporate-tax-svc/internal/domain"
	"zoiko.io/corporate-tax-svc/internal/middleware"
)

// Store defines all persistence operations for corporate tax returns.
type Store interface {
	Create(ctx context.Context, r *domain.TaxReturn) error
	GetByID(ctx context.Context, id string) (*domain.TaxReturn, error)
	List(ctx context.Context, legalEntityID, jurisdictionID, status string, fiscalYear int) ([]domain.TaxReturn, error)
	Update(ctx context.Context, r *domain.TaxReturn) error
	Submit(ctx context.Context, id, submittedBy string) (*domain.TaxReturn, error)
	Assess(ctx context.Context, id string, req *domain.AssessTaxReturnRequest) (*domain.TaxReturn, error)
}

// PgStore is the PostgreSQL-backed implementation of Store.
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

func (s *PgStore) Create(ctx context.Context, r *domain.TaxReturn) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if r.ReturnID == "" {
		r.ReturnID = "ctret-" + uuid.New().String()
	}
	r.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	if r.Status == "" {
		r.Status = domain.StatusDraft
	}
	if r.Currency == "" {
		r.Currency = "USD"
	}
	r.Compute()

	_, err = tx.Exec(ctx, `
		INSERT INTO corporate_tax_returns
			(return_id, tenant_id, legal_entity_id, jurisdiction_id, tax_registration_number,
			 fiscal_year, accounting_period_start, accounting_period_end,
			 gross_revenue, allowable_deductions, taxable_income, tax_rate_percent,
			 gross_tax_liability, tax_credits, net_tax_payable, tax_already_paid,
			 balance_due, currency, status, notes, effective_from, effective_to,
			 created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`,
		r.ReturnID, r.TenantID, r.LegalEntityID, r.JurisdictionID, r.TaxRegistrationNumber,
		r.FiscalYear, r.AccountingPeriodStart, r.AccountingPeriodEnd,
		r.GrossRevenue, r.AllowableDeductions, r.TaxableIncome, r.TaxRatePercent,
		r.GrossTaxLiability, r.TaxCredits, r.NetTaxPayable, r.TaxAlreadyPaid,
		r.BalanceDue, r.Currency, string(r.Status), r.Notes, r.EffectiveFrom, r.EffectiveTo,
		r.CreatedBy, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert corporate tax return: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) GetByID(ctx context.Context, id string) (*domain.TaxReturn, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var r domain.TaxReturn
	var status string
	err = tx.QueryRow(ctx, `
		SELECT return_id, tenant_id, legal_entity_id, jurisdiction_id, tax_registration_number,
		       fiscal_year, accounting_period_start, accounting_period_end,
		       gross_revenue, allowable_deductions, taxable_income, tax_rate_percent,
		       gross_tax_liability, tax_credits, net_tax_payable, tax_already_paid,
		       balance_due, currency, status, submitted_at, submitted_by,
		       assessed_tax_amount, assessment_reference, notes,
		       effective_from, effective_to, created_by, created_at, updated_at
		FROM corporate_tax_returns WHERE return_id = $1`, id,
	).Scan(
		&r.ReturnID, &r.TenantID, &r.LegalEntityID, &r.JurisdictionID, &r.TaxRegistrationNumber,
		&r.FiscalYear, &r.AccountingPeriodStart, &r.AccountingPeriodEnd,
		&r.GrossRevenue, &r.AllowableDeductions, &r.TaxableIncome, &r.TaxRatePercent,
		&r.GrossTaxLiability, &r.TaxCredits, &r.NetTaxPayable, &r.TaxAlreadyPaid,
		&r.BalanceDue, &r.Currency, &status, &r.SubmittedAt, &r.SubmittedBy,
		&r.AssessedTaxAmount, &r.AssessmentReference, &r.Notes,
		&r.EffectiveFrom, &r.EffectiveTo, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrTaxReturnNotFound
		}
		return nil, err
	}
	r.Status = domain.FilingStatus(status)
	_ = tx.Commit(ctx)
	return &r, nil
}

func (s *PgStore) List(ctx context.Context, legalEntityID, jurisdictionID, status string, fiscalYear int) ([]domain.TaxReturn, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT return_id, tenant_id, legal_entity_id, jurisdiction_id, tax_registration_number,
		       fiscal_year, accounting_period_start, accounting_period_end,
		       gross_revenue, allowable_deductions, taxable_income, tax_rate_percent,
		       gross_tax_liability, tax_credits, net_tax_payable, tax_already_paid,
		       balance_due, currency, status, submitted_at, submitted_by,
		       assessed_tax_amount, assessment_reference, notes,
		       effective_from, effective_to, created_by, created_at, updated_at
		FROM corporate_tax_returns
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR jurisdiction_id = $2)
		  AND ($3 = '' OR status = $3)
		  AND ($4 = 0  OR fiscal_year = $4)
		ORDER BY fiscal_year DESC, created_at DESC`,
		legalEntityID, jurisdictionID, status, fiscalYear,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TaxReturn
	for rows.Next() {
		var r domain.TaxReturn
		var stat string
		if err := rows.Scan(
			&r.ReturnID, &r.TenantID, &r.LegalEntityID, &r.JurisdictionID, &r.TaxRegistrationNumber,
			&r.FiscalYear, &r.AccountingPeriodStart, &r.AccountingPeriodEnd,
			&r.GrossRevenue, &r.AllowableDeductions, &r.TaxableIncome, &r.TaxRatePercent,
			&r.GrossTaxLiability, &r.TaxCredits, &r.NetTaxPayable, &r.TaxAlreadyPaid,
			&r.BalanceDue, &r.Currency, &stat, &r.SubmittedAt, &r.SubmittedBy,
			&r.AssessedTaxAmount, &r.AssessmentReference, &r.Notes,
			&r.EffectiveFrom, &r.EffectiveTo, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		r.Status = domain.FilingStatus(stat)
		out = append(out, r)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) Update(ctx context.Context, r *domain.TaxReturn) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}
	r.Compute()
	r.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE corporate_tax_returns
		SET gross_revenue=$1, allowable_deductions=$2, taxable_income=$3, tax_rate_percent=$4,
		    gross_tax_liability=$5, tax_credits=$6, net_tax_payable=$7, tax_already_paid=$8,
		    balance_due=$9, notes=$10, effective_to=$11, updated_at=$12
		WHERE return_id=$13`,
		r.GrossRevenue, r.AllowableDeductions, r.TaxableIncome, r.TaxRatePercent,
		r.GrossTaxLiability, r.TaxCredits, r.NetTaxPayable, r.TaxAlreadyPaid,
		r.BalanceDue, r.Notes, r.EffectiveTo, r.UpdatedAt, r.ReturnID,
	)
	if err != nil {
		return fmt.Errorf("update corporate tax return: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) Submit(ctx context.Context, id, submittedBy string) (*domain.TaxReturn, error) {
	r, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Status == domain.StatusSubmitted || r.Status == domain.StatusAssessed || r.Status == domain.StatusSettled {
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
	r.Status = domain.StatusSubmitted
	r.SubmittedAt = &now
	r.SubmittedBy = &submittedBy
	r.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE corporate_tax_returns
		SET status=$1, submitted_at=$2, submitted_by=$3, updated_at=$4
		WHERE return_id=$5`,
		string(r.Status), r.SubmittedAt, r.SubmittedBy, r.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("submit corporate tax return: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *PgStore) Assess(ctx context.Context, id string, req *domain.AssessTaxReturnRequest) (*domain.TaxReturn, error) {
	r, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
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
	r.Status = domain.StatusAssessed
	r.AssessedTaxAmount = &req.AssessedTaxAmount
	r.AssessmentReference = &req.AssessmentReference
	if req.Notes != "" {
		r.Notes = req.Notes
	}
	r.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE corporate_tax_returns
		SET status=$1, assessed_tax_amount=$2, assessment_reference=$3, notes=$4, updated_at=$5
		WHERE return_id=$6`,
		string(r.Status), r.AssessedTaxAmount, r.AssessmentReference, r.Notes, r.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("assess corporate tax return: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}
