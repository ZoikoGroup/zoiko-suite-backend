package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

// mapPgError translates the Postgres failures that are really caller mistakes
// into domain errors, so they stop arriving at the handler as "the store is
// unavailable".
//
// fiscal_period_id is a uuid column, so a mistyped id compared against it dies
// inside the driver as SQLSTATE 22P02 before any row is examined — and used to
// reach the caller as 503 store_unavailable, a status that sends an operator to
// look at infrastructure over what is a typo in a URL. A period id that cannot
// be a UUID names no period, which is exactly what "not found" means. Same fix,
// same reasoning, as accounts-payable-svc and general-ledger-svc.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return domain.ErrFiscalPeriodNotFound
	}
	return err
}

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// withRLS runs a query block under the specified tenant RLS context.
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// Inject tenant context into the session configuration
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

// CreateFiscalPeriod inserts a fiscal period header in OPEN status.
//
// Idempotent on the existing (tenant_id, legal_entity_id, period_name)
// unique constraint: a retried call (e.g. a client timeout on a POST that
// actually succeeded server-side) resolves to the ORIGINAL period — mutating
// *fp in place to reflect it — rather than erroring out as a 503 (the
// previous behavior: any constraint violation was reported identically to
// a genuine store outage). Returns created=false when the row already
// existed. Two distinct periods can never legitimately share a name for
// the same legal entity, so this collision is always a safe one to treat
// as a replay.
func (s *PgStore) CreateFiscalPeriod(ctx context.Context, fp *domain.FiscalPeriod) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO fiscal_periods (
				fiscal_period_id, tenant_id, legal_entity_id, period_name, period_start, period_end, close_status
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, legal_entity_id, period_name) DO NOTHING
		`, fp.FiscalPeriodID, tenantID, fp.LegalEntityID, fp.PeriodName, fp.PeriodStart, fp.PeriodEnd, fp.CloseStatus)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT fiscal_period_id, period_start, period_end, close_status, close_locked_at, evidence_document_id
				FROM fiscal_periods WHERE tenant_id = $1 AND legal_entity_id = $2 AND period_name = $3
			`, tenantID, fp.LegalEntityID, fp.PeriodName)
			if err := row.Scan(&fp.FiscalPeriodID, &fp.PeriodStart, &fp.PeriodEnd, &fp.CloseStatus, &fp.CloseLockedAt, &fp.EvidenceDocumentID); err != nil {
				return err
			}
			created = false
			return nil
		}
		created = true
		return nil
	})
	return created, err
}

func (s *PgStore) GetFiscalPeriod(ctx context.Context, id string) (*domain.FiscalPeriod, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var fp domain.FiscalPeriod
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT fiscal_period_id, tenant_id, legal_entity_id, period_name, period_start, period_end, close_status, close_locked_at, evidence_document_id
			FROM fiscal_periods WHERE fiscal_period_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&fp.FiscalPeriodID, &fp.TenantID, &fp.LegalEntityID, &fp.PeriodName, &fp.PeriodStart, &fp.PeriodEnd, &fp.CloseStatus, &fp.CloseLockedAt, &fp.EvidenceDocumentID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFiscalPeriodNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &fp, nil
}

func (s *PgStore) GetFiscalPeriodByName(ctx context.Context, legalEntityID, name string) (*domain.FiscalPeriod, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var fp domain.FiscalPeriod
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT fiscal_period_id, tenant_id, legal_entity_id, period_name, period_start, period_end, close_status, close_locked_at, evidence_document_id
			FROM fiscal_periods WHERE legal_entity_id = $1 AND period_name = $2 AND tenant_id = $3
		`, legalEntityID, name, tenantID).Scan(
			&fp.FiscalPeriodID, &fp.TenantID, &fp.LegalEntityID, &fp.PeriodName, &fp.PeriodStart, &fp.PeriodEnd, &fp.CloseStatus, &fp.CloseLockedAt, &fp.EvidenceDocumentID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFiscalPeriodNotFound
	}
	if err != nil {
		return nil, err
	}
	return &fp, nil
}

func (s *PgStore) ListFiscalPeriods(ctx context.Context, legalEntityID string) ([]domain.FiscalPeriod, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.FiscalPeriod
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT fiscal_period_id, tenant_id, legal_entity_id, period_name, period_start, period_end, close_status, close_locked_at, evidence_document_id
			FROM fiscal_periods WHERE legal_entity_id = $1 AND tenant_id = $2
			ORDER BY period_start DESC
		`, legalEntityID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var fp domain.FiscalPeriod
			if err := rows.Scan(
				&fp.FiscalPeriodID, &fp.TenantID, &fp.LegalEntityID, &fp.PeriodName, &fp.PeriodStart, &fp.PeriodEnd, &fp.CloseStatus, &fp.CloseLockedAt, &fp.EvidenceDocumentID,
			); err != nil {
				return err
			}
			out = append(out, fp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) LockFiscalPeriod(ctx context.Context, id string, lockedAt time.Time, evidenceDocID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE fiscal_periods
			SET close_status = 'LOCKED', close_locked_at = $1, evidence_document_id = $2
			WHERE fiscal_period_id = $3 AND tenant_id = $4 AND close_status = 'OPEN'
		`, lockedAt, evidenceDocID, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrPeriodAlreadyLocked
		}
		return nil
	})
	// A malformed id names no period, so nothing was locked. Reported as absent
	// rather than as a dead store, and never as success.
	return mapPgError(err)
}

// ReopenFiscalPeriod transitions id from LOCKED back to OPEN, atomically and
// only from LOCKED — the WHERE guard mirrors LockFiscalPeriod's own
// OPEN-only guard, so a period that isn't currently LOCKED (already OPEN,
// or somehow both requests raced) reopens nothing rather than corrupting
// state. evidence_document_id is cleared: the PRIOR close's own
// CloseEvidence row is untouched in close_evidences and stays permanently
// queryable by fiscal_period_id; only the pointer on the period itself is
// reset, since the next close will produce a new evidence document.
func (s *PgStore) ReopenFiscalPeriod(ctx context.Context, id string, reopenedAt time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE fiscal_periods
			SET close_status = 'OPEN', close_locked_at = NULL, evidence_document_id = NULL
			WHERE fiscal_period_id = $1 AND tenant_id = $2 AND close_status = 'LOCKED'
		`, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrPeriodNotLocked
		}
		return nil
	})
	return mapPgError(err)
}

// CreateReopenEvent inserts one permanent, append-only reopen record — a
// database trigger (migration 000003) rejects any UPDATE/DELETE on this
// table at the schema level, not just in application code.
func (s *PgStore) CreateReopenEvent(ctx context.Context, event *domain.PeriodReopenEvent) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO period_reopen_events (
				reopen_event_id, tenant_id, fiscal_period_id, reason, reopened_by_principal_id, reopened_at
			) VALUES ($1, $2, $3, $4, $5, $6)
		`, event.ReopenEventID, tenantID, event.FiscalPeriodID, event.Reason, event.ReopenedByPrincipalID, event.ReopenedAt)
		return err
	})
}

func (s *PgStore) CreateCloseEvidence(ctx context.Context, evidence *domain.CloseEvidence) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO close_evidences (
				evidence_id, tenant_id, fiscal_period_id, trial_balance_hash, signature, generated_at
			) VALUES ($1, $2, $3, $4, $5, $6)
		`, evidence.EvidenceID, tenantID, evidence.FiscalPeriodID, evidence.TrialBalanceHash, evidence.Signature, evidence.GeneratedAt)
		return err
	})
}

// CreateControlRun persists one ACC-06 subledger-to-GL reconciliation
// result — append-only, see migration 000004's doc comment.
func (s *PgStore) CreateControlRun(ctx context.Context, run *domain.SubledgerControlRun) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO subledger_control_runs (
				control_run_id, tenant_id, legal_entity_id, fiscal_period, subledger,
				control_account_code, subledger_total_amount, gl_control_balance_amount,
				difference_amount, status, run_at, run_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, run.ControlRunID, tenantID, run.LegalEntityID, run.FiscalPeriod, run.Subledger,
			run.ControlAccountCode, run.SubledgerTotalAmount, run.GLControlBalanceAmount,
			run.DifferenceAmount, run.Status, run.RunAt, run.RunByPrincipalID)
		return err
	})
}

func (s *PgStore) ListControlRuns(ctx context.Context, legalEntityID, fiscalPeriod string) ([]domain.SubledgerControlRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.SubledgerControlRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT control_run_id, tenant_id, legal_entity_id, fiscal_period, subledger,
			       control_account_code, subledger_total_amount, gl_control_balance_amount,
			       difference_amount, status, run_at, run_by_principal_id
			FROM subledger_control_runs
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_period = $3
			ORDER BY run_at DESC
		`, tenantID, legalEntityID, fiscalPeriod)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var run domain.SubledgerControlRun
			if err := rows.Scan(
				&run.ControlRunID, &run.TenantID, &run.LegalEntityID, &run.FiscalPeriod, &run.Subledger,
				&run.ControlAccountCode, &run.SubledgerTotalAmount, &run.GLControlBalanceAmount,
				&run.DifferenceAmount, &run.Status, &run.RunAt, &run.RunByPrincipalID,
			); err != nil {
				return err
			}
			out = append(out, run)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const accrualScheduleColumns = `
	schedule_id, tenant_id, legal_entity_id, description, policy_version,
	total_amount, start_fiscal_period, period_count, debit_account_code,
	credit_account_code, status, created_at, created_by_principal_id,
	submitted_at, submitted_by_principal_id, approved_at, approved_by_principal_id,
	cancelled_at, cancelled_by_principal_id`

func scanAccrualSchedule(row pgx.Row) (*domain.AccrualSchedule, error) {
	var sch domain.AccrualSchedule
	if err := row.Scan(
		&sch.ScheduleID, &sch.TenantID, &sch.LegalEntityID, &sch.Description, &sch.PolicyVersion,
		&sch.TotalAmount, &sch.StartFiscalPeriod, &sch.PeriodCount, &sch.DebitAccountCode,
		&sch.CreditAccountCode, &sch.Status, &sch.CreatedAt, &sch.CreatedByPrincipalID,
		&sch.SubmittedAt, &sch.SubmittedByPrincipalID, &sch.ApprovedAt, &sch.ApprovedByPrincipalID,
		&sch.CancelledAt, &sch.CancelledByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &sch, nil
}

// CreateAccrualSchedule persists a new ACC-07 schedule in DRAFT status.
func (s *PgStore) CreateAccrualSchedule(ctx context.Context, sch *domain.AccrualSchedule) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO accrual_schedules (
				schedule_id, tenant_id, legal_entity_id, description, policy_version,
				total_amount, start_fiscal_period, period_count, debit_account_code,
				credit_account_code, status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, sch.ScheduleID, tenantID, sch.LegalEntityID, sch.Description, sch.PolicyVersion,
			sch.TotalAmount, sch.StartFiscalPeriod, sch.PeriodCount, sch.DebitAccountCode,
			sch.CreditAccountCode, sch.Status, sch.CreatedAt, sch.CreatedByPrincipalID)
		return err
	})
}

func (s *PgStore) GetAccrualSchedule(ctx context.Context, scheduleID string) (*domain.AccrualSchedule, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var sch *domain.AccrualSchedule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+accrualScheduleColumns+` FROM accrual_schedules WHERE schedule_id = $1`, scheduleID)
		var err error
		sch, err = scanAccrualSchedule(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAccrualNotFound
		}
		return mapPgError(err)
	})
	if err != nil {
		return nil, err
	}
	return sch, nil
}

func (s *PgStore) ListAccrualSchedules(ctx context.Context, legalEntityID string) ([]domain.AccrualSchedule, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.AccrualSchedule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+accrualScheduleColumns+` FROM accrual_schedules WHERE tenant_id = $1 AND legal_entity_id = $2 ORDER BY created_at DESC`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sch, err := scanAccrualSchedule(rows)
			if err != nil {
				return err
			}
			out = append(out, *sch)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// transitionAccrualStatus moves a schedule from fromStatus to toStatus,
// atomically guarded by a WHERE status = fromStatus clause — the same
// posture as LockFiscalPeriod's OPEN-only guard. setClause supplies any
// additional columns (timestamps/actor) the specific transition sets.
func (s *PgStore) transitionAccrualStatus(ctx context.Context, scheduleID, fromStatus, toStatus, setClause string, args ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := fmt.Sprintf(`UPDATE accrual_schedules SET status = $1%s WHERE schedule_id = $2 AND status = $3`, setClause)
		fullArgs := append([]any{toStatus}, args...)
		fullArgs = append(fullArgs, scheduleID, fromStatus)
		tag, err := tx.Exec(ctx, query, fullArgs...)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			// Either the schedule does not exist, or it exists but is not in
			// fromStatus. Distinguished so the handler can tell "not found"
			// from "wrong state" rather than reporting both the same way.
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accrual_schedules WHERE schedule_id = $1)`, scheduleID).Scan(&exists); err != nil {
				return mapPgError(err)
			}
			if !exists {
				return domain.ErrAccrualNotFound
			}
			return domain.ErrInvalidAccrualTransition
		}
		return nil
	})
}

func (s *PgStore) SubmitAccrualSchedule(ctx context.Context, scheduleID, principalID string, at time.Time) error {
	return s.transitionAccrualStatus(ctx, scheduleID, domain.AccrualStatusDraft, domain.AccrualStatusPendingApproval,
		`, submitted_at = $4, submitted_by_principal_id = $5`, at, principalID)
}

func (s *PgStore) ApproveAccrualSchedule(ctx context.Context, scheduleID, principalID string, at time.Time) error {
	return s.transitionAccrualStatus(ctx, scheduleID, domain.AccrualStatusPendingApproval, domain.AccrualStatusApproved,
		`, approved_at = $4, approved_by_principal_id = $5`, at, principalID)
}

// ActivateAccrualSchedule moves APPROVED -> ACTIVE, on the first
// recognition run. A no-op (not an error) if the schedule is already
// ACTIVE — the caller here is the recognition path itself, which must stay
// idempotent on replay.
func (s *PgStore) ActivateAccrualSchedule(ctx context.Context, scheduleID string) error {
	err := s.transitionAccrualStatus(ctx, scheduleID, domain.AccrualStatusApproved, domain.AccrualStatusActive, "")
	if errors.Is(err, domain.ErrInvalidAccrualTransition) {
		return nil
	}
	return err
}

func (s *PgStore) CompleteAccrualSchedule(ctx context.Context, scheduleID string) error {
	return s.transitionAccrualStatus(ctx, scheduleID, domain.AccrualStatusActive, domain.AccrualStatusCompleted, "")
}

func (s *PgStore) CancelAccrualSchedule(ctx context.Context, scheduleID, fromStatus, principalID string, at time.Time) error {
	return s.transitionAccrualStatus(ctx, scheduleID, fromStatus, domain.AccrualStatusCancelled,
		`, cancelled_at = $4, cancelled_by_principal_id = $5`, at, principalID)
}

// AmendAccrualSchedule updates the FUTURE shape of an APPROVED/ACTIVE
// schedule. Callers must have already verified periodCount does not drop
// below the number of periods already recognized (ErrAmendWouldDropRecognizedPeriods)
// — enforced by the handler, not here, since only the handler has counted
// recognition instances.
func (s *PgStore) AmendAccrualSchedule(ctx context.Context, scheduleID string, totalAmount float64, periodCount int) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE accrual_schedules SET total_amount = $1, period_count = $2
			WHERE schedule_id = $3 AND status IN ($4, $5)
		`, totalAmount, periodCount, scheduleID, domain.AccrualStatusApproved, domain.AccrualStatusActive)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidAccrualTransition
		}
		return nil
	})
}

const recognitionInstanceColumns = `
	recognition_instance_id, tenant_id, schedule_id, fiscal_period,
	recognized_amount, journal_id, recognized_at, recognized_by_principal_id`

func scanRecognitionInstance(row pgx.Row) (*domain.RecognitionInstance, error) {
	var inst domain.RecognitionInstance
	if err := row.Scan(
		&inst.RecognitionInstanceID, &inst.TenantID, &inst.ScheduleID, &inst.FiscalPeriod,
		&inst.RecognizedAmount, &inst.JournalID, &inst.RecognizedAt, &inst.RecognizedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &inst, nil
}

// CreateRecognitionInstance inserts a permanent recognition record.
// Idempotent on the UNIQUE(schedule_id, fiscal_period) constraint: a
// replayed recognition run for a period already recognized returns
// created=false and the EXISTING instance rather than erroring — the
// spec's own negative-path requirement that a replay produce "no
// unauthorized or duplicate accounting consequence."
func (s *PgStore) CreateRecognitionInstance(ctx context.Context, inst *domain.RecognitionInstance) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO accrual_recognition_instances (
				recognition_instance_id, tenant_id, schedule_id, fiscal_period,
				recognized_amount, journal_id, recognized_at, recognized_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (schedule_id, fiscal_period) DO NOTHING
		`, inst.RecognitionInstanceID, tenantID, inst.ScheduleID, inst.FiscalPeriod,
			inst.RecognizedAmount, inst.JournalID, inst.RecognizedAt, inst.RecognizedByPrincipalID)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			created = false
			row := tx.QueryRow(ctx, `SELECT `+recognitionInstanceColumns+` FROM accrual_recognition_instances WHERE schedule_id = $1 AND fiscal_period = $2`, inst.ScheduleID, inst.FiscalPeriod)
			existing, err := scanRecognitionInstance(row)
			if err != nil {
				return err
			}
			*inst = *existing
			return nil
		}
		created = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) ListRecognitionInstances(ctx context.Context, scheduleID string) ([]domain.RecognitionInstance, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.RecognitionInstance
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+recognitionInstanceColumns+` FROM accrual_recognition_instances WHERE schedule_id = $1 ORDER BY fiscal_period ASC`, scheduleID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			inst, err := scanRecognitionInstance(rows)
			if err != nil {
				return err
			}
			out = append(out, *inst)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const prepaymentScheduleColumns = `
	schedule_id, tenant_id, legal_entity_id, description,
	total_amount, start_fiscal_period, period_count, debit_account_code,
	credit_account_code, status, created_at, created_by_principal_id,
	approved_at, approved_by_principal_id,
	terminated_at, terminated_by_principal_id, termination_reason, termination_final_treatment`

func scanPrepaymentSchedule(row pgx.Row) (*domain.PrepaymentSchedule, error) {
	var sch domain.PrepaymentSchedule
	if err := row.Scan(
		&sch.ScheduleID, &sch.TenantID, &sch.LegalEntityID, &sch.Description,
		&sch.TotalAmount, &sch.StartFiscalPeriod, &sch.PeriodCount, &sch.DebitAccountCode,
		&sch.CreditAccountCode, &sch.Status, &sch.CreatedAt, &sch.CreatedByPrincipalID,
		&sch.ApprovedAt, &sch.ApprovedByPrincipalID,
		&sch.TerminatedAt, &sch.TerminatedByPrincipalID, &sch.TerminationReason, &sch.TerminationFinalTreatment,
	); err != nil {
		return nil, err
	}
	return &sch, nil
}

func (s *PgStore) CreatePrepaymentSchedule(ctx context.Context, sch *domain.PrepaymentSchedule) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO prepayment_schedules (
				schedule_id, tenant_id, legal_entity_id, description,
				total_amount, start_fiscal_period, period_count, debit_account_code,
				credit_account_code, status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, sch.ScheduleID, tenantID, sch.LegalEntityID, sch.Description,
			sch.TotalAmount, sch.StartFiscalPeriod, sch.PeriodCount, sch.DebitAccountCode,
			sch.CreditAccountCode, sch.Status, sch.CreatedAt, sch.CreatedByPrincipalID)
		return err
	})
}

func (s *PgStore) GetPrepaymentSchedule(ctx context.Context, scheduleID string) (*domain.PrepaymentSchedule, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var sch *domain.PrepaymentSchedule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+prepaymentScheduleColumns+` FROM prepayment_schedules WHERE schedule_id = $1`, scheduleID)
		var err error
		sch, err = scanPrepaymentSchedule(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPrepaymentNotFound
		}
		return mapPgError(err)
	})
	if err != nil {
		return nil, err
	}
	return sch, nil
}

func (s *PgStore) ListPrepaymentSchedules(ctx context.Context, legalEntityID string) ([]domain.PrepaymentSchedule, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.PrepaymentSchedule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+prepaymentScheduleColumns+` FROM prepayment_schedules WHERE tenant_id = $1 AND legal_entity_id = $2 ORDER BY created_at DESC`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sch, err := scanPrepaymentSchedule(rows)
			if err != nil {
				return err
			}
			out = append(out, *sch)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) transitionPrepaymentStatus(ctx context.Context, scheduleID, fromStatus, toStatus, setClause string, args ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := fmt.Sprintf(`UPDATE prepayment_schedules SET status = $1%s WHERE schedule_id = $2 AND status = $3`, setClause)
		fullArgs := append([]any{toStatus}, args...)
		fullArgs = append(fullArgs, scheduleID, fromStatus)
		tag, err := tx.Exec(ctx, query, fullArgs...)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM prepayment_schedules WHERE schedule_id = $1)`, scheduleID).Scan(&exists); err != nil {
				return mapPgError(err)
			}
			if !exists {
				return domain.ErrPrepaymentNotFound
			}
			return domain.ErrInvalidPrepaymentTransition
		}
		return nil
	})
}

func (s *PgStore) ApprovePrepaymentSchedule(ctx context.Context, scheduleID, principalID string, at time.Time) error {
	return s.transitionPrepaymentStatus(ctx, scheduleID, domain.PrepaymentStatusDraft, domain.PrepaymentStatusApproved,
		`, approved_at = $4, approved_by_principal_id = $5`, at, principalID)
}

// ActivatePrepaymentSchedule moves APPROVED -> ACTIVE on the first
// recognition run — a no-op (not an error) if already ACTIVE, same
// idempotent-on-replay posture as ACC-07's ActivateAccrualSchedule.
func (s *PgStore) ActivatePrepaymentSchedule(ctx context.Context, scheduleID string) error {
	err := s.transitionPrepaymentStatus(ctx, scheduleID, domain.PrepaymentStatusApproved, domain.PrepaymentStatusActive, "")
	if errors.Is(err, domain.ErrInvalidPrepaymentTransition) {
		return nil
	}
	return err
}

func (s *PgStore) CompletePrepaymentSchedule(ctx context.Context, scheduleID string) error {
	return s.transitionPrepaymentStatus(ctx, scheduleID, domain.PrepaymentStatusActive, domain.PrepaymentStatusCompleted, "")
}

func (s *PgStore) TerminatePrepaymentSchedule(ctx context.Context, scheduleID, fromStatus, principalID, reason, treatment string, at time.Time) error {
	return s.transitionPrepaymentStatus(ctx, scheduleID, fromStatus, domain.PrepaymentStatusTerminated,
		`, terminated_at = $4, terminated_by_principal_id = $5, termination_reason = $6, termination_final_treatment = $7`,
		at, principalID, reason, treatment)
}

// ModifyFutureSchedule updates the FUTURE shape of an APPROVED/ACTIVE
// schedule. The handler is responsible for verifying periodCount does not
// drop below the number of periods already recognized before calling this
// (ErrModifyWouldDropRecognizedPeriods) — only it has counted recognition
// instances.
func (s *PgStore) ModifyFuturePrepaymentSchedule(ctx context.Context, scheduleID string, totalAmount float64, periodCount int) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE prepayment_schedules SET total_amount = $1, period_count = $2
			WHERE schedule_id = $3 AND status IN ($4, $5)
		`, totalAmount, periodCount, scheduleID, domain.PrepaymentStatusApproved, domain.PrepaymentStatusActive)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidPrepaymentTransition
		}
		return nil
	})
}

const prepaymentRecognitionColumns = `
	recognition_instance_id, tenant_id, schedule_id, fiscal_period,
	recognized_amount, journal_id, recognized_at, recognized_by_principal_id`

func scanPrepaymentRecognition(row pgx.Row) (*domain.PrepaymentRecognitionInstance, error) {
	var inst domain.PrepaymentRecognitionInstance
	if err := row.Scan(
		&inst.RecognitionInstanceID, &inst.TenantID, &inst.ScheduleID, &inst.FiscalPeriod,
		&inst.RecognizedAmount, &inst.JournalID, &inst.RecognizedAt, &inst.RecognizedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &inst, nil
}

// CreatePrepaymentRecognition mirrors ACC-07's CreateRecognitionInstance:
// idempotent on the UNIQUE(schedule_id, fiscal_period) constraint — a
// replayed recognition (or a replayed terminate, via the pseudo-period
// TerminationPseudoPeriod) returns the EXISTING instance rather than
// erroring or duplicating.
func (s *PgStore) CreatePrepaymentRecognition(ctx context.Context, inst *domain.PrepaymentRecognitionInstance) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO prepayment_recognition_instances (
				recognition_instance_id, tenant_id, schedule_id, fiscal_period,
				recognized_amount, journal_id, recognized_at, recognized_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (schedule_id, fiscal_period) DO NOTHING
		`, inst.RecognitionInstanceID, tenantID, inst.ScheduleID, inst.FiscalPeriod,
			inst.RecognizedAmount, inst.JournalID, inst.RecognizedAt, inst.RecognizedByPrincipalID)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			created = false
			row := tx.QueryRow(ctx, `SELECT `+prepaymentRecognitionColumns+` FROM prepayment_recognition_instances WHERE schedule_id = $1 AND fiscal_period = $2`, inst.ScheduleID, inst.FiscalPeriod)
			existing, err := scanPrepaymentRecognition(row)
			if err != nil {
				return err
			}
			*inst = *existing
			return nil
		}
		created = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) ListPrepaymentRecognitions(ctx context.Context, scheduleID string) ([]domain.PrepaymentRecognitionInstance, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.PrepaymentRecognitionInstance
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+prepaymentRecognitionColumns+` FROM prepayment_recognition_instances WHERE schedule_id = $1 ORDER BY fiscal_period ASC`, scheduleID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			inst, err := scanPrepaymentRecognition(rows)
			if err != nil {
				return err
			}
			out = append(out, *inst)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── ACC-09 (Allocation Engine) ───────────────────────────────────────────────────

const allocationRuleColumns = `
	rule_version_id, rule_id, version, tenant_id, legal_entity_id, name,
	source_account_code, status, created_at, created_by_principal_id,
	approved_at, approved_by_principal_id, effective_to`

func scanAllocationRule(row pgx.Row) (*domain.AllocationRule, error) {
	var rule domain.AllocationRule
	if err := row.Scan(
		&rule.RuleVersionID, &rule.RuleID, &rule.Version, &rule.TenantID, &rule.LegalEntityID, &rule.Name,
		&rule.SourceAccountCode, &rule.Status, &rule.CreatedAt, &rule.CreatedByPrincipalID,
		&rule.ApprovedAt, &rule.ApprovedByPrincipalID, &rule.EffectiveTo,
	); err != nil {
		return nil, err
	}
	return &rule, nil
}

func (s *PgStore) loadAllocationDrivers(ctx context.Context, tx pgx.Tx, ruleVersionID string) ([]domain.AllocationDriver, error) {
	rows, err := tx.Query(ctx, `SELECT recipient_account_code, weight_percentage FROM allocation_rule_drivers WHERE rule_version_id = $1 ORDER BY recipient_account_code`, ruleVersionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AllocationDriver
	for rows.Next() {
		var d domain.AllocationDriver
		if err := rows.Scan(&d.RecipientAccountCode, &d.WeightPercentage); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreateAllocationRule inserts a new rule version (rule.RuleVersionID and
// rule.RuleID must both already be set — a first-ever rule sets RuleID to
// a fresh UUID matching RuleVersionID; a superseding version reuses the
// PRIOR rule's RuleID) plus its driver rows, in one transaction.
func (s *PgStore) CreateAllocationRule(ctx context.Context, rule *domain.AllocationRule) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO allocation_rules (
				rule_version_id, rule_id, version, tenant_id, legal_entity_id, name,
				source_account_code, status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, rule.RuleVersionID, rule.RuleID, rule.Version, tenantID, rule.LegalEntityID, rule.Name,
			rule.SourceAccountCode, rule.Status, rule.CreatedAt, rule.CreatedByPrincipalID)
		if err != nil {
			return err
		}
		for _, d := range rule.Drivers {
			if _, err := tx.Exec(ctx, `
				INSERT INTO allocation_rule_drivers (rule_version_id, recipient_account_code, weight_percentage)
				VALUES ($1, $2, $3)
			`, rule.RuleVersionID, d.RecipientAccountCode, d.WeightPercentage); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PgStore) GetCurrentAllocationRule(ctx context.Context, ruleID string) (*domain.AllocationRule, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var rule *domain.AllocationRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+allocationRuleColumns+` FROM allocation_rules WHERE rule_id = $1 AND effective_to IS NULL`, ruleID)
		var err error
		rule, err = scanAllocationRule(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAllocationRuleNotFound
		}
		if err != nil {
			return mapPgError(err)
		}
		rule.Drivers, err = s.loadAllocationDrivers(ctx, tx, rule.RuleVersionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rule, nil
}

func (s *PgStore) GetAllocationRuleVersion(ctx context.Context, ruleVersionID string) (*domain.AllocationRule, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var rule *domain.AllocationRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+allocationRuleColumns+` FROM allocation_rules WHERE rule_version_id = $1`, ruleVersionID)
		var err error
		rule, err = scanAllocationRule(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAllocationRuleNotFound
		}
		if err != nil {
			return mapPgError(err)
		}
		rule.Drivers, err = s.loadAllocationDrivers(ctx, tx, rule.RuleVersionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rule, nil
}

func (s *PgStore) ListAllocationRules(ctx context.Context, legalEntityID string) ([]domain.AllocationRule, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.AllocationRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+allocationRuleColumns+` FROM allocation_rules WHERE tenant_id = $1 AND legal_entity_id = $2 AND effective_to IS NULL ORDER BY created_at DESC`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		var rules []domain.AllocationRule
		for rows.Next() {
			rule, err := scanAllocationRule(rows)
			if err != nil {
				return err
			}
			rules = append(rules, *rule)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range rules {
			drivers, err := s.loadAllocationDrivers(ctx, tx, rules[i].RuleVersionID)
			if err != nil {
				return err
			}
			rules[i].Drivers = drivers
		}
		out = rules
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) ApproveAllocationRule(ctx context.Context, ruleVersionID, principalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE allocation_rules SET status = $1, approved_at = $2, approved_by_principal_id = $3
			WHERE rule_version_id = $4 AND status = $5
		`, domain.AllocationRuleStatusApproved, at, principalID, ruleVersionID, domain.AllocationRuleStatusDraft)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidAllocationRuleTransition
		}
		return nil
	})
}

// ActivateAllocationRule moves APPROVED -> ACTIVE on the first successful
// execution — a no-op (not an error) if already ACTIVE, same posture as
// ACC-07/08's schedule activation.
func (s *PgStore) ActivateAllocationRule(ctx context.Context, ruleVersionID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE allocation_rules SET status = $1
			WHERE rule_version_id = $2 AND status = $3
		`, domain.AllocationRuleStatusActive, ruleVersionID, domain.AllocationRuleStatusApproved)
		return mapPgError(err)
	})
}

const allocationRunColumns = `
	run_id, tenant_id, legal_entity_id, rule_id, rule_version_id, fiscal_period,
	source_account_code, source_amount, status, journal_id, failure_reason,
	created_at, created_by_principal_id, calculated_at, posted_at`

func scanAllocationRun(row pgx.Row) (*domain.AllocationRun, error) {
	var run domain.AllocationRun
	if err := row.Scan(
		&run.RunID, &run.TenantID, &run.LegalEntityID, &run.RuleID, &run.RuleVersionID, &run.FiscalPeriod,
		&run.SourceAccountCode, &run.SourceAmount, &run.Status, &run.JournalID, &run.FailureReason,
		&run.CreatedAt, &run.CreatedByPrincipalID, &run.CalculatedAt, &run.PostedAt,
	); err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *PgStore) CreateAllocationRun(ctx context.Context, run *domain.AllocationRun) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO allocation_runs (
				run_id, tenant_id, legal_entity_id, rule_id, rule_version_id, fiscal_period,
				source_account_code, source_amount, status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, run.RunID, tenantID, run.LegalEntityID, run.RuleID, run.RuleVersionID, run.FiscalPeriod,
			run.SourceAccountCode, run.SourceAmount, run.Status, run.CreatedAt, run.CreatedByPrincipalID)
		return mapPgError(err)
	})
}

// GetAllocationRunByRuleAndPeriod is ACC-09's idempotency check for
// ExecuteAllocation — the spec's own negative path, "Rerun duplicates
// posting," relies on this returning the ONE existing run for
// (rule_id, fiscal_period) rather than a caller creating a second one.
func (s *PgStore) GetAllocationRunByRuleAndPeriod(ctx context.Context, ruleID, fiscalPeriod string) (*domain.AllocationRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var run *domain.AllocationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+allocationRunColumns+` FROM allocation_runs WHERE rule_id = $1 AND fiscal_period = $2`, ruleID, fiscalPeriod)
		var err error
		run, err = scanAllocationRun(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAllocationRunNotFound
		}
		return mapPgError(err)
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (s *PgStore) GetAllocationRun(ctx context.Context, runID string) (*domain.AllocationRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var run *domain.AllocationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+allocationRunColumns+` FROM allocation_runs WHERE run_id = $1`, runID)
		var err error
		run, err = scanAllocationRun(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAllocationRunNotFound
		}
		if err != nil {
			return mapPgError(err)
		}
		rows, err := tx.Query(ctx, `SELECT result_line_id, run_id, recipient_account_code, allocated_amount FROM allocation_run_result_lines WHERE run_id = $1 ORDER BY recipient_account_code`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.AllocationResultLine
			if err := rows.Scan(&l.ResultLineID, &l.RunID, &l.RecipientAccountCode, &l.AllocatedAmount); err != nil {
				return err
			}
			run.ResultLines = append(run.ResultLines, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (s *PgStore) MarkAllocationRunCalculated(ctx context.Context, runID string, sourceAmount float64, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE allocation_runs SET status = $1, source_amount = $2, calculated_at = $3 WHERE run_id = $4`,
			domain.AllocationRunStatusCalculated, sourceAmount, at, runID)
		return mapPgError(err)
	})
}

func (s *PgStore) MarkAllocationRunPosted(ctx context.Context, runID, journalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE allocation_runs SET status = $1, journal_id = $2, posted_at = $3 WHERE run_id = $4`,
			domain.AllocationRunStatusPosted, journalID, at, runID)
		return mapPgError(err)
	})
}

func (s *PgStore) MarkAllocationRunFailed(ctx context.Context, runID, reason string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE allocation_runs SET status = $1, failure_reason = $2 WHERE run_id = $3`,
			domain.AllocationRunStatusFailed, reason, runID)
		return mapPgError(err)
	})
}

// CreateAllocationResultLines persists the run's calculation evidence —
// append-only (migration 000007), inserted once per run.
func (s *PgStore) CreateAllocationResultLines(ctx context.Context, runID string, lines []domain.AllocationResultLine) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		for _, l := range lines {
			if _, err := tx.Exec(ctx, `
				INSERT INTO allocation_run_result_lines (result_line_id, tenant_id, run_id, recipient_account_code, allocated_amount)
				VALUES ($1, $2, $3, $4, $5)
			`, l.ResultLineID, tenantID, runID, l.RecipientAccountCode, l.AllocatedAmount); err != nil {
				return mapPgError(err)
			}
		}
		return nil
	})
}

// ListAllocationExceptions answers ACC-09's own ListAllocationExceptions
// query — every FAILED run for the legal entity, the closest real
// equivalent to an "exception list" this v1 tracks.
func (s *PgStore) ListAllocationExceptions(ctx context.Context, legalEntityID string) ([]domain.AllocationRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.AllocationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+allocationRunColumns+` FROM allocation_runs WHERE tenant_id = $1 AND legal_entity_id = $2 AND status = $3 ORDER BY created_at DESC`,
			tenantID, legalEntityID, domain.AllocationRunStatusFailed)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			run, err := scanAllocationRun(rows)
			if err != nil {
				return err
			}
			out = append(out, *run)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── ACC-10 (Foreign Currency Revaluation) ────────────────────────────────────────

const fxRunColumns = `
	run_id, tenant_id, legal_entity_id, fiscal_period, fx_gain_loss_account_code,
	status, reversal_of_run_id, journal_id, created_at, created_by_principal_id,
	approved_at, approved_by_principal_id, posted_at, posted_by_principal_id`

func scanFXRun(row pgx.Row) (*domain.FXRevaluationRun, error) {
	var run domain.FXRevaluationRun
	if err := row.Scan(
		&run.RunID, &run.TenantID, &run.LegalEntityID, &run.FiscalPeriod, &run.FXGainLossAccountCode,
		&run.Status, &run.ReversalOfRunID, &run.JournalID, &run.CreatedAt, &run.CreatedByPrincipalID,
		&run.ApprovedAt, &run.ApprovedByPrincipalID, &run.PostedAt, &run.PostedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *PgStore) CreateFXRevaluationRun(ctx context.Context, run *domain.FXRevaluationRun) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO fx_revaluation_runs (
				run_id, tenant_id, legal_entity_id, fiscal_period, fx_gain_loss_account_code,
				status, reversal_of_run_id, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, run.RunID, tenantID, run.LegalEntityID, run.FiscalPeriod, run.FXGainLossAccountCode,
			run.Status, run.ReversalOfRunID, run.CreatedAt, run.CreatedByPrincipalID)
		if err != nil {
			return mapPgError(err)
		}
		for _, item := range run.Items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO fx_revaluation_items (
					item_id, tenant_id, run_id, account_code, account_type, currency_code,
					foreign_amount, book_amount, closing_rate, revalued_amount, adjustment_amount
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			`, item.ItemID, tenantID, run.RunID, item.AccountCode, item.AccountType, item.CurrencyCode,
				item.ForeignAmount, item.BookAmount, item.ClosingRate, item.RevaluedAmount, item.AdjustmentAmount); err != nil {
				return mapPgError(err)
			}
		}
		return nil
	})
}

func (s *PgStore) GetFXRevaluationRun(ctx context.Context, runID string) (*domain.FXRevaluationRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var run *domain.FXRevaluationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+fxRunColumns+` FROM fx_revaluation_runs WHERE run_id = $1`, runID)
		var err error
		run, err = scanFXRun(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFXRevaluationRunNotFound
		}
		if err != nil {
			return mapPgError(err)
		}
		rows, err := tx.Query(ctx, `
			SELECT item_id, run_id, account_code, account_type, currency_code,
			       foreign_amount, book_amount, closing_rate, revalued_amount, adjustment_amount
			FROM fx_revaluation_items WHERE run_id = $1 ORDER BY account_code
		`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item domain.FXRevaluationItem
			if err := rows.Scan(
				&item.ItemID, &item.RunID, &item.AccountCode, &item.AccountType, &item.CurrencyCode,
				&item.ForeignAmount, &item.BookAmount, &item.ClosingRate, &item.RevaluedAmount, &item.AdjustmentAmount,
			); err != nil {
				return err
			}
			run.Items = append(run.Items, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (s *PgStore) ListFXRevaluationRuns(ctx context.Context, legalEntityID, fiscalPeriod string) ([]domain.FXRevaluationRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.FXRevaluationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+fxRunColumns+` FROM fx_revaluation_runs WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_period = $3 ORDER BY created_at DESC`,
			tenantID, legalEntityID, fiscalPeriod)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			run, err := scanFXRun(rows)
			if err != nil {
				return err
			}
			out = append(out, *run)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) ApproveFXRevaluationRun(ctx context.Context, runID, principalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE fx_revaluation_runs SET status = $1, approved_at = $2, approved_by_principal_id = $3
			WHERE run_id = $4 AND status = $5
		`, domain.FXRevaluationStatusApproved, at, principalID, runID, domain.FXRevaluationStatusReview)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidFXRevaluationTransition
		}
		return nil
	})
}

func (s *PgStore) MarkFXRevaluationPosted(ctx context.Context, runID, journalID, principalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE fx_revaluation_runs SET status = $1, journal_id = $2, posted_at = $3, posted_by_principal_id = $4
			WHERE run_id = $5 AND status = $6
		`, domain.FXRevaluationStatusPosted, journalID, at, principalID, runID, domain.FXRevaluationStatusApproved)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidFXRevaluationTransition
		}
		return nil
	})
}

// ── ACC-17 (Opening Balance & Migration) ─────────────────────────────────────────

const migrationBatchColumns = `
	batch_id, tenant_id, legal_entity_id, fiscal_period, source_system_name,
	source_extract_hash, expected_row_count, expected_total_debits, expected_total_credits,
	status, quarantine_reason, journal_id, created_at, created_by_principal_id,
	validated_at, approved_at, approved_by_principal_id, posted_at, reconciled_at,
	certified_at, certified_by_principal_id, certification_reason`

func scanMigrationBatch(row pgx.Row) (*domain.MigrationBatch, error) {
	var b domain.MigrationBatch
	if err := row.Scan(
		&b.BatchID, &b.TenantID, &b.LegalEntityID, &b.FiscalPeriod, &b.SourceSystemName,
		&b.SourceExtractHash, &b.ExpectedRowCount, &b.ExpectedTotalDebits, &b.ExpectedTotalCredits,
		&b.Status, &b.QuarantineReason, &b.JournalID, &b.CreatedAt, &b.CreatedByPrincipalID,
		&b.ValidatedAt, &b.ApprovedAt, &b.ApprovedByPrincipalID, &b.PostedAt, &b.ReconciledAt,
		&b.CertifiedAt, &b.CertifiedByPrincipalID, &b.CertificationReason,
	); err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *PgStore) CreateMigrationBatch(ctx context.Context, b *domain.MigrationBatch) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO migration_batches (
				batch_id, tenant_id, legal_entity_id, fiscal_period, source_system_name,
				source_extract_hash, expected_row_count, expected_total_debits, expected_total_credits,
				status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, b.BatchID, tenantID, b.LegalEntityID, b.FiscalPeriod, b.SourceSystemName,
			b.SourceExtractHash, b.ExpectedRowCount, b.ExpectedTotalDebits, b.ExpectedTotalCredits,
			b.Status, b.CreatedAt, b.CreatedByPrincipalID)
		if err != nil {
			return mapPgError(err)
		}
		for _, e := range b.Entries {
			if _, err := tx.Exec(ctx, `
				INSERT INTO migration_crosswalk_entries (
					entry_id, tenant_id, batch_id, source_reference_id, source_account_code,
					target_account_code, debit_amount, credit_amount
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			`, e.EntryID, tenantID, b.BatchID, e.SourceReferenceID, e.SourceAccountCode,
				e.TargetAccountCode, e.DebitAmount, e.CreditAmount); err != nil {
				return mapPgError(err)
			}
		}
		return nil
	})
}

// GetMigrationBatchBySourceSystem is the idempotency check backing
// CreateMigrationAccountingBatch's UNIQUE(tenant_id, legal_entity_id,
// fiscal_period, source_system_name) constraint — a retried create for
// the same source system and period returns the EXISTING batch rather
// than erroring or creating a duplicate.
func (s *PgStore) GetMigrationBatchBySourceSystem(ctx context.Context, legalEntityID, fiscalPeriod, sourceSystemName string) (*domain.MigrationBatch, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var b *domain.MigrationBatch
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+migrationBatchColumns+` FROM migration_batches WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_period = $3 AND source_system_name = $4`,
			tenantID, legalEntityID, fiscalPeriod, sourceSystemName)
		var err error
		b, err = scanMigrationBatch(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMigrationBatchNotFound
		}
		return mapPgError(err)
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s *PgStore) GetMigrationBatch(ctx context.Context, batchID string) (*domain.MigrationBatch, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var b *domain.MigrationBatch
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+migrationBatchColumns+` FROM migration_batches WHERE batch_id = $1`, batchID)
		var err error
		b, err = scanMigrationBatch(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMigrationBatchNotFound
		}
		if err != nil {
			return mapPgError(err)
		}
		rows, err := tx.Query(ctx, `
			SELECT entry_id, batch_id, source_reference_id, source_account_code, target_account_code, debit_amount, credit_amount
			FROM migration_crosswalk_entries WHERE batch_id = $1 ORDER BY source_reference_id
		`, batchID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.MigrationCrosswalkEntry
			if err := rows.Scan(&e.EntryID, &e.BatchID, &e.SourceReferenceID, &e.SourceAccountCode, &e.TargetAccountCode, &e.DebitAmount, &e.CreditAmount); err != nil {
				return err
			}
			b.Entries = append(b.Entries, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s *PgStore) transitionMigrationBatch(ctx context.Context, batchID, fromStatus, toStatus, setClause string, args ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := fmt.Sprintf(`UPDATE migration_batches SET status = $1%s WHERE batch_id = $2 AND status = $3`, setClause)
		fullArgs := append([]any{toStatus}, args...)
		fullArgs = append(fullArgs, batchID, fromStatus)
		tag, err := tx.Exec(ctx, query, fullArgs...)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_batches WHERE batch_id = $1)`, batchID).Scan(&exists); err != nil {
				return mapPgError(err)
			}
			if !exists {
				return domain.ErrMigrationBatchNotFound
			}
			return domain.ErrInvalidMigrationBatchTransition
		}
		return nil
	})
}

func (s *PgStore) MarkMigrationBatchValidated(ctx context.Context, batchID string, at time.Time) error {
	return s.transitionMigrationBatch(ctx, batchID, domain.MigrationBatchStatusLoaded, domain.MigrationBatchStatusValidated,
		`, validated_at = $4`, at)
}

func (s *PgStore) QuarantineMigrationBatch(ctx context.Context, batchID, fromStatus, reason string) error {
	return s.transitionMigrationBatch(ctx, batchID, fromStatus, domain.MigrationBatchStatusQuarantined,
		`, quarantine_reason = $4`, reason)
}

func (s *PgStore) ApproveMigrationBatch(ctx context.Context, batchID, principalID string, at time.Time) error {
	return s.transitionMigrationBatch(ctx, batchID, domain.MigrationBatchStatusValidated, domain.MigrationBatchStatusApproved,
		`, approved_at = $4, approved_by_principal_id = $5`, at, principalID)
}

func (s *PgStore) MarkMigrationBatchPosted(ctx context.Context, batchID, journalID string, at time.Time) error {
	return s.transitionMigrationBatch(ctx, batchID, domain.MigrationBatchStatusApproved, domain.MigrationBatchStatusPosted,
		`, journal_id = $4, posted_at = $5`, journalID, at)
}

func (s *PgStore) MarkMigrationBatchReconciled(ctx context.Context, batchID string, at time.Time) error {
	return s.transitionMigrationBatch(ctx, batchID, domain.MigrationBatchStatusPosted, domain.MigrationBatchStatusReconciled,
		`, reconciled_at = $4`, at)
}

func (s *PgStore) CertifyMigrationBatch(ctx context.Context, batchID, principalID, reason string, at time.Time) error {
	return s.transitionMigrationBatch(ctx, batchID, domain.MigrationBatchStatusReconciled, domain.MigrationBatchStatusCertified,
		`, certified_at = $4, certified_by_principal_id = $5, certification_reason = $6`, at, principalID, reason)
}

// ListQuarantinedMigrationBatches answers ACC-17's own
// GetMigrationExceptions query — every QUARANTINED batch for the legal
// entity, the closest real equivalent to an "exception list" this v1
// tracks (same posture as ACC-09's ListAllocationExceptions).
func (s *PgStore) ListQuarantinedMigrationBatches(ctx context.Context, legalEntityID string) ([]domain.MigrationBatch, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.MigrationBatch
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+migrationBatchColumns+` FROM migration_batches WHERE tenant_id = $1 AND legal_entity_id = $2 AND status = $3 ORDER BY created_at DESC`,
			tenantID, legalEntityID, domain.MigrationBatchStatusQuarantined)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanMigrationBatch(rows)
			if err != nil {
				return err
			}
			out = append(out, *b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── ACC-16 (Signed Financial Snapshot) ────────────────────────────────────────────

const financialSnapshotColumns = `
	snapshot_id, tenant_id, legal_entity_id, purpose, content, source_references,
	content_hash, signature, has_unresolved_exceptions, status, superseded_by_snapshot_id,
	created_at, created_by_principal_id, sealed_at, certified_at, certified_by_principal_id,
	certification_reason, superseded_at`

func scanFinancialSnapshot(row pgx.Row) (*domain.FinancialSnapshot, error) {
	var s domain.FinancialSnapshot
	if err := row.Scan(
		&s.SnapshotID, &s.TenantID, &s.LegalEntityID, &s.Purpose, &s.Content, &s.SourceReferences,
		&s.ContentHash, &s.Signature, &s.HasUnresolvedExceptions, &s.Status, &s.SupersededBySnapshotID,
		&s.CreatedAt, &s.CreatedByPrincipalID, &s.SealedAt, &s.CertifiedAt, &s.CertifiedByPrincipalID,
		&s.CertificationReason, &s.SupersededAt,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *PgStore) CreateFinancialSnapshot(ctx context.Context, snap *domain.FinancialSnapshot) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO financial_snapshots (
				snapshot_id, tenant_id, legal_entity_id, purpose, content, source_references,
				has_unresolved_exceptions, status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, snap.SnapshotID, tenantID, snap.LegalEntityID, snap.Purpose, snap.Content, snap.SourceReferences,
			snap.HasUnresolvedExceptions, snap.Status, snap.CreatedAt, snap.CreatedByPrincipalID)
		return mapPgError(err)
	})
}

func (s *PgStore) GetFinancialSnapshot(ctx context.Context, snapshotID string) (*domain.FinancialSnapshot, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var snap *domain.FinancialSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+financialSnapshotColumns+` FROM financial_snapshots WHERE snapshot_id = $1`, snapshotID)
		var err error
		snap, err = scanFinancialSnapshot(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrFinancialSnapshotNotFound
		}
		return mapPgError(err)
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

func (s *PgStore) SealFinancialSnapshot(ctx context.Context, snapshotID, contentHash, signature string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE financial_snapshots SET status = $1, content_hash = $2, signature = $3, sealed_at = $4
			WHERE snapshot_id = $5 AND status = $6
		`, domain.SnapshotStatusSealed, contentHash, signature, at, snapshotID, domain.SnapshotStatusDraft)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidSnapshotTransition
		}
		return nil
	})
}

func (s *PgStore) CertifyFinancialSnapshot(ctx context.Context, snapshotID, principalID, reason string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE financial_snapshots SET status = $1, certified_at = $2, certified_by_principal_id = $3, certification_reason = $4
			WHERE snapshot_id = $5 AND status = $6
		`, domain.SnapshotStatusCertified, at, principalID, reason, snapshotID, domain.SnapshotStatusSealed)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidSnapshotTransition
		}
		return nil
	})
}

// SupersedeFinancialSnapshot marks a CERTIFIED (or SEALED) snapshot
// SUPERSEDED, pointing at the new snapshot that replaces it — the
// supersession chain is this table's own linked list, never a
// destructive overwrite of the superseded snapshot's content.
func (s *PgStore) SupersedeFinancialSnapshot(ctx context.Context, snapshotID, fromStatus, newSnapshotID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE financial_snapshots SET status = $1, superseded_by_snapshot_id = $2, superseded_at = $3
			WHERE snapshot_id = $4 AND status = $5
		`, domain.SnapshotStatusSuperseded, newSnapshotID, at, snapshotID, fromStatus)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidSnapshotTransition
		}
		return nil
	})
}

func (s *PgStore) ListSnapshotSupersession(ctx context.Context, legalEntityID, purpose string) ([]domain.FinancialSnapshot, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.FinancialSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+financialSnapshotColumns+` FROM financial_snapshots WHERE tenant_id = $1 AND legal_entity_id = $2 AND purpose = $3 ORDER BY created_at ASC`,
			tenantID, legalEntityID, purpose)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			snap, err := scanFinancialSnapshot(rows)
			if err != nil {
				return err
			}
			out = append(out, *snap)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── ACC-18 (Source-to-Report Traceability) ───────────────────────────────────────

// RecordLineageEdge inserts one permanent provenance edge. Idempotent on
// the migration's own UNIQUE(tenant_id, from_type, from_id, to_type,
// to_id) constraint — recording the same edge twice (e.g. a replayed
// posting call) is a harmless no-op, never a duplicate row.
func (s *PgStore) RecordLineageEdge(ctx context.Context, edge *domain.LineageEdge) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO lineage_edges (edge_id, tenant_id, legal_entity_id, from_type, from_id, to_type, to_id, recorded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (tenant_id, from_type, from_id, to_type, to_id) DO NOTHING
		`, edge.EdgeID, tenantID, edge.LegalEntityID, edge.FromType, edge.FromID, edge.ToType, edge.ToID, edge.RecordedAt)
		return mapPgError(err)
	})
}

func (s *PgStore) ListLineageEdgesTo(ctx context.Context, toType, toID string) ([]domain.LineageEdge, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.LineageEdge
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT edge_id, tenant_id, legal_entity_id, from_type, from_id, to_type, to_id, recorded_at
			FROM lineage_edges WHERE tenant_id = $1 AND to_type = $2 AND to_id = $3 ORDER BY recorded_at
		`, tenantID, toType, toID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.LineageEdge
			if err := rows.Scan(&e.EdgeID, &e.TenantID, &e.LegalEntityID, &e.FromType, &e.FromID, &e.ToType, &e.ToID, &e.RecordedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPostedJournalRefs is the authoritative source of "which journals
// SHOULD have a recorded lineage edge" — drawn directly from every
// already-built ACC capability's own posted records, a real UNION across
// their tables, never a guess. Used by VerifyLineageCompleteness and
// RebuildLineageProjection.
func (s *PgStore) ListPostedJournalRefs(ctx context.Context, legalEntityID string) ([]domain.PostedJournalRef, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.PostedJournalRef
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT 'accrual_recognition', recognition_instance_id::text, journal_id
			FROM accrual_recognition_instances ari
			JOIN accrual_schedules sch ON sch.schedule_id = ari.schedule_id
			WHERE ari.tenant_id = $1 AND sch.legal_entity_id = $2

			UNION ALL

			SELECT 'prepayment_recognition', recognition_instance_id::text, journal_id
			FROM prepayment_recognition_instances pri
			JOIN prepayment_schedules psch ON psch.schedule_id = pri.schedule_id
			WHERE pri.tenant_id = $1 AND psch.legal_entity_id = $2

			UNION ALL

			SELECT 'allocation_run', run_id::text, journal_id
			FROM allocation_runs
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND status = 'POSTED' AND journal_id IS NOT NULL

			UNION ALL

			SELECT 'fx_revaluation_run', run_id::text, journal_id
			FROM fx_revaluation_runs
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND status = 'POSTED' AND journal_id IS NOT NULL AND journal_id != ''

			UNION ALL

			SELECT 'migration_batch', batch_id::text, journal_id
			FROM migration_batches
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND status IN ('POSTED', 'RECONCILED', 'CERTIFIED') AND journal_id IS NOT NULL
		`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref domain.PostedJournalRef
			if err := rows.Scan(&ref.FromType, &ref.FromID, &ref.JournalID); err != nil {
				return err
			}
			out = append(out, ref)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetLineageProjectionStatus(ctx context.Context, legalEntityID string) (*domain.LineageProjectionStatus, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var st *domain.LineageProjectionStatus
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT tenant_id, legal_entity_id, status, degraded_reason, last_rebuilt_at FROM lineage_projection_status WHERE tenant_id = $1 AND legal_entity_id = $2`, tenantID, legalEntityID)
		var s domain.LineageProjectionStatus
		if err := row.Scan(&s.TenantID, &s.LegalEntityID, &s.Status, &s.DegradedReason, &s.LastRebuiltAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// No row yet is a real, honest CURRENT — a legal entity
				// with no lineage-bearing activity at all has nothing
				// to be behind on.
				st = &domain.LineageProjectionStatus{TenantID: tenantID, LegalEntityID: legalEntityID, Status: domain.LineageProjectionCurrent}
				return nil
			}
			return mapPgError(err)
		}
		st = &s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

func (s *PgStore) UpsertLineageProjectionStatus(ctx context.Context, legalEntityID, status string, degradedReason *string, at *time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO lineage_projection_status (tenant_id, legal_entity_id, status, degraded_reason, last_rebuilt_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, legal_entity_id) DO UPDATE SET
				status = EXCLUDED.status,
				degraded_reason = EXCLUDED.degraded_reason,
				last_rebuilt_at = COALESCE(EXCLUDED.last_rebuilt_at, lineage_projection_status.last_rebuilt_at)
		`, tenantID, legalEntityID, status, degradedReason, at)
		return mapPgError(err)
	})
}
