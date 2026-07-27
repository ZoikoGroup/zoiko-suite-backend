package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/tax-rules-svc/internal/domain"
	"zoiko.io/tax-rules-svc/internal/middleware"
)

type Store interface {
	CreateTaxRule(ctx context.Context, r *domain.TaxRule) error
	GetTaxRule(ctx context.Context, id string) (*domain.TaxRule, error)
	ListTaxRules(ctx context.Context, jurisdictionID, category, status string) ([]domain.TaxRule, error)
	UpdateTaxRule(ctx context.Context, r *domain.TaxRule) error
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

func (s *PgStore) CreateTaxRule(ctx context.Context, r *domain.TaxRule) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if r.RuleID == "" {
		r.RuleID = "trule-" + uuid.New().String()
	}
	r.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	if r.Status == "" {
		r.Status = domain.StatusDraft
	}
	r.Version = 1
	if r.ExemptionsJSON == "" {
		r.ExemptionsJSON = "{}"
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO tax_rules
			(rule_id, tenant_id, jurisdiction_id, rule_code, name, category, tax_rate_percentage,
			 standard_deductions, exemptions_json, status, version, effective_from, effective_to,
			 created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		r.RuleID, r.TenantID, r.JurisdictionID, r.RuleCode, r.Name, string(r.Category), r.TaxRatePercentage,
		r.StandardDeductions, r.ExemptionsJSON, string(r.Status), r.Version, r.EffectiveFrom, r.EffectiveTo,
		r.CreatedBy, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert tax rule: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetTaxRule(ctx context.Context, id string) (*domain.TaxRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var r domain.TaxRule
	var cat, status string
	err = tx.QueryRow(ctx, `
		SELECT rule_id, tenant_id, jurisdiction_id, rule_code, name, category, tax_rate_percentage,
		       standard_deductions, exemptions_json, status, version, effective_from, effective_to,
		       created_by, created_at, updated_at
		FROM tax_rules WHERE rule_id = $1`, id,
	).Scan(
		&r.RuleID, &r.TenantID, &r.JurisdictionID, &r.RuleCode, &r.Name, &cat, &r.TaxRatePercentage,
		&r.StandardDeductions, &r.ExemptionsJSON, &status, &r.Version, &r.EffectiveFrom, &r.EffectiveTo,
		&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrTaxRuleNotFound
		}
		return nil, err
	}
	r.Category = domain.TaxCategory(cat)
	r.Status = domain.RuleStatus(status)
	_ = tx.Commit(ctx)
	return &r, nil
}

func (s *PgStore) ListTaxRules(ctx context.Context, jurisdictionID, category, status string) ([]domain.TaxRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT rule_id, tenant_id, jurisdiction_id, rule_code, name, category, tax_rate_percentage,
		       standard_deductions, exemptions_json, status, version, effective_from, effective_to,
		       created_by, created_at, updated_at
		FROM tax_rules
		WHERE ($1 = '' OR jurisdiction_id = $1)
		  AND ($2 = '' OR category = $2)
		  AND ($3 = '' OR status = $3)
		ORDER BY created_at DESC`, jurisdictionID, category, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.TaxRule
	for rows.Next() {
		var r domain.TaxRule
		var cat, stat string
		if err := rows.Scan(
			&r.RuleID, &r.TenantID, &r.JurisdictionID, &r.RuleCode, &r.Name, &cat, &r.TaxRatePercentage,
			&r.StandardDeductions, &r.ExemptionsJSON, &stat, &r.Version, &r.EffectiveFrom, &r.EffectiveTo,
			&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		r.Category = domain.TaxCategory(cat)
		r.Status = domain.RuleStatus(stat)
		out = append(out, r)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateTaxRule(ctx context.Context, r *domain.TaxRule) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	r.Version++
	r.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE tax_rules
		SET name=$1, category=$2, tax_rate_percentage=$3, standard_deductions=$4,
		    exemptions_json=$5, status=$6, version=$7, effective_to=$8, updated_at=$9
		WHERE rule_id=$10`,
		r.Name, string(r.Category), r.TaxRatePercentage, r.StandardDeductions,
		r.ExemptionsJSON, string(r.Status), r.Version, r.EffectiveTo, r.UpdatedAt, r.RuleID,
	)
	if err != nil {
		return fmt.Errorf("update tax rule: %w", err)
	}

	return tx.Commit(ctx)
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
