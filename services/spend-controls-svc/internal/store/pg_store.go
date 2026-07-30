package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/spend-controls-svc/internal/domain"
	svcmiddleware "zoiko.io/spend-controls-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func (s *PgStore) CreatePolicy(ctx context.Context, p *domain.SpendPolicy) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO spend_policies (
				spend_policy_id, tenant_id, legal_entity_id, category, period,
				threshold_amount, currency_code, active_flag, created_by_principal_id,
				created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		`, p.SpendPolicyID, tenantID, p.LegalEntityID, p.Category, p.Period,
			p.ThresholdAmount, p.CurrencyCode, p.ActiveFlag, p.CreatedByPrincipalID,
			p.CreatedAt, p.UpdatedAt)
		return err
	})
}

func (s *PgStore) ListPolicies(ctx context.Context, legalEntityID, category string) ([]domain.SpendPolicy, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.SpendPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT spend_policy_id, tenant_id, legal_entity_id, category, period,
			       threshold_amount, currency_code, active_flag, created_by_principal_id,
			       created_at, updated_at
			FROM spend_policies
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if category != "" {
			args = append(args, category)
			query += fmt.Sprintf(" AND category = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var p domain.SpendPolicy
			if err := rows.Scan(
				&p.SpendPolicyID, &p.TenantID, &p.LegalEntityID, &p.Category, &p.Period,
				&p.ThresholdAmount, &p.CurrencyCode, &p.ActiveFlag, &p.CreatedByPrincipalID,
				&p.CreatedAt, &p.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FindActivePolicy returns the single active policy matching legalEntityID
// and category, or nil if none is configured — callers must treat "no
// policy" as a distinct outcome from "policy denies."
func (s *PgStore) FindActivePolicy(ctx context.Context, legalEntityID, category string) (*domain.SpendPolicy, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var p domain.SpendPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT spend_policy_id, tenant_id, legal_entity_id, category, period,
			       threshold_amount, currency_code, active_flag, created_by_principal_id,
			       created_at, updated_at
			FROM spend_policies
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND category = $3 AND active_flag = TRUE
			ORDER BY created_at DESC
			LIMIT 1
		`, tenantID, legalEntityID, category).Scan(
			&p.SpendPolicyID, &p.TenantID, &p.LegalEntityID, &p.Category, &p.Period,
			&p.ThresholdAmount, &p.CurrencyCode, &p.ActiveFlag, &p.CreatedByPrincipalID,
			&p.CreatedAt, &p.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SumConsumption sums ALLOWED consumption amounts for a policy since a given
// window start — the running total a new spend-check's amount is added to
// before comparing against the policy's threshold.
func (s *PgStore) SumConsumption(ctx context.Context, spendPolicyID string, since time.Time) (float64, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}

	var total float64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount), 0)
			FROM spend_consumptions
			WHERE tenant_id = $1 AND spend_policy_id = $2
			  AND decision_outcome = 'ALLOWED' AND recorded_at >= $3
		`, tenantID, spendPolicyID, since).Scan(&total)
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// FindConsumptionByCorrelation looks up a prior spend-check by idempotency
// key, so a retried request replays the original decision instead of
// re-evaluating (and potentially double-counting) consumption.
func (s *PgStore) FindConsumptionByCorrelation(ctx context.Context, correlationID string) (*domain.SpendConsumption, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var c domain.SpendConsumption
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT consumption_id, tenant_id, legal_entity_id, spend_policy_id, amount,
			       currency_code, COALESCE(source_reference, ''), correlation_id,
			       decision_outcome, recorded_by_principal_id, recorded_at
			FROM spend_consumptions
			WHERE tenant_id = $1 AND correlation_id = $2
		`, tenantID, correlationID).Scan(
			&c.ConsumptionID, &c.TenantID, &c.LegalEntityID, &c.SpendPolicyID, &c.Amount,
			&c.CurrencyCode, &c.SourceReference, &c.CorrelationID,
			&c.DecisionOutcome, &c.RecordedByPrincipalID, &c.RecordedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// RecordConsumption inserts an ALLOWED consumption row, idempotent on
// (tenant_id, correlation_id): if a retry races a concurrent original
// request, the loser fetches and returns the winner's row instead of
// erroring or double-counting.
func (s *PgStore) RecordConsumption(ctx context.Context, c *domain.SpendConsumption) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO spend_consumptions (
				consumption_id, tenant_id, legal_entity_id, spend_policy_id, amount,
				currency_code, source_reference, correlation_id, decision_outcome,
				recorded_by_principal_id, recorded_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, c.ConsumptionID, tenantID, c.LegalEntityID, c.SpendPolicyID, c.Amount,
			c.CurrencyCode, c.SourceReference, c.CorrelationID, c.DecisionOutcome,
			c.RecordedByPrincipalID, c.RecordedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}

		// Conflict: a consumption for this (tenant_id, correlation_id)
		// already exists — a concurrent retry won the race. Fetch its
		// values so the caller returns the winner's decision, not its own.
		row := tx.QueryRow(ctx, `
			SELECT consumption_id, spend_policy_id, amount, currency_code,
			       COALESCE(source_reference, ''), decision_outcome, recorded_at
			FROM spend_consumptions WHERE tenant_id = $1 AND correlation_id = $2
		`, tenantID, c.CorrelationID)
		return row.Scan(
			&c.ConsumptionID, &c.SpendPolicyID, &c.Amount, &c.CurrencyCode,
			&c.SourceReference, &c.DecisionOutcome, &c.RecordedAt,
		)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) ListConsumptions(ctx context.Context, legalEntityID, spendPolicyID string) ([]domain.SpendConsumption, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.SpendConsumption
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT consumption_id, tenant_id, legal_entity_id, spend_policy_id, amount,
			       currency_code, COALESCE(source_reference, ''), correlation_id,
			       decision_outcome, recorded_by_principal_id, recorded_at
			FROM spend_consumptions
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if spendPolicyID != "" {
			args = append(args, spendPolicyID)
			query += fmt.Sprintf(" AND spend_policy_id = $%d", len(args))
		}
		query += " ORDER BY recorded_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.SpendConsumption
			if err := rows.Scan(
				&c.ConsumptionID, &c.TenantID, &c.LegalEntityID, &c.SpendPolicyID, &c.Amount,
				&c.CurrencyCode, &c.SourceReference, &c.CorrelationID,
				&c.DecisionOutcome, &c.RecordedByPrincipalID, &c.RecordedAt,
			); err != nil {
				return err
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
