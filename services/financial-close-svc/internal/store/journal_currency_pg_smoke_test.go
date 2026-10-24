package store_test

// Migration 000022's journal-currency guards (SPEC_DEVIATIONS A13) against a
// real Postgres. The handlers already require a currency on every create;
// these prove the row itself refuses a new record without one and refuses
// changing one, so an internal insert that forgets to copy it (allocation
// supersede, FX reversal) or a script cannot reopen the gap. Skips (not
// fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/financial-close-svc/internal/domain"
)

func TestPgStore_JournalCurrency_RequiredAndWellFormedOnInsert_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)

	missing := draftAccrual("no currency")
	missing.CurrencyCode = ""
	if _, err := s.CreateAccrualSchedule(ctx, missing, uuid.NewString(), testRequestHash); !errors.Is(err, domain.ErrCurrencyCodeMissing) {
		t.Fatalf("accrual without currency: want ErrCurrencyCodeMissing, got %v", err)
	}
	lower := draftAccrual("lower-case currency")
	lower.CurrencyCode = "gbp"
	if _, err := s.CreateAccrualSchedule(ctx, lower, uuid.NewString(), testRequestHash); !errors.Is(err, domain.ErrInvalidCurrencyCode) {
		t.Fatalf("accrual with lower-case currency: want ErrInvalidCurrencyCode, got %v", err)
	}

	ok := draftAccrual("with currency")
	if _, err := s.CreateAccrualSchedule(ctx, ok, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAccrualSchedule: %v", err)
	}
	got, err := s.GetAccrualSchedule(ctx, ok.ScheduleID)
	if err != nil || got.CurrencyCode != storeTestCurrency {
		t.Fatalf("read back: currency %q, err %v", got.CurrencyCode, err)
	}

	// The two internal inserts that copy a currency rather than take it from
	// a request: forgetting to copy it is refused by the row, not posted.
	now := time.Now().UTC()
	prior := &domain.FXRevaluationRun{
		CurrencyCode: storeTestCurrency, RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		FXGainLossAccountCode: "7900-FX", Status: domain.FXRevaluationStatusReview, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if _, err := s.CreateFXRevaluationRun(ctx, prior, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateFXRevaluationRun: %v", err)
	}
	priorID := prior.RunID
	reversal := &domain.FXRevaluationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7900-FX",
		Status: domain.FXRevaluationStatusReview, ReversalOfRunID: &priorID, CreatedAt: now, CreatedByPrincipalID: "preparer-2",
	}
	if _, err := s.CreateFXReversalRun(ctx, reversal); !errors.Is(err, domain.ErrCurrencyCodeMissing) {
		t.Fatalf("FX reversal without currency: want ErrCurrencyCodeMissing, got %v", err)
	}

	rule := &domain.AllocationRule{
		CurrencyCode: storeTestCurrency, RuleVersionID: uuid.New().String(), Version: 1, LegalEntityID: "le-1", Name: "IT",
		SourceAccountCode: "5000-IT", Status: domain.AllocationRuleStatusDraft, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
		Drivers: []domain.AllocationDriver{{RecipientAccountCode: "6100", WeightPercentage: 100}},
	}
	rule.RuleID = rule.RuleVersionID
	if _, err := s.CreateAllocationRule(ctx, rule, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAllocationRule: %v", err)
	}
	if err := s.ApproveAllocationRule(ctx, rule.RuleID, "approver-1", now); err != nil {
		t.Fatalf("ApproveAllocationRule: %v", err)
	}
	next := &domain.AllocationRule{RuleVersionID: uuid.New().String(), LegalEntityID: "le-1", Name: "IT v2", SourceAccountCode: "5000-IT",
		CreatedAt: now, CreatedByPrincipalID: "preparer-2", Drivers: rule.Drivers}
	if err := s.SupersedeAllocationRule(ctx, rule.RuleID, next, now); !errors.Is(err, domain.ErrCurrencyCodeMissing) {
		t.Fatalf("supersede without currency: want ErrCurrencyCodeMissing, got %v", err)
	}
}

// A currency, once set, never changes; a legacy row (written before 000022,
// so NULL) may be given one exactly once.
func TestPgStore_JournalCurrency_ImmutableOnceSet_RealDB(t *testing.T) {
	pool, s, ctx, tenantID := newHardeningStore(t)

	set := draftAccrual("currency set")
	if _, err := s.CreateAccrualSchedule(ctx, set, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAccrualSchedule: %v", err)
	}
	if err := setCurrency(ctx, pool, tenantID, set.ScheduleID, "EUR"); !isConstraint(err, "journal_currency_immutable") {
		t.Fatalf("changing a set currency: want journal_currency_immutable, got %v", err)
	}

	// A legacy row: inserted with triggers suspended, as if before 000022.
	legacy := draftAccrual("legacy")
	legacy.CurrencyCode = ""
	insertLegacyAccrual(ctx, t, pool, tenantID, legacy)
	if err := setCurrency(ctx, pool, tenantID, legacy.ScheduleID, "GBP"); err != nil {
		t.Fatalf("setting a legacy row's currency the first time: %v", err)
	}
	if err := setCurrency(ctx, pool, tenantID, legacy.ScheduleID, "EUR"); !isConstraint(err, "journal_currency_immutable") {
		t.Fatalf("changing it afterwards: want journal_currency_immutable, got %v", err)
	}
	// Ordinary updates of a legacy row that leave currency alone still work —
	// the guard fires only on currency_code, so status transitions on rows
	// written before 000022 are untouched.
	other := draftAccrual("legacy, status change")
	other.CurrencyCode = ""
	insertLegacyAccrual(ctx, t, pool, tenantID, other)
	if err := s.SubmitAccrualSchedule(ctx, other.ScheduleID, "submitter-1", time.Now().UTC()); err != nil {
		t.Fatalf("submitting a legacy row without touching currency: %v", err)
	}
}

func setCurrency(ctx context.Context, pool *pgxpool.Pool, tenantID, scheduleID, currency string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE accrual_schedules SET currency_code = $1 WHERE schedule_id = $2`, currency, scheduleID)
		return err
	})
}

func insertLegacyAccrual(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tenantID string, sch *domain.AccrualSchedule) {
	t.Helper()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO accrual_schedules (schedule_id, tenant_id, legal_entity_id, description, policy_version,
				total_amount, start_fiscal_period, period_count, debit_account_code, credit_account_code,
				status, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			sch.ScheduleID, tenantID, sch.LegalEntityID, sch.Description, sch.PolicyVersion,
			sch.TotalAmount, sch.StartFiscalPeriod, sch.PeriodCount, sch.DebitAccountCode, sch.CreditAccountCode,
			sch.Status, sch.CreatedAt, sch.CreatedByPrincipalID)
		return err
	})
	if err != nil {
		t.Fatalf("insert legacy accrual: %v", err)
	}
}

func isConstraint(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == name
}
