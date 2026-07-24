package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/tax-determination-svc/internal/domain"
	"zoiko.io/tax-determination-svc/internal/middleware"
)

type Store interface {
	CreateDetermination(ctx context.Context, d *domain.TaxDetermination) error
	GetDetermination(ctx context.Context, id string) (*domain.TaxDetermination, error)
	ListDeterminations(ctx context.Context, transactionID, jurisdictionID, status string) ([]domain.TaxDetermination, error)
	UpdateDetermination(ctx context.Context, d *domain.TaxDetermination) error
	OverrideDetermination(ctx context.Context, id string, req *domain.OverrideTaxRequest) (*domain.TaxDetermination, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID))
	return err
}

func (s *PgStore) CreateDetermination(ctx context.Context, d *domain.TaxDetermination) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if d.DeterminationID == "" {
		d.DeterminationID = "tdet-" + uuid.New().String()
	}
	d.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	d.EvaluatedAt = now
	d.CreatedAt = now
	d.UpdatedAt = now
	if d.Status == "" {
		d.Status = domain.StatusCalculated
	}
	if d.Currency == "" {
		d.Currency = "USD"
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO tax_determinations
			(determination_id, tenant_id, transaction_id, source_module, legal_entity_id, jurisdiction_id,
			 rule_id, tax_category, gross_amount, taxable_amount, tax_rate_percentage, calculated_tax_amount,
			 exempt_amount, currency, status, effective_from, effective_to, evaluated_at, evaluated_by,
			 created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		d.DeterminationID, d.TenantID, d.TransactionID, d.SourceModule, d.LegalEntityID, d.JurisdictionID,
		d.RuleID, d.TaxCategory, d.GrossAmount, d.TaxableAmount, d.TaxRatePercentage, d.CalculatedTaxAmount,
		d.ExemptAmount, d.Currency, string(d.Status), d.EffectiveFrom, d.EffectiveTo, d.EvaluatedAt, d.EvaluatedBy,
		d.CreatedAt, d.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert tax determination: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetDetermination(ctx context.Context, id string) (*domain.TaxDetermination, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var d domain.TaxDetermination
	var status string
	err = tx.QueryRow(ctx, `
		SELECT determination_id, tenant_id, transaction_id, source_module, legal_entity_id, jurisdiction_id,
		       COALESCE(rule_id,''), tax_category, gross_amount, taxable_amount, tax_rate_percentage,
		       calculated_tax_amount, exempt_amount, currency, status, effective_from, effective_to,
		       evaluated_at, evaluated_by, created_at, updated_at
		FROM tax_determinations WHERE determination_id = $1`, id,
	).Scan(
		&d.DeterminationID, &d.TenantID, &d.TransactionID, &d.SourceModule, &d.LegalEntityID, &d.JurisdictionID,
		&d.RuleID, &d.TaxCategory, &d.GrossAmount, &d.TaxableAmount, &d.TaxRatePercentage,
		&d.CalculatedTaxAmount, &d.ExemptAmount, &d.Currency, &status, &d.EffectiveFrom, &d.EffectiveTo,
		&d.EvaluatedAt, &d.EvaluatedBy, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrTaxDeterminationNotFound
		}
		return nil, err
	}
	d.Status = domain.DeterminationStatus(status)
	_ = tx.Commit(ctx)
	return &d, nil
}

func (s *PgStore) ListDeterminations(ctx context.Context, transactionID, jurisdictionID, status string) ([]domain.TaxDetermination, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT determination_id, tenant_id, transaction_id, source_module, legal_entity_id, jurisdiction_id,
		       COALESCE(rule_id,''), tax_category, gross_amount, taxable_amount, tax_rate_percentage,
		       calculated_tax_amount, exempt_amount, currency, status, effective_from, effective_to,
		       evaluated_at, evaluated_by, created_at, updated_at
		FROM tax_determinations
		WHERE ($1 = '' OR transaction_id = $1)
		  AND ($2 = '' OR jurisdiction_id = $2)
		  AND ($3 = '' OR status = $3)
		ORDER BY created_at DESC`, transactionID, jurisdictionID, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TaxDetermination
	for rows.Next() {
		var d domain.TaxDetermination
		var stat string
		if err := rows.Scan(
			&d.DeterminationID, &d.TenantID, &d.TransactionID, &d.SourceModule, &d.LegalEntityID, &d.JurisdictionID,
			&d.RuleID, &d.TaxCategory, &d.GrossAmount, &d.TaxableAmount, &d.TaxRatePercentage,
			&d.CalculatedTaxAmount, &d.ExemptAmount, &d.Currency, &stat, &d.EffectiveFrom, &d.EffectiveTo,
			&d.EvaluatedAt, &d.EvaluatedBy, &d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			return nil, err
		}
		d.Status = domain.DeterminationStatus(stat)
		out = append(out, d)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateDetermination(ctx context.Context, d *domain.TaxDetermination) error {
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
		UPDATE tax_determinations
		SET gross_amount=$1, taxable_amount=$2, tax_rate_percentage=$3, calculated_tax_amount=$4,
		    exempt_amount=$5, status=$6, effective_to=$7, updated_at=$8
		WHERE determination_id=$9`,
		d.GrossAmount, d.TaxableAmount, d.TaxRatePercentage, d.CalculatedTaxAmount,
		d.ExemptAmount, string(d.Status), d.EffectiveTo, d.UpdatedAt, d.DeterminationID,
	)
	if err != nil {
		return fmt.Errorf("update tax determination: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) OverrideDetermination(ctx context.Context, id string, req *domain.OverrideTaxRequest) (*domain.TaxDetermination, error) {
	d, err := s.GetDetermination(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.Status == domain.StatusOverridden {
		return nil, domain.ErrAlreadyOverridden
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	d.Status = domain.StatusOverridden
	d.CalculatedTaxAmount = req.OverriddenTaxAmount
	d.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE tax_determinations
		SET calculated_tax_amount=$1, status=$2, updated_at=$3
		WHERE determination_id=$4`,
		d.CalculatedTaxAmount, string(d.Status), d.UpdatedAt, id,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
