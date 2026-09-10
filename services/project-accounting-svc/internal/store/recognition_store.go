package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/project-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/project-accounting-svc/internal/middleware"
)

// ── Recognition estimates ────────────────────────────────────────────────────

// SetApprovedEstimate end-dates the current version (at the NEW version's
// own effective_from, never "now") and inserts the new one — the ONLY
// way an estimate's own value changes, the real enforcement of "Progress
// estimate changed after approval without invalidation."
func (s *PgStore) SetApprovedEstimate(ctx context.Context, newVersion *domain.RecognitionEstimate) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var currentVersion int
		var estimateID string
		err := tx.QueryRow(ctx, `
			UPDATE project_recognition_estimates SET effective_to = $1
			WHERE tenant_id = $2 AND project_id = $3 AND effective_to IS NULL
			RETURNING estimate_id, version
		`, newVersion.EffectiveFrom, tenantID, newVersion.ProjectID).Scan(&estimateID, &currentVersion)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if estimateID != "" {
			newVersion.EstimateID = estimateID
			newVersion.Version = currentVersion + 1
		} else {
			newVersion.EstimateID = uuidNewString()
			newVersion.Version = 1
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO project_recognition_estimates (
				estimate_version_id, estimate_id, version, tenant_id, project_id,
				estimate_to_complete, effective_from, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, newVersion.EstimateVersionID, newVersion.EstimateID, newVersion.Version, tenantID, newVersion.ProjectID,
			newVersion.EstimateToComplete, newVersion.EffectiveFrom, newVersion.CreatedAt, newVersion.CreatedByPrincipalID)
		return err
	})
}

func (s *PgStore) GetCurrentEstimate(ctx context.Context, projectID string) (*domain.RecognitionEstimate, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var e *domain.RecognitionEstimate
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT estimate_version_id, estimate_id, version, project_id, estimate_to_complete, effective_from, effective_to, created_at, created_by_principal_id
			FROM project_recognition_estimates WHERE tenant_id = $1 AND project_id = $2 AND effective_to IS NULL
		`, tenantID, projectID)
		var err error
		e, err = scanEstimate(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no estimate set yet — a real, valid state, not an error
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

func scanEstimate(row pgx.Row) (*domain.RecognitionEstimate, error) {
	var e domain.RecognitionEstimate
	if err := row.Scan(&e.EstimateVersionID, &e.EstimateID, &e.Version, &e.ProjectID, &e.EstimateToComplete, &e.EffectiveFrom, &e.EffectiveTo, &e.CreatedAt, &e.CreatedByPrincipalID); err != nil {
		return nil, err
	}
	return &e, nil
}

// ── Recognition runs ─────────────────────────────────────────────────────────

const recognitionRunColumns = `
	run_id, legal_entity_id, project_id, fiscal_period, status,
	contract_value, billed_to_date, estimate_to_complete, itd_cost_incurred,
	percent_complete, cumulative_recognized_revenue, period_recognized_revenue, recognized_cost, margin,
	balance_type, balance_amount, revenue_account_code, wip_account_code, journal_id,
	supersedes_run_id, superseded_by_run_id,
	created_at, created_by_principal_id, frozen_at, calculated_at, validated_at,
	approved_at, approved_by_principal_id, emitted_at, superseded_at, superseded_by_principal_id`

func scanRecognitionRun(row pgx.Row) (*domain.RecognitionRun, error) {
	var r domain.RecognitionRun
	var revenueAccountCode, wipAccountCode *string
	if err := row.Scan(
		&r.RunID, &r.LegalEntityID, &r.ProjectID, &r.FiscalPeriod, &r.Status,
		&r.ContractValue, &r.BilledToDate, &r.EstimateToComplete, &r.ITDCostIncurred,
		&r.PercentComplete, &r.CumulativeRecognizedRevenue, &r.PeriodRecognizedRevenue, &r.RecognizedCost, &r.Margin,
		&r.BalanceType, &r.BalanceAmount, &revenueAccountCode, &wipAccountCode, &r.JournalID,
		&r.SupersedesRunID, &r.SupersededByRunID,
		&r.CreatedAt, &r.CreatedByPrincipalID, &r.FrozenAt, &r.CalculatedAt, &r.ValidatedAt,
		&r.ApprovedAt, &r.ApprovedByPrincipalID, &r.EmittedAt, &r.SupersededAt, &r.SupersededByPrincipalID,
	); err != nil {
		return nil, err
	}
	if revenueAccountCode != nil {
		r.RevenueAccountCode = *revenueAccountCode
	}
	if wipAccountCode != nil {
		r.WIPAccountCode = *wipAccountCode
	}
	return &r, nil
}

// CreateRecognitionRun inserts a new run in DRAFT — refused by the real
// UNIQUE(tenant_id, project_id, fiscal_period) WHERE status != 'SUPERSEDED'
// constraint if a live run already exists for this period.
func (s *PgStore) CreateRecognitionRun(ctx context.Context, r *domain.RecognitionRun) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO project_recognition_runs (
				run_id, tenant_id, legal_entity_id, project_id, fiscal_period, status,
				contract_value, billed_to_date, revenue_account_code, wip_account_code,
				created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, r.RunID, tenantID, r.LegalEntityID, r.ProjectID, r.FiscalPeriod, r.Status,
			r.ContractValue, r.BilledToDate, nullIfEmpty(r.RevenueAccountCode), nullIfEmpty(r.WIPAccountCode),
			r.CreatedAt, r.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrRecognitionRunAlreadyExistsForPeriod
			}
			return err
		}
		return nil
	})
}

func (s *PgStore) GetRecognitionRun(ctx context.Context, runID string) (*domain.RecognitionRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var r *domain.RecognitionRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+recognitionRunColumns+` FROM project_recognition_runs WHERE run_id = $1 AND tenant_id = $2`, runID, tenantID)
		var err error
		r, err = scanRecognitionRun(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRecognitionRunNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// FreezeAndCalculate is CalculateProjectRevenue + CalculateProjectWIP,
// collapsed into one real step — see migration 000003's doc comment.
// Moves DRAFT -> CALCULATED, snapshotting the current approved estimate
// and the real, live-summed ITD cost from project_cost_entries, then
// performs the percentage-of-completion calculation. Everything it
// writes becomes immutable the moment this transaction commits (the
// row's own status leaves DRAFT/POPULATION_FROZEN), enforced by the
// reject-mutation trigger.
func (s *PgStore) FreezeAndCalculate(ctx context.Context, runID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var projectID string
		var contractValue *float64
		var billedToDate *float64
		err := tx.QueryRow(ctx, `
			SELECT project_id, contract_value, billed_to_date FROM project_recognition_runs
			WHERE run_id = $1 AND status = $2 AND tenant_id = $3
		`, runID, domain.RecognitionRunStatusDraft, tenantID).Scan(&projectID, &contractValue, &billedToDate)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidRecognitionRunTransition
		}
		if err != nil {
			return err
		}
		if contractValue == nil || *contractValue <= 0 {
			return domain.ErrContractValueRequired
		}

		var estimateToComplete float64
		err = tx.QueryRow(ctx, `
			SELECT estimate_to_complete FROM project_recognition_estimates
			WHERE tenant_id = $1 AND project_id = $2 AND effective_to IS NULL
		`, tenantID, projectID).Scan(&estimateToComplete)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrApprovedEstimateRequired
		}
		if err != nil {
			return err
		}

		var itdCost float64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount), 0) FROM project_cost_entries WHERE tenant_id = $1 AND project_id = $2 AND status != 'REVERSED'
		`, tenantID, projectID).Scan(&itdCost); err != nil {
			return err
		}

		var priorCumulative float64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(cumulative_recognized_revenue), 0) FROM project_recognition_runs
			WHERE tenant_id = $1 AND project_id = $2 AND status NOT IN ($3, $4, $5)
		`, tenantID, projectID, domain.RecognitionRunStatusDraft, domain.RecognitionRunStatusPopulationFrozen, domain.RecognitionRunStatusSuperseded).Scan(&priorCumulative); err != nil {
			return err
		}

		denominator := itdCost + estimateToComplete
		var percentComplete float64
		if denominator > 0 {
			percentComplete = itdCost / denominator
		}
		cumulativeRevenue := roundCents(percentComplete * *contractValue)
		periodRevenue := roundCents(cumulativeRevenue - priorCumulative)
		recognizedCost := roundCents(itdCost)
		margin := roundCents(cumulativeRevenue - recognizedCost)

		billed := 0.0
		if billedToDate != nil {
			billed = *billedToDate
		}
		balanceAmount := roundCents(cumulativeRevenue - billed)
		balanceType := domain.BalanceTypeNone
		if balanceAmount > 0 {
			balanceType = domain.BalanceTypeContractAsset
		} else if balanceAmount < 0 {
			balanceType = domain.BalanceTypeContractLiability
		}

		tag, err := tx.Exec(ctx, `
			UPDATE project_recognition_runs SET
				status = $1, frozen_at = $2, calculated_at = $2,
				estimate_to_complete = $3, itd_cost_incurred = $4, percent_complete = $5,
				cumulative_recognized_revenue = $6, period_recognized_revenue = $7, recognized_cost = $8, margin = $9,
				balance_type = $10, balance_amount = $11
			WHERE run_id = $12 AND status = $13 AND tenant_id = $14
		`, domain.RecognitionRunStatusCalculated, at, estimateToComplete, itdCost, percentComplete,
			cumulativeRevenue, periodRevenue, recognizedCost, margin,
			balanceType, balanceAmount, runID, domain.RecognitionRunStatusDraft, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidRecognitionRunTransition
		}
		return nil
	})
}

func (s *PgStore) transitionRecognitionRun(ctx context.Context, runID, fromStatus, toStatus, extraSet string, extraArgs ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		args := append([]any{toStatus}, extraArgs...)
		args = append(args, runID, fromStatus, tenantID)
		query := fmt.Sprintf(`
			UPDATE project_recognition_runs SET status = $1%s
			WHERE run_id = $%d AND status = $%d AND tenant_id = $%d
		`, extraSet, len(args)-2, len(args)-1, len(args))
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidRecognitionRunTransition
		}
		return nil
	})
}

func (s *PgStore) ValidateRecognitionRun(ctx context.Context, runID string, at time.Time) error {
	return s.transitionRecognitionRun(ctx, runID, domain.RecognitionRunStatusCalculated, domain.RecognitionRunStatusReviewed,
		", validated_at = $2", at)
}

func (s *PgStore) ApproveRecognitionRun(ctx context.Context, runID, principalID string, at time.Time) error {
	return s.transitionRecognitionRun(ctx, runID, domain.RecognitionRunStatusReviewed, domain.RecognitionRunStatusApproved,
		", approved_at = $2, approved_by_principal_id = $3", at, principalID)
}

func (s *PgStore) MarkRecognitionRunEmitted(ctx context.Context, runID, journalID string, at time.Time) error {
	return s.transitionRecognitionRun(ctx, runID, domain.RecognitionRunStatusApproved, domain.RecognitionRunStatusAccountingEventEmitted,
		", emitted_at = $2, journal_id = $3", at, journalID)
}

// SupersedeRecognitionRun marks runID SUPERSEDED, releasing the (project,
// fiscal_period) slot the partial UNIQUE index otherwise holds — mirrors
// AST-02's own SupersedeDepreciationRun and INV-04's own
// MarkValuationRunEmitted-adjacent pattern exactly.
func (s *PgStore) SupersedeRecognitionRun(ctx context.Context, runID, principalID string, at time.Time) error {
	return s.transitionRecognitionRun(ctx, runID, domain.RecognitionRunStatusAccountingEventEmitted, domain.RecognitionRunStatusSuperseded,
		", superseded_at = $2, superseded_by_principal_id = $3", at, principalID)
}

// GetPostedRevenueTotal is the AST/INV/PRJ domain spec's own §9 "Project
// revenue/WIP → GL" assertion (verbatim): "Certified recognition/WIP run
// reconciles to posted revenue/contract balance/WIP accounts and AR
// billing separately." A real GL balance comparison, the same shape as
// Assets → GL and Inventory value → GL: SUM(period_recognized_revenue)
// across every run that has actually reached ACCOUNTING_EVENT_EMITTED
// (and has not since been SUPERSEDED) for this legal entity and fiscal
// period — the real revenue this service itself posted through
// general-ledger-svc, never a run that merely calculated a figure but
// never emitted it.
func (s *PgStore) GetPostedRevenueTotal(ctx context.Context, legalEntityID, fiscalPeriod string) (float64, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}
	var total float64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(period_recognized_revenue), 0) FROM project_recognition_runs
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_period = $3 AND status = $4
		`, tenantID, legalEntityID, fiscalPeriod, domain.RecognitionRunStatusAccountingEventEmitted).Scan(&total)
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
