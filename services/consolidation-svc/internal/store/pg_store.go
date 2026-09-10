package store

import (
	"context"
	"encoding/json"
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

// CreateBalanceContributions persists the ACC-13 entity-to-group provenance
// records — see migration 000002's doc comment for why this exists.
func (s *PgStore) CreateBalanceContributions(ctx context.Context, contributions []domain.BalanceContribution) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	if len(contributions) == 0 {
		return nil
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		for _, c := range contributions {
			_, err := tx.Exec(ctx, `
				INSERT INTO balance_contributions (
					balance_contribution_id, tenant_id, consolidation_run_id,
					account_code, source_legal_entity_id, gross_amount, generated_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7)
			`, c.BalanceContributionID, tenantID, c.ConsolidationRunID,
				c.AccountCode, c.SourceLegalEntityID, c.GrossAmount, c.GeneratedAt)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PgStore) ListContributionsByRun(ctx context.Context, runID string) ([]domain.BalanceContribution, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.BalanceContribution
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT balance_contribution_id, tenant_id, consolidation_run_id,
			       account_code, source_legal_entity_id, gross_amount, generated_at
			FROM balance_contributions
			WHERE consolidation_run_id = $1 AND tenant_id = $2
			ORDER BY account_code ASC, source_legal_entity_id ASC
		`, runID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.BalanceContribution
			if err := rows.Scan(
				&c.BalanceContributionID, &c.TenantID, &c.ConsolidationRunID,
				&c.AccountCode, &c.SourceLegalEntityID, &c.GrossAmount, &c.GeneratedAt,
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

// ── ACC-12 Elimination & Consolidation Adjustments ──────────────────────────
// See migration 000003's doc comment.

const consolidationAdjustmentColumns = `
	consolidation_adjustment_id, tenant_id, group_legal_entity_id, fiscal_period,
	adjustment_type, description, status, lines, consolidation_book_journal_id,
	created_at, created_by_principal_id,
	approved_at, approved_by_principal_id,
	posted_at, posted_by_principal_id,
	reversed_at, reversed_by_principal_id, reversal_reason, superseded_by_adjustment_id`

func scanConsolidationAdjustment(row pgx.Row, a *domain.ConsolidationAdjustment) error {
	var linesRaw []byte
	if err := row.Scan(
		&a.ConsolidationAdjustmentID, &a.TenantID, &a.GroupLegalEntityID, &a.FiscalPeriod,
		&a.AdjustmentType, &a.Description, &a.Status, &linesRaw, &a.ConsolidationBookJournalID,
		&a.CreatedAt, &a.CreatedByPrincipalID,
		&a.ApprovedAt, &a.ApprovedByPrincipalID,
		&a.PostedAt, &a.PostedByPrincipalID,
		&a.ReversedAt, &a.ReversedByPrincipalID, &a.ReversalReason, &a.SupersededByAdjustmentID,
	); err != nil {
		return err
	}
	return json.Unmarshal(linesRaw, &a.Lines)
}

// GroupEntityHasRun answers ACC-12's own negative-path guard, "Adjustment
// targets statutory book": group_legal_entity_id must have appeared as a
// GroupLegalEntityID in at least one real ConsolidationRun for this
// tenant — a checkable fact, not a claim taken on faith from the caller.
func (s *PgStore) GroupEntityHasRun(ctx context.Context, groupLegalEntityID string) (bool, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	var exists bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM consolidation_runs WHERE tenant_id = $1 AND group_legal_entity_id = $2)
		`, tenantID, groupLegalEntityID).Scan(&exists)
	})
	return exists, err
}

// HasSnapshotForPeriod answers ACC-12's other negative-path guard,
// "Reverse after snapshot without supersession": once a real
// BalanceSnapshot exists for this group/period, a POSTED adjustment can
// only be reversed by naming a superseding replacement.
func (s *PgStore) HasSnapshotForPeriod(ctx context.Context, groupLegalEntityID, fiscalPeriod string) (bool, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	var exists bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM balance_snapshots
				WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_period = $3
			)
		`, tenantID, groupLegalEntityID, fiscalPeriod).Scan(&exists)
	})
	return exists, err
}

// CreateAdjustment inserts a new ConsolidationAdjustment. Callers land it
// directly in PENDING_APPROVAL — see migration 000003's doc comment on why
// no DRAFT resting state is reachable via this v1's API.
func (s *PgStore) CreateAdjustment(ctx context.Context, a *domain.ConsolidationAdjustment) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	linesJSON, err := json.Marshal(a.Lines)
	if err != nil {
		return err
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO consolidation_adjustments (
				consolidation_adjustment_id, tenant_id, group_legal_entity_id, fiscal_period,
				adjustment_type, description, status, lines,
				created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, a.ConsolidationAdjustmentID, tenantID, a.GroupLegalEntityID, a.FiscalPeriod,
			a.AdjustmentType, a.Description, a.Status, linesJSON,
			a.CreatedAt, a.CreatedByPrincipalID)
		return err
	})
}

func (s *PgStore) GetAdjustment(ctx context.Context, id string) (*domain.ConsolidationAdjustment, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var a domain.ConsolidationAdjustment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanConsolidationAdjustment(tx.QueryRow(ctx, `
			SELECT `+consolidationAdjustmentColumns+`
			FROM consolidation_adjustments
			WHERE consolidation_adjustment_id = $1 AND tenant_id = $2
		`, id, tenantID), &a)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAdjustmentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *PgStore) ListAdjustments(ctx context.Context, groupLegalEntityID, fiscalPeriod string) ([]domain.ConsolidationAdjustment, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.ConsolidationAdjustment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + consolidationAdjustmentColumns + ` FROM consolidation_adjustments WHERE tenant_id = $1`
		args := []any{tenantID}
		if groupLegalEntityID != "" {
			args = append(args, groupLegalEntityID)
			query += fmt.Sprintf(" AND group_legal_entity_id = $%d", len(args))
		}
		if fiscalPeriod != "" {
			args = append(args, fiscalPeriod)
			query += fmt.Sprintf(" AND fiscal_period = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.ConsolidationAdjustment
			if err := scanConsolidationAdjustment(rows, &a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ApproveAdjustment moves PENDING_APPROVAL -> APPROVED. Guarded to only
// ever succeed from PENDING_APPROVAL — the self-approval check (the spec's
// own "Top-side journal self-approved" negative path) is the handler's
// job, since it needs to compare against the CALLER's principal, which
// this store method deliberately doesn't decide.
func (s *PgStore) ApproveAdjustment(ctx context.Context, id, principalID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		res, err := tx.Exec(ctx, `
			UPDATE consolidation_adjustments
			SET status = $1, approved_at = $2, approved_by_principal_id = $3
			WHERE consolidation_adjustment_id = $4 AND tenant_id = $5 AND status = $6
		`, domain.AdjustmentStatusApproved, now, principalID, id, tenantID, domain.AdjustmentStatusPendingApproval)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidAdjustmentTransition
		}
		return nil
	})
}

// MarkAdjustmentPosted moves APPROVED -> POSTED, recording the real
// consolidation-book journal general-ledger-svc actually created.
func (s *PgStore) MarkAdjustmentPosted(ctx context.Context, id, principalID, journalID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		res, err := tx.Exec(ctx, `
			UPDATE consolidation_adjustments
			SET status = $1, posted_at = $2, posted_by_principal_id = $3, consolidation_book_journal_id = $4
			WHERE consolidation_adjustment_id = $5 AND tenant_id = $6 AND status = $7
		`, domain.AdjustmentStatusPosted, now, principalID, journalID, id, tenantID, domain.AdjustmentStatusApproved)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidAdjustmentTransition
		}
		return nil
	})
}

// ReverseAdjustment moves POSTED -> REVERSED, optionally recording the
// replacement adjustment that supersedes it.
func (s *PgStore) ReverseAdjustment(ctx context.Context, id, principalID, reason string, supersededBy *string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		res, err := tx.Exec(ctx, `
			UPDATE consolidation_adjustments
			SET status = $1, reversed_at = $2, reversed_by_principal_id = $3, reversal_reason = $4, superseded_by_adjustment_id = $5
			WHERE consolidation_adjustment_id = $6 AND tenant_id = $7 AND status = $8
		`, domain.AdjustmentStatusReversed, now, principalID, reason, supersededBy, id, tenantID, domain.AdjustmentStatusPosted)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidAdjustmentTransition
		}
		return nil
	})
}
