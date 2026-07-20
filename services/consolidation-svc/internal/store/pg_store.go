package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
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

func (s *PgStore) CreateRun(ctx context.Context, run *domain.ConsolidationRun) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO consolidation_runs (
				consolidation_run_id, tenant_id, group_legal_entity_id, fiscal_period,
				target_currency, status, exception_count, started_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, run.ConsolidationRunID, tenantID, run.GroupLegalEntityID, run.FiscalPeriod,
			run.TargetCurrency, run.Status, run.ExceptionCount, run.StartedAt)
		return err
	})
}

func (s *PgStore) GetRun(ctx context.Context, id string) (*domain.ConsolidationRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var run domain.ConsolidationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT consolidation_run_id, tenant_id, group_legal_entity_id, fiscal_period,
			       target_currency, status, exception_count, started_at, completed_at
			FROM consolidation_runs
			WHERE consolidation_run_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&run.ConsolidationRunID, &run.TenantID, &run.GroupLegalEntityID, &run.FiscalPeriod,
			&run.TargetCurrency, &run.Status, &run.ExceptionCount, &run.StartedAt, &run.CompletedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *PgStore) ListRuns(ctx context.Context, groupLegalEntityID string) ([]domain.ConsolidationRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.ConsolidationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT consolidation_run_id, tenant_id, group_legal_entity_id, fiscal_period,
			       target_currency, status, exception_count, started_at, completed_at
			FROM consolidation_runs
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if groupLegalEntityID != "" {
			args = append(args, groupLegalEntityID)
			query += fmt.Sprintf(" AND group_legal_entity_id = $%d", len(args))
		}
		query += " ORDER BY started_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var run domain.ConsolidationRun
			if err := rows.Scan(
				&run.ConsolidationRunID, &run.TenantID, &run.GroupLegalEntityID, &run.FiscalPeriod,
				&run.TargetCurrency, &run.Status, &run.ExceptionCount, &run.StartedAt, &run.CompletedAt,
			); err != nil {
				return err
			}
			out = append(out, run)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) CompleteRun(ctx context.Context, id, status string, exceptionCount int, completedAt time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE consolidation_runs
			SET status = $1, exception_count = $2, completed_at = $3
			WHERE consolidation_run_id = $4 AND tenant_id = $5
		`, status, exceptionCount, completedAt, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrRunNotFound
		}
		return nil
	})
}

func (s *PgStore) CreateBalanceSnapshots(ctx context.Context, snapshots []domain.BalanceSnapshot) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if len(snapshots) == 0 {
		return nil
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		for _, snap := range snapshots {
			_, err := tx.Exec(ctx, `
				INSERT INTO balance_snapshots (
					balance_snapshot_id, tenant_id, consolidation_run_id, legal_entity_id,
					fiscal_period, account_code, consolidated_balance, currency_code,
					snapshot_signature, generated_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			`, snap.BalanceSnapshotID, tenantID, snap.ConsolidationRunID, snap.LegalEntityID,
				snap.FiscalPeriod, snap.AccountCode, snap.ConsolidatedBalance, snap.CurrencyCode,
				snap.SnapshotSignature, snap.GeneratedAt)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PgStore) ListSnapshotsByRun(ctx context.Context, runID string) ([]domain.BalanceSnapshot, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.BalanceSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT balance_snapshot_id, tenant_id, consolidation_run_id, legal_entity_id,
			       fiscal_period, account_code, consolidated_balance, currency_code,
			       snapshot_signature, generated_at
			FROM balance_snapshots
			WHERE consolidation_run_id = $1 AND tenant_id = $2
			ORDER BY account_code ASC
		`, runID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var snap domain.BalanceSnapshot
			if err := rows.Scan(
				&snap.BalanceSnapshotID, &snap.TenantID, &snap.ConsolidationRunID, &snap.LegalEntityID,
				&snap.FiscalPeriod, &snap.AccountCode, &snap.ConsolidatedBalance, &snap.CurrencyCode,
				&snap.SnapshotSignature, &snap.GeneratedAt,
			); err != nil {
				return err
			}
			out = append(out, snap)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}