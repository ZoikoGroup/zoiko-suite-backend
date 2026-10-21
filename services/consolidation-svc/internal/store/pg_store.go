package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
)

type PgStore struct {
	pool        *pgxpool.Pool
	log         *zap.Logger
	eventRegion string
}

// Option configures a PgStore.
type Option func(*PgStore)

// WithEventRegion sets the residency region carried by emitted events
// (config EVENT_RESIDENCY_REGION).
func WithEventRegion(region string) Option {
	return func(s *PgStore) { s.eventRegion = region }
}

func New(pool *pgxpool.Pool, log *zap.Logger, opts ...Option) *PgStore {
	s := &PgStore{pool: pool, log: log}
	for _, o := range opts {
		o(s)
	}
	return s
}

// enqueueRunEvent writes one consolidation-run lifecycle event to the
// transactional outbox inside tx, the transaction that made the change it
// reports. fact is kebab-case (envelope.New rejects underscores); legacyType
// is the exact pre-standard event_type existing consumers still filter on.
// The aggregate is always the run itself — never a general-ledger or
// intercompany id the run happened to read from.
func (s *PgStore) enqueueRunEvent(ctx context.Context, tx pgx.Tx, fact, legacyType, tenantID, groupLegalEntityID, runID, actorID, correlationID string, data map[string]any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            "com.zoikosuite.accounting.consolidation-run." + fact,
		LegacyType:      legacyType,
		Service:         "consolidation-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   groupLegalEntityID,
		AggregateType:   "consolidation-run",
		AggregateID:     runID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		Classification:  envelope.Confidential,
		Data:            data,
	})
	if err != nil {
		return fmt.Errorf("build consolidation-run.%s event: %w", fact, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue consolidation-run.%s event: %w", fact, err)
	}
	return nil
}

// enqueueAdjustmentEvent writes one ACC-12 consolidation-adjustment
// lifecycle event to the transactional outbox inside tx, the transaction
// that made the change it reports. fact is kebab-case (envelope.New rejects
// underscores); legacyType is the exact pre-standard event name existing
// consumers filter on (ConsolidationAdjustmentCreated and the like). The
// aggregate is always the adjustment itself — never the run or the
// consolidation-book journal it may reference.
func (s *PgStore) enqueueAdjustmentEvent(ctx context.Context, tx pgx.Tx, fact, legacyType, tenantID, groupLegalEntityID, adjustmentID, actorID, correlationID string, data map[string]any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            "com.zoikosuite.accounting.consolidation-adjustment." + fact,
		LegacyType:      legacyType,
		Service:         "consolidation-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   groupLegalEntityID,
		AggregateType:   "consolidation-adjustment",
		AggregateID:     adjustmentID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		Classification:  envelope.Confidential,
		Data:            data,
	})
	if err != nil {
		return fmt.Errorf("build consolidation-adjustment.%s event: %w", fact, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue consolidation-adjustment.%s event: %w", fact, err)
	}
	return nil
}

// appendAdjustmentTransition writes one row of the append-only lifecycle log
// inside the same transaction as the status change it records. fromStatus is
// nil for the creation step; details carries step-specific evidence. The
// table is insert-and-read only: nothing in this store updates or deletes it.
func (s *PgStore) appendAdjustmentTransition(ctx context.Context, tx pgx.Tx, tenantID, adjustmentID string, fromStatus *string, toStatus, actorID, correlationID string, occurredAt time.Time, reason *string, details map[string]any) error {
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("marshal adjustment transition details: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO consolidation_adjustment_transitions (
			transition_id, tenant_id, consolidation_adjustment_id, from_status, to_status,
			actor_principal_id, occurred_at, correlation_id, reason, details
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, uuid.NewString(), tenantID, adjustmentID, fromStatus, toStatus,
		actorID, occurredAt, correlationID, reason, detailsJSON)
	if err != nil {
		return fmt.Errorf("record adjustment transition to %s: %w", toStatus, err)
	}
	return nil
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

func (s *PgStore) CreateRun(ctx context.Context, run *domain.ConsolidationRun, actorID, correlationID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO consolidation_runs (
				consolidation_run_id, tenant_id, group_legal_entity_id, fiscal_period,
				target_currency, status, exception_count, started_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, run.ConsolidationRunID, tenantID, run.GroupLegalEntityID, run.FiscalPeriod,
			run.TargetCurrency, run.Status, run.ExceptionCount, run.StartedAt); err != nil {
			return err
		}
		return s.enqueueRunEvent(ctx, tx, "started", "consolidation.run.started", tenantID, run.GroupLegalEntityID, run.ConsolidationRunID, actorID, correlationID, map[string]any{
			"consolidation_run_id":  run.ConsolidationRunID,
			"tenant_id":             tenantID,
			"group_legal_entity_id": run.GroupLegalEntityID,
			"fiscal_period":         run.FiscalPeriod,
			"target_currency":       run.TargetCurrency,
			"started_at":            run.StartedAt,
		})
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

// CompleteRun records a run's terminal status. Only a COMPLETED run emits
// events — exception.detected (when exceptions is non-empty) then completed,
// the same order and the same condition the handler used to publish them in.
// A FAILED run has never emitted an event; that is unchanged here.
func (s *PgStore) CompleteRun(ctx context.Context, id, status string, exceptionCount int, completedAt time.Time, actorID, correlationID string, snapshotCount int, exceptions []string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var groupLegalEntityID, fiscalPeriod string
		err := tx.QueryRow(ctx, `
			UPDATE consolidation_runs
			SET status = $1, exception_count = $2, completed_at = $3
			WHERE consolidation_run_id = $4 AND tenant_id = $5
			RETURNING group_legal_entity_id, fiscal_period
		`, status, exceptionCount, completedAt, id, tenantID).Scan(&groupLegalEntityID, &fiscalPeriod)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		if err != nil {
			return err
		}
		if status != "COMPLETED" {
			return nil
		}

		if len(exceptions) > 0 {
			if err := s.enqueueRunEvent(ctx, tx, "exception-detected", "consolidation.exception.detected", tenantID, groupLegalEntityID, id, actorID, correlationID, map[string]any{
				"consolidation_run_id":  id,
				"tenant_id":             tenantID,
				"group_legal_entity_id": groupLegalEntityID,
				"fiscal_period":         fiscalPeriod,
				"exceptions":            exceptions,
				"timestamp":             time.Now().UTC(),
			}); err != nil {
				return err
			}
		}
		return s.enqueueRunEvent(ctx, tx, "completed", "consolidation.completed", tenantID, groupLegalEntityID, id, actorID, correlationID, map[string]any{
			"consolidation_run_id":  id,
			"tenant_id":             tenantID,
			"group_legal_entity_id": groupLegalEntityID,
			"fiscal_period":         fiscalPeriod,
			"status":                status,
			"exception_count":       exceptionCount,
			"snapshot_count":        snapshotCount,
			"completed_at":          completedAt,
		})
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
// no DRAFT resting state is reachable via this v1's API. Consolidation-
// AdjustmentCreated is enqueued in the same transaction as the insert.
func (s *PgStore) CreateAdjustment(ctx context.Context, a *domain.ConsolidationAdjustment, correlationID string) error {
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
		if err != nil {
			return err
		}
		if err := s.appendAdjustmentTransition(ctx, tx, tenantID, a.ConsolidationAdjustmentID,
			nil, a.Status, a.CreatedByPrincipalID, correlationID, a.CreatedAt, nil,
			map[string]any{"adjustment_type": a.AdjustmentType}); err != nil {
			return err
		}
		return s.enqueueAdjustmentEvent(ctx, tx, "created", "ConsolidationAdjustmentCreated",
			tenantID, a.GroupLegalEntityID, a.ConsolidationAdjustmentID,
			a.CreatedByPrincipalID, correlationID, map[string]any{
				"consolidation_adjustment_id": a.ConsolidationAdjustmentID,
				"tenant_id":                   tenantID,
				"group_legal_entity_id":       a.GroupLegalEntityID,
				"fiscal_period":               a.FiscalPeriod,
				"adjustment_type":             a.AdjustmentType,
				"status":                      a.Status,
				"created_at":                  a.CreatedAt,
				"created_by_principal_id":     a.CreatedByPrincipalID,
			})
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
func (s *PgStore) ApproveAdjustment(ctx context.Context, id, principalID, correlationID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		var groupLegalEntityID, fiscalPeriod string
		err := tx.QueryRow(ctx, `
			UPDATE consolidation_adjustments
			SET status = $1, approved_at = $2, approved_by_principal_id = $3
			WHERE consolidation_adjustment_id = $4 AND tenant_id = $5 AND status = $6
			RETURNING group_legal_entity_id, fiscal_period
		`, domain.AdjustmentStatusApproved, now, principalID, id, tenantID, domain.AdjustmentStatusPendingApproval).
			Scan(&groupLegalEntityID, &fiscalPeriod)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidAdjustmentTransition
		}
		if err != nil {
			return err
		}
		from := domain.AdjustmentStatusPendingApproval
		if err := s.appendAdjustmentTransition(ctx, tx, tenantID, id, &from,
			domain.AdjustmentStatusApproved, principalID, correlationID, now, nil, nil); err != nil {
			return err
		}
		return s.enqueueAdjustmentEvent(ctx, tx, "approved", "ConsolidationAdjustmentApproved",
			tenantID, groupLegalEntityID, id, principalID, correlationID, map[string]any{
				"consolidation_adjustment_id": id,
				"tenant_id":                   tenantID,
				"group_legal_entity_id":       groupLegalEntityID,
				"fiscal_period":               fiscalPeriod,
				"status":                      domain.AdjustmentStatusApproved,
				"approved_at":                 now,
				"approved_by_principal_id":    principalID,
			})
	})
}

// MarkAdjustmentPosted moves APPROVED -> POSTED, recording the real
// consolidation-book journal general-ledger-svc actually created.
func (s *PgStore) MarkAdjustmentPosted(ctx context.Context, id, principalID, journalID, correlationID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		var groupLegalEntityID, fiscalPeriod string
		err := tx.QueryRow(ctx, `
			UPDATE consolidation_adjustments
			SET status = $1, posted_at = $2, posted_by_principal_id = $3, consolidation_book_journal_id = $4
			WHERE consolidation_adjustment_id = $5 AND tenant_id = $6 AND status = $7
			RETURNING group_legal_entity_id, fiscal_period
		`, domain.AdjustmentStatusPosted, now, principalID, journalID, id, tenantID, domain.AdjustmentStatusApproved).
			Scan(&groupLegalEntityID, &fiscalPeriod)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidAdjustmentTransition
		}
		if err != nil {
			return err
		}
		from := domain.AdjustmentStatusApproved
		if err := s.appendAdjustmentTransition(ctx, tx, tenantID, id, &from,
			domain.AdjustmentStatusPosted, principalID, correlationID, now, nil,
			map[string]any{"consolidation_book_journal_id": journalID}); err != nil {
			return err
		}
		return s.enqueueAdjustmentEvent(ctx, tx, "posted", "ConsolidationAdjustmentPosted",
			tenantID, groupLegalEntityID, id, principalID, correlationID, map[string]any{
				"consolidation_adjustment_id":   id,
				"tenant_id":                     tenantID,
				"group_legal_entity_id":         groupLegalEntityID,
				"fiscal_period":                 fiscalPeriod,
				"status":                        domain.AdjustmentStatusPosted,
				"posted_at":                     now,
				"posted_by_principal_id":        principalID,
				"consolidation_book_journal_id": journalID,
			})
	})
}

// ReverseAdjustment moves POSTED -> REVERSED, optionally recording the
// replacement adjustment that supersedes it.
func (s *PgStore) ReverseAdjustment(ctx context.Context, id, principalID, reason string, supersededBy *string, correlationID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		var groupLegalEntityID, fiscalPeriod string
		err := tx.QueryRow(ctx, `
			UPDATE consolidation_adjustments
			SET status = $1, reversed_at = $2, reversed_by_principal_id = $3, reversal_reason = $4, superseded_by_adjustment_id = $5
			WHERE consolidation_adjustment_id = $6 AND tenant_id = $7 AND status = $8
			RETURNING group_legal_entity_id, fiscal_period
		`, domain.AdjustmentStatusReversed, now, principalID, reason, supersededBy, id, tenantID, domain.AdjustmentStatusPosted).
			Scan(&groupLegalEntityID, &fiscalPeriod)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidAdjustmentTransition
		}
		if err != nil {
			return err
		}
		from := domain.AdjustmentStatusPosted
		if err := s.appendAdjustmentTransition(ctx, tx, tenantID, id, &from,
			domain.AdjustmentStatusReversed, principalID, correlationID, now, &reason,
			map[string]any{"superseded_by_adjustment_id": supersededBy}); err != nil {
			return err
		}
		return s.enqueueAdjustmentEvent(ctx, tx, "reversed", "ConsolidationAdjustmentReversed",
			tenantID, groupLegalEntityID, id, principalID, correlationID, map[string]any{
				"consolidation_adjustment_id": id,
				"tenant_id":                   tenantID,
				"group_legal_entity_id":       groupLegalEntityID,
				"fiscal_period":               fiscalPeriod,
				"status":                      domain.AdjustmentStatusReversed,
				"reversed_at":                 now,
				"reversed_by_principal_id":    principalID,
				"reversal_reason":             reason,
				"superseded_by_adjustment_id": supersededBy,
			})
	})
}

// GetAdjustmentHistory returns the adjustment's append-only lifecycle. The
// transition table is the record; rows that predate migration 000006 (or were
// written by an old instance during a rolling deploy after the backfill) are
// synthesized from the lifecycle columns on the adjustment itself, so the
// answer is always the full chain rather than an empty one.
func (s *PgStore) GetAdjustmentHistory(ctx context.Context, id string) (*domain.AdjustmentHistory, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var a domain.ConsolidationAdjustment
	out := &domain.AdjustmentHistory{Transitions: []domain.AdjustmentTransition{}}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanConsolidationAdjustment(tx.QueryRow(ctx, `
			SELECT `+consolidationAdjustmentColumns+`
			FROM consolidation_adjustments
			WHERE consolidation_adjustment_id = $1 AND tenant_id = $2
		`, id, tenantID), &a); err != nil {
			return err
		}

		out.ConsolidationAdjustmentID = a.ConsolidationAdjustmentID
		out.GroupLegalEntityID = a.GroupLegalEntityID
		out.FiscalPeriod = a.FiscalPeriod
		out.AdjustmentType = a.AdjustmentType
		out.Status = a.Status

		rows, err := tx.Query(ctx, `
			SELECT transition_id, consolidation_adjustment_id, sequence, from_status, to_status,
			       actor_principal_id, occurred_at, COALESCE(correlation_id, ''), reason, details
			FROM consolidation_adjustment_transitions
			WHERE consolidation_adjustment_id = $1 AND tenant_id = $2
			ORDER BY sequence ASC
		`, id, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var t domain.AdjustmentTransition
			var details []byte
			if err := rows.Scan(&t.TransitionID, &t.ConsolidationAdjustmentID, &t.Sequence,
				&t.FromStatus, &t.ToStatus, &t.ActorPrincipalID, &t.OccurredAt,
				&t.CorrelationID, &t.Reason, &details); err != nil {
				return err
			}
			if len(details) > 0 {
				if err := json.Unmarshal(details, &t.Details); err != nil {
					return fmt.Errorf("read adjustment transition %s details: %w", t.TransitionID, err)
				}
			}
			out.Transitions = append(out.Transitions, t)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out.Transitions) == 0 {
			out.Transitions = synthesizeAdjustmentTransitions(&a)
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAdjustmentNotFound
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// synthesizeAdjustmentTransitions rebuilds the lifecycle chain from the
// adjustment row for an adjustment whose transitions were never written
// (created before migration 000006). It mirrors exactly what the commands
// would have recorded, from the same columns.
func synthesizeAdjustmentTransitions(a *domain.ConsolidationAdjustment) []domain.AdjustmentTransition {
	out := make([]domain.AdjustmentTransition, 0, 4)
	var seq int64
	add := func(from *string, to, actor string, at time.Time, reason *string, details map[string]any) {
		seq++
		out = append(out, domain.AdjustmentTransition{
			TransitionID:              uuid.NewString(),
			ConsolidationAdjustmentID: a.ConsolidationAdjustmentID,
			Sequence:                  seq,
			FromStatus:                from,
			ToStatus:                  to,
			ActorPrincipalID:          actor,
			OccurredAt:                at,
			Reason:                    reason,
			Details:                   details,
		})
	}

	add(nil, domain.AdjustmentStatusPendingApproval, a.CreatedByPrincipalID, a.CreatedAt, nil,
		map[string]any{"adjustment_type": a.AdjustmentType})

	if a.ApprovedAt != nil {
		from := domain.AdjustmentStatusPendingApproval
		actor := ""
		if a.ApprovedByPrincipalID != nil {
			actor = *a.ApprovedByPrincipalID
		}
		add(&from, domain.AdjustmentStatusApproved, actor, *a.ApprovedAt, nil, nil)
	}
	if a.PostedAt != nil {
		from := domain.AdjustmentStatusApproved
		actor := ""
		if a.PostedByPrincipalID != nil {
			actor = *a.PostedByPrincipalID
		}
		details := map[string]any{}
		if a.ConsolidationBookJournalID != nil {
			details["consolidation_book_journal_id"] = *a.ConsolidationBookJournalID
		}
		add(&from, domain.AdjustmentStatusPosted, actor, *a.PostedAt, nil, details)
	}
	if a.ReversedAt != nil {
		from := domain.AdjustmentStatusPosted
		actor := ""
		if a.ReversedByPrincipalID != nil {
			actor = *a.ReversedByPrincipalID
		}
		details := map[string]any{}
		if a.SupersededByAdjustmentID != nil {
			details["superseded_by_adjustment_id"] = *a.SupersededByAdjustmentID
		}
		add(&from, domain.AdjustmentStatusReversed, actor, *a.ReversedAt, a.ReversalReason, details)
	}
	return out
}
