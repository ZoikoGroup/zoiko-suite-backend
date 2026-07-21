package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/vat-gst-svc/internal/domain"
	"zoiko.io/vat-gst-svc/internal/middleware"
)

type Store interface {
	CreateVATReturn(ctx context.Context, r *domain.VATReturn) error
	GetVATReturn(ctx context.Context, id string) (*domain.VATReturn, error)
	ListVATReturns(ctx context.Context, legalEntityID, jurisdictionID, status string) ([]domain.VATReturn, error)
	UpdateVATReturn(ctx context.Context, r *domain.VATReturn) error
	FileVATReturn(ctx context.Context, id, filedBy string) (*domain.VATReturn, error)
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

func (s *PgStore) CreateVATReturn(ctx context.Context, r *domain.VATReturn) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if r.ReturnID == "" {
		r.ReturnID = "vret-" + uuid.New().String()
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
	r.NetTaxPayable = r.OutputTaxAmount - r.InputTaxAmount

	_, err = tx.Exec(ctx, `
		INSERT INTO vat_returns
			(return_id, tenant_id, legal_entity_id, jurisdiction_id, tax_registration_number,
			 tax_period, total_sales_amount, total_purchase_amount, output_tax_amount, input_tax_amount,
			 net_tax_payable, currency, status, effective_from, effective_to, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		r.ReturnID, r.TenantID, r.LegalEntityID, r.JurisdictionID, r.TaxRegistrationNumber,
		r.TaxPeriod, r.TotalSalesAmount, r.TotalPurchaseAmount, r.OutputTaxAmount, r.InputTaxAmount,
		r.NetTaxPayable, r.Currency, string(r.Status), r.EffectiveFrom, r.EffectiveTo, r.CreatedBy, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert vat return: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetVATReturn(ctx context.Context, id string) (*domain.VATReturn, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var r domain.VATReturn
	var status string
	err = tx.QueryRow(ctx, `
		SELECT return_id, tenant_id, legal_entity_id, jurisdiction_id, tax_registration_number,
		       tax_period, total_sales_amount, total_purchase_amount, output_tax_amount, input_tax_amount,
		       net_tax_payable, currency, status, filed_at, filed_by, effective_from, effective_to,
		       created_by, created_at, updated_at
		FROM vat_returns WHERE return_id = $1`, id,
	).Scan(
		&r.ReturnID, &r.TenantID, &r.LegalEntityID, &r.JurisdictionID, &r.TaxRegistrationNumber,
		&r.TaxPeriod, &r.TotalSalesAmount, &r.TotalPurchaseAmount, &r.OutputTaxAmount, &r.InputTaxAmount,
		&r.NetTaxPayable, &r.Currency, &status, &r.FiledAt, &r.FiledBy, &r.EffectiveFrom, &r.EffectiveTo,
		&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrVATReturnNotFound
		}
		return nil, err
	}
	r.Status = domain.FilingStatus(status)
	_ = tx.Commit(ctx)
	return &r, nil
}

func (s *PgStore) ListVATReturns(ctx context.Context, legalEntityID, jurisdictionID, status string) ([]domain.VATReturn, error) {
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
		       tax_period, total_sales_amount, total_purchase_amount, output_tax_amount, input_tax_amount,
		       net_tax_payable, currency, status, filed_at, filed_by, effective_from, effective_to,
		       created_by, created_at, updated_at
		FROM vat_returns
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR jurisdiction_id = $2)
		  AND ($3 = '' OR status = $3)
		ORDER BY created_at DESC`, legalEntityID, jurisdictionID, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.VATReturn
	for rows.Next() {
		var r domain.VATReturn
		var stat string
		if err := rows.Scan(
			&r.ReturnID, &r.TenantID, &r.LegalEntityID, &r.JurisdictionID, &r.TaxRegistrationNumber,
			&r.TaxPeriod, &r.TotalSalesAmount, &r.TotalPurchaseAmount, &r.OutputTaxAmount, &r.InputTaxAmount,
			&r.NetTaxPayable, &r.Currency, &stat, &r.FiledAt, &r.FiledBy, &r.EffectiveFrom, &r.EffectiveTo,
			&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		r.Status = domain.FilingStatus(stat)
		out = append(out, r)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateVATReturn(ctx context.Context, r *domain.VATReturn) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	r.NetTaxPayable = r.OutputTaxAmount - r.InputTaxAmount
	r.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE vat_returns
		SET total_sales_amount=$1, total_purchase_amount=$2, output_tax_amount=$3, input_tax_amount=$4,
		    net_tax_payable=$5, status=$6, effective_to=$7, updated_at=$8
		WHERE return_id=$9`,
		r.TotalSalesAmount, r.TotalPurchaseAmount, r.OutputTaxAmount, r.InputTaxAmount,
		r.NetTaxPayable, string(r.Status), r.EffectiveTo, r.UpdatedAt, r.ReturnID,
	)
	if err != nil {
		return fmt.Errorf("update vat return: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) FileVATReturn(ctx context.Context, id, filedBy string) (*domain.VATReturn, error) {
	r, err := s.GetVATReturn(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Status == domain.StatusFiled || r.Status == domain.StatusAccepted {
		return nil, domain.ErrAlreadyFiled
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
	r.Status = domain.StatusFiled
	r.FiledAt = &now
	r.FiledBy = &filedBy
	r.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE vat_returns
		SET status=$1, filed_at=$2, filed_by=$3, updated_at=$4
		WHERE return_id=$5`,
		string(r.Status), r.FiledAt, r.FiledBy, r.UpdatedAt, id,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
