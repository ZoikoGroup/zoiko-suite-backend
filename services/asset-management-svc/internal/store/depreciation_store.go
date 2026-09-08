package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/asset-management-svc/internal/domain"
	svcmiddleware "zoiko.io/asset-management-svc/internal/middleware"
)

// ── Depreciation schedules ───────────────────────────────────────────────────

const depreciationScheduleColumns = `
	schedule_version_id, schedule_id, version, tenant_id, legal_entity_id, asset_id, book_id,
	method, cost_basis, residual_value, useful_life_months, in_service_date, status, effective_to,
	created_at, created_by_principal_id`

func scanDepreciationSchedule(row pgx.Row) (*domain.DepreciationSchedule, error) {
	var s domain.DepreciationSchedule
	if err := row.Scan(
		&s.ScheduleVersionID, &s.ScheduleID, &s.Version, &s.TenantID, &s.LegalEntityID, &s.AssetID, &s.BookID,
		&s.Method, &s.CostBasis, &s.ResidualValue, &s.UsefulLifeMonths, &s.InServiceDate, &s.Status, &s.EffectiveTo,
		&s.CreatedAt, &s.CreatedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateDepreciationSchedule inserts a brand-new schedule (version 1,
// ACTIVE) — refused by the real UNIQUE(tenant_id, asset_id, book_id)
// WHERE effective_to IS NULL constraint if a current schedule already
// exists for this asset/book, the spec's own negative path, "Same asset
// depreciated twice in period," enforced at its root.
func (s *PgStore) CreateDepreciationSchedule(ctx context.Context, sch *domain.DepreciationSchedule) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO depreciation_schedules (
				schedule_version_id, schedule_id, version, tenant_id, legal_entity_id, asset_id, book_id,
				method, cost_basis, residual_value, useful_life_months, in_service_date, status,
				created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		`, sch.ScheduleVersionID, sch.ScheduleID, sch.Version, tenantID, sch.LegalEntityID, sch.AssetID, sch.BookID,
			sch.Method, sch.CostBasis, sch.ResidualValue, sch.UsefulLifeMonths, sch.InServiceDate, sch.Status,
			sch.CreatedAt, sch.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrDuplicateScheduleForAssetBook
			}
			return err
		}
		return nil
	})
}

func (s *PgStore) GetCurrentDepreciationSchedule(ctx context.Context, scheduleID string) (*domain.DepreciationSchedule, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var sch *domain.DepreciationSchedule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+depreciationScheduleColumns+` FROM depreciation_schedules WHERE tenant_id = $1 AND schedule_id = $2 AND effective_to IS NULL`, tenantID, scheduleID)
		var err error
		sch, err = scanDepreciationSchedule(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrScheduleNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return sch, nil
}

// RecalculateSchedule end-dates the current version and inserts a new
// one — the ONLY way a schedule's own parameters change, the real
// enforcement of "Useful life changed after approval without
// invalidation."
func (s *PgStore) RecalculateSchedule(ctx context.Context, scheduleID string, newVersion *domain.DepreciationSchedule, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var currentVersion int
		err := tx.QueryRow(ctx, `
			UPDATE depreciation_schedules SET status = $1, effective_to = $2
			WHERE tenant_id = $3 AND schedule_id = $4 AND effective_to IS NULL AND status = $5
			RETURNING version
		`, domain.DepreciationScheduleStatusSuperseded, at, tenantID, scheduleID, domain.DepreciationScheduleStatusActive).Scan(&currentVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrScheduleNotFound
		}
		if err != nil {
			return err
		}
		newVersion.Version = currentVersion + 1
		_, err = tx.Exec(ctx, `
			INSERT INTO depreciation_schedules (
				schedule_version_id, schedule_id, version, tenant_id, legal_entity_id, asset_id, book_id,
				method, cost_basis, residual_value, useful_life_months, in_service_date, status,
				created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		`, newVersion.ScheduleVersionID, scheduleID, newVersion.Version, tenantID, newVersion.LegalEntityID, newVersion.AssetID, newVersion.BookID,
			newVersion.Method, newVersion.CostBasis, newVersion.ResidualValue, newVersion.UsefulLifeMonths, newVersion.InServiceDate,
			domain.DepreciationScheduleStatusActive, newVersion.CreatedAt, newVersion.CreatedByPrincipalID)
		return err
	})
}

// ── Depreciation runs ─────────────────────────────────────────────────────────

const depreciationRunColumns = `
	run_id, tenant_id, legal_entity_id, fiscal_period,
	depreciation_expense_account_code, accumulated_depreciation_account_code,
	status, journal_id, supersedes_run_id, superseded_by_run_id,
	created_at, created_by_principal_id, frozen_at, validated_at,
	approved_at, approved_by_principal_id, emitted_at, superseded_at, superseded_by_principal_id`

func scanDepreciationRun(row pgx.Row) (*domain.DepreciationRun, error) {
	var r domain.DepreciationRun
	if err := row.Scan(
		&r.RunID, &r.TenantID, &r.LegalEntityID, &r.FiscalPeriod,
		&r.DepreciationExpenseAccountCode, &r.AccumulatedDepreciationAccountCode,
		&r.Status, &r.JournalID, &r.SupersedesRunID, &r.SupersededByRunID,
		&r.CreatedAt, &r.CreatedByPrincipalID, &r.FrozenAt, &r.ValidatedAt,
		&r.ApprovedAt, &r.ApprovedByPrincipalID, &r.EmittedAt, &r.SupersededAt, &r.SupersededByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateDepreciationRun inserts a new run in DRAFT — refused by the real
// UNIQUE(tenant_id, legal_entity_id, fiscal_period) WHERE status !=
// 'SUPERSEDED' constraint if a live run already exists for this period.
func (s *PgStore) CreateDepreciationRun(ctx context.Context, r *domain.DepreciationRun) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO depreciation_runs (
				run_id, tenant_id, legal_entity_id, fiscal_period,
				depreciation_expense_account_code, accumulated_depreciation_account_code,
				status, supersedes_run_id, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, r.RunID, tenantID, r.LegalEntityID, r.FiscalPeriod,
			r.DepreciationExpenseAccountCode, r.AccumulatedDepreciationAccountCode,
			r.Status, r.SupersedesRunID, r.CreatedAt, r.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrRunAlreadyExistsForPeriod
			}
			return err
		}
		return nil
	})
}

func (s *PgStore) GetDepreciationRun(ctx context.Context, runID string) (*domain.DepreciationRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var r *domain.DepreciationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+depreciationRunColumns+` FROM depreciation_runs WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID)
		var err error
		r, err = scanDepreciationRun(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRunNotFound
		}
		if err != nil {
			return err
		}
		lines, err := s.listDepreciationLines(ctx, tx, tenantID, runID)
		r.Lines = lines
		return err
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s *PgStore) listDepreciationLines(ctx context.Context, tx pgx.Tx, tenantID, runID string) ([]domain.DepreciationLine, error) {
	rows, err := tx.Query(ctx, `
		SELECT line_id, run_id, schedule_version_id, asset_id, book_id, period_amount, accumulated_depreciation_after, created_at
		FROM depreciation_lines WHERE tenant_id = $1 AND run_id = $2 ORDER BY created_at
	`, tenantID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DepreciationLine
	for rows.Next() {
		var l domain.DepreciationLine
		if err := rows.Scan(&l.LineID, &l.RunID, &l.ScheduleVersionID, &l.AssetID, &l.BookID, &l.PeriodAmount, &l.AccumulatedDepreciationAfter, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *PgStore) transitionRun(ctx context.Context, runID, fromStatus, toStatus, extraSet string, extraArgs ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		args := append([]any{toStatus}, extraArgs...)
		args = append(args, runID, fromStatus, tenantID)
		query := fmt.Sprintf(`
			UPDATE depreciation_runs SET status = $1%s
			WHERE run_id = $%d AND status = $%d AND tenant_id = $%d
		`, extraSet, len(args)-2, len(args)-1, len(args))
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidRunTransition
		}
		return nil
	})
}

// FreezeDepreciationPopulation moves DRAFT -> POPULATION_FROZEN, and
// records the frozen population manifest — every CURRENT ACTIVE
// depreciation schedule for the run's own legal_entity_id, as it exists
// at the moment of freezing, never re-queried live afterward.
func (s *PgStore) FreezeDepreciationPopulation(ctx context.Context, runID, legalEntityID string, at time.Time) (frozenCount int, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE depreciation_runs SET status = $1, frozen_at = $2
			WHERE run_id = $3 AND status = $4 AND tenant_id = $5
		`, domain.DepreciationRunStatusPopulationFrozen, at, runID, domain.DepreciationRunStatusDraft, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidRunTransition
		}

		// Eligible population: every asset's CURRENT ACTIVE schedule, for
		// an asset that is itself ACTIVE (excludes SUSPENDED/MERGED/
		// CANDIDATE/REGISTERED today, and will automatically exclude a
		// future AST-03 DISPOSED status too, without any change here — see
		// migration 000002's own doc comment).
		insertTag, err := tx.Exec(ctx, `
			INSERT INTO depreciation_run_population (run_id, schedule_version_id)
			SELECT $1, ds.schedule_version_id
			FROM depreciation_schedules ds
			JOIN fixed_assets fa ON fa.asset_id = ds.asset_id
			WHERE ds.tenant_id = $2 AND ds.legal_entity_id = $3 AND ds.effective_to IS NULL
			  AND ds.status = $4 AND fa.status = $5
		`, runID, tenantID, legalEntityID, domain.DepreciationScheduleStatusActive, domain.AssetStatusActive)
		if err != nil {
			return err
		}
		frozenCount = int(insertTag.RowsAffected())
		return nil
	})
	return frozenCount, err
}

// ValidateDepreciationRun performs the actual calculation (straight-line,
// one line per frozen schedule version) AND validates it in the same
// step — collapsing Run's own Calculated state, see migration 000002's
// doc comment. Idempotent: refuses (via the guarded status transition)
// to recalculate a run already past POPULATION_FROZEN.
func (s *PgStore) ValidateDepreciationRun(ctx context.Context, runID string, at time.Time) (lineCount int, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE depreciation_runs SET status = $1, validated_at = $2
			WHERE run_id = $3 AND status = $4 AND tenant_id = $5
		`, domain.DepreciationRunStatusValidated, at, runID, domain.DepreciationRunStatusPopulationFrozen, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidRunTransition
		}

		rows, err := tx.Query(ctx, `
			SELECT ds.schedule_version_id, ds.asset_id, ds.book_id, ds.cost_basis, ds.residual_value, ds.useful_life_months
			FROM depreciation_run_population rp
			JOIN depreciation_schedules ds ON ds.schedule_version_id = rp.schedule_version_id
			WHERE rp.run_id = $1
		`, runID)
		if err != nil {
			return err
		}
		type frozenSchedule struct {
			scheduleVersionID, assetID, bookID string
			costBasis, residualValue           float64
			usefulLifeMonths                   int
		}
		var scheds []frozenSchedule
		for rows.Next() {
			var fs frozenSchedule
			if err := rows.Scan(&fs.scheduleVersionID, &fs.assetID, &fs.bookID, &fs.costBasis, &fs.residualValue, &fs.usefulLifeMonths); err != nil {
				rows.Close()
				return err
			}
			scheds = append(scheds, fs)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, fs := range scheds {
			// Straight-line monthly amount, capped at (cost - residual)
			// total accumulated — a schedule already fully depreciated
			// contributes a zero line, never a negative one.
			monthly := roundCents((fs.costBasis - fs.residualValue) / float64(fs.usefulLifeMonths))
			var priorAccumulated float64
			if err := tx.QueryRow(ctx, `
				SELECT COALESCE(SUM(dl.period_amount), 0)
				FROM depreciation_lines dl
				WHERE dl.tenant_id = $1 AND dl.schedule_version_id = $2
			`, tenantID, fs.scheduleVersionID).Scan(&priorAccumulated); err != nil {
				return err
			}
			remaining := roundCents(fs.costBasis - fs.residualValue - priorAccumulated)
			amount := monthly
			if amount > remaining {
				amount = remaining
			}
			if amount < 0 {
				amount = 0
			}
			accumulatedAfter := roundCents(priorAccumulated + amount)

			if _, err := tx.Exec(ctx, `
				INSERT INTO depreciation_lines (
					line_id, tenant_id, run_id, schedule_version_id, asset_id, book_id,
					period_amount, accumulated_depreciation_after, created_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			`, newUUID(), tenantID, runID, fs.scheduleVersionID, fs.assetID, fs.bookID, amount, accumulatedAfter, at); err != nil {
				return err
			}
			lineCount++
		}
		return nil
	})
	return lineCount, err
}

func (s *PgStore) ApproveDepreciationRun(ctx context.Context, runID, principalID string, at time.Time) error {
	return s.transitionRun(ctx, runID, domain.DepreciationRunStatusValidated, domain.DepreciationRunStatusApproved,
		", approved_at = $2, approved_by_principal_id = $3", at, principalID)
}

func (s *PgStore) MarkDepreciationRunEmitted(ctx context.Context, runID, journalID string, at time.Time) error {
	return s.transitionRun(ctx, runID, domain.DepreciationRunStatusApproved, domain.DepreciationRunStatusAccountingEventEmitted,
		", emitted_at = $2, journal_id = $3", at, journalID)
}

// SupersedeDepreciationRun marks runID SUPERSEDED and links newRunID (if
// given) — the real, database-enforced release of the (entity, period)
// slot the partial UNIQUE index otherwise holds.
func (s *PgStore) SupersedeDepreciationRun(ctx context.Context, runID, principalID string, at time.Time) error {
	return s.transitionRun(ctx, runID, domain.DepreciationRunStatusAccountingEventEmitted, domain.DepreciationRunStatusSuperseded,
		", superseded_at = $2, superseded_by_principal_id = $3", at, principalID)
}

func newUUID() string {
	return uuidNewString()
}
