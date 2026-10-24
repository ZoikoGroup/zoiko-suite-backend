package store_test

// Migration 000021's four database-level controls (SPEC_DEVIATIONS B1-B4)
// and the set-based lineage read, against a real Postgres: each test proves
// the row itself refuses what the handler also refuses, so a caller that
// bypasses the handler — a script, a future endpoint, a race — gets the same
// answer. Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

// testRequestHash stands in for a request fingerprint (a sha256 hex string).
const testRequestHash = "0000000000000000000000000000000000000000000000000000000000000000"

const otherRequestHash = "1111111111111111111111111111111111111111111111111111111111111111"

func newHardeningStore(t *testing.T) (*pgxpool.Pool, *store.PgStore, context.Context, string) {
	t.Helper()
	pool := openTestPool(t)
	tenantID := uuid.New().String()
	return pool, store.New(pool, zap.NewNop(), store.WithEventRegion("uk")), svcmiddleware.WithTenant(context.Background(), tenantID), tenantID
}

func draftAccrual(description string) *domain.AccrualSchedule {
	return &domain.AccrualSchedule{
		ScheduleID: uuid.New().String(), LegalEntityID: "le-1", Description: description, PolicyVersion: "v1",
		TotalAmount: 900, StartFiscalPeriod: "2026-01", PeriodCount: 3,
		DebitAccountCode: "6100-AuditFee", CreditAccountCode: "2100-AccruedLiabilities",
		Status: domain.AccrualStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
}

// ── B1. Maker-checker ────────────────────────────────────────────────────────

func TestPgStore_SoD_SelfApprovalRefusedByDatabase_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	sch := draftAccrual("sod accrual")
	if _, err := s.CreateAccrualSchedule(ctx, sch, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAccrualSchedule: %v", err)
	}
	if err := s.SubmitAccrualSchedule(ctx, sch.ScheduleID, "submitter-1", now); err != nil {
		t.Fatalf("SubmitAccrualSchedule: %v", err)
	}
	for _, self := range []string{"preparer-1", "submitter-1"} {
		if err := s.ApproveAccrualSchedule(ctx, sch.ScheduleID, self, now); !errors.Is(err, domain.ErrSelfApproval) {
			t.Fatalf("accrual approved by its %s: want ErrSelfApproval, got %v", self, err)
		}
	}
	if err := s.ApproveAccrualSchedule(ctx, sch.ScheduleID, "approver-1", now); err != nil {
		t.Fatalf("independent accrual approval refused: %v", err)
	}

	ppy := &domain.PrepaymentSchedule{
		ScheduleID: uuid.New().String(), LegalEntityID: "le-1", Description: "sod prepayment",
		TotalAmount: 1200, StartFiscalPeriod: "2026-01", PeriodCount: 12,
		DebitAccountCode: "6200-Insurance", CreditAccountCode: "1400-Prepaid",
		Status: domain.PrepaymentStatusDraft, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if _, err := s.CreatePrepaymentSchedule(ctx, ppy, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreatePrepaymentSchedule: %v", err)
	}
	if err := s.ApprovePrepaymentSchedule(ctx, ppy.ScheduleID, "preparer-1", now); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("prepayment self-approval: want ErrSelfApproval, got %v", err)
	}

	ruleID := uuid.New().String()
	rule := &domain.AllocationRule{
		RuleVersionID: ruleID, RuleID: ruleID, Version: 1, LegalEntityID: "le-1", Name: "sod rule",
		SourceAccountCode: "5000-Shared", Drivers: []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}},
		Status: domain.AllocationRuleStatusDraft, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if _, err := s.CreateAllocationRule(ctx, rule, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAllocationRule: %v", err)
	}
	if err := s.ApproveAllocationRule(ctx, ruleID, "preparer-1", now); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("allocation rule self-approval: want ErrSelfApproval, got %v", err)
	}

	fx := &domain.FXRevaluationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7900-FX",
		Status: domain.FXRevaluationStatusReview, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if _, err := s.CreateFXRevaluationRun(ctx, fx, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateFXRevaluationRun: %v", err)
	}
	if err := s.ApproveFXRevaluationRun(ctx, fx.RunID, "preparer-1", now); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("FX revaluation self-approval: want ErrSelfApproval, got %v", err)
	}

	batch := &domain.MigrationBatch{
		BatchID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:x", ExpectedRowCount: 1,
		Status: domain.MigrationBatchStatusLoaded, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
		Entries: []domain.MigrationCrosswalkEntry{{EntryID: uuid.New().String(), SourceReferenceID: "R1", TargetAccountCode: "1000-Cash"}},
	}
	if err := s.CreateMigrationBatch(ctx, batch); err != nil {
		t.Fatalf("CreateMigrationBatch: %v", err)
	}
	if err := s.MarkMigrationBatchValidated(ctx, batch.BatchID, now); err != nil {
		t.Fatalf("MarkMigrationBatchValidated: %v", err)
	}
	if err := s.ApproveMigrationBatch(ctx, batch.BatchID, "preparer-1", now); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("migration self-approval: want ErrSelfApproval, got %v", err)
	}
	if err := s.ApproveMigrationBatch(ctx, batch.BatchID, "approver-1", now); err != nil {
		t.Fatalf("ApproveMigrationBatch: %v", err)
	}
	if err := s.MarkMigrationBatchPosted(ctx, batch.BatchID, "journal-1", now); err != nil {
		t.Fatalf("MarkMigrationBatchPosted: %v", err)
	}
	if err := s.MarkMigrationBatchReconciled(ctx, batch.BatchID, now); err != nil {
		t.Fatalf("MarkMigrationBatchReconciled: %v", err)
	}
	if err := s.CertifyMigrationBatch(ctx, batch.BatchID, "preparer-1", "cutover signed", now); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("migration self-certification: want ErrSelfApproval, got %v", err)
	}
	if err := s.CertifyMigrationBatch(ctx, batch.BatchID, "certifier-1", "cutover signed", now); err != nil {
		t.Fatalf("independent certification refused: %v", err)
	}
}

// Every guarded transition that also sets columns used to bind its id to the
// wrong placeholder (see transitionArgs) and fail with 22P02 against a real
// database. The SoD test above walks submit/approve/validate/post/reconcile/
// certify; this covers the remaining two, and that the columns they set land.
func TestPgStore_TransitionsWithSetClause_ApplyAgainstRealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	sch := draftAccrual("cancel me")
	if _, err := s.CreateAccrualSchedule(ctx, sch, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAccrualSchedule: %v", err)
	}
	if err := s.CancelAccrualSchedule(ctx, sch.ScheduleID, domain.AccrualStatusDraft, "canceller-1", now); err != nil {
		t.Fatalf("CancelAccrualSchedule: %v", err)
	}
	got, err := s.GetAccrualSchedule(ctx, sch.ScheduleID)
	if err != nil || got.Status != domain.AccrualStatusCancelled || derefStr(got.CancelledByPrincipalID) != "canceller-1" {
		t.Fatalf("cancel did not land: %+v (err %v)", got, err)
	}
	if err := s.CancelAccrualSchedule(ctx, sch.ScheduleID, domain.AccrualStatusDraft, "canceller-1", now); !errors.Is(err, domain.ErrInvalidAccrualTransition) {
		t.Fatalf("second cancel from DRAFT: want ErrInvalidAccrualTransition, got %v", err)
	}

	ppy := &domain.PrepaymentSchedule{
		ScheduleID: uuid.New().String(), LegalEntityID: "le-1", Description: "terminate me", TotalAmount: 1200,
		StartFiscalPeriod: "2026-01", PeriodCount: 12, DebitAccountCode: "6200-Insurance", CreditAccountCode: "1400-Prepaid",
		Status: domain.PrepaymentStatusDraft, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
	}
	if _, err := s.CreatePrepaymentSchedule(ctx, ppy, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreatePrepaymentSchedule: %v", err)
	}
	if err := s.TerminatePrepaymentSchedule(ctx, ppy.ScheduleID, domain.PrepaymentStatusDraft, "terminator-1", "contract ended",
		domain.TerminationTreatmentWriteOff, now); err != nil {
		t.Fatalf("TerminatePrepaymentSchedule: %v", err)
	}
	gotP, err := s.GetPrepaymentSchedule(ctx, ppy.ScheduleID)
	if err != nil || gotP.Status != domain.PrepaymentStatusTerminated || derefStr(gotP.TerminationFinalTreatment) != domain.TerminationTreatmentWriteOff {
		t.Fatalf("terminate did not land: %+v (err %v)", gotP, err)
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ── B3. command_idempotency ──────────────────────────────────────────────────

func TestPgStore_CommandIdempotency_ReplayConflictAndRace_RealDB(t *testing.T) {
	pool, s, ctx, tenantID := newHardeningStore(t)

	first := draftAccrual("idempotent accrual")
	if replay, err := s.CreateAccrualSchedule(ctx, first, "key-1", testRequestHash); err != nil || replay {
		t.Fatalf("first create: replay=%v err=%v", replay, err)
	}

	// A retry mints a new id (the handler does) but must get the original.
	retry := draftAccrual("idempotent accrual")
	replay, err := s.CreateAccrualSchedule(ctx, retry, "key-1", testRequestHash)
	if err != nil || !replay {
		t.Fatalf("retry: want replay, got replay=%v err=%v", replay, err)
	}
	if retry.ScheduleID != first.ScheduleID {
		t.Fatalf("replay returned schedule %s, want the original %s", retry.ScheduleID, first.ScheduleID)
	}

	if _, err := s.CreateAccrualSchedule(ctx, draftAccrual("different body"), "key-1", otherRequestHash); !errors.Is(err, domain.ErrIdempotencyKeyConflict) {
		t.Fatalf("same key, different body: want ErrIdempotencyKeyConflict, got %v", err)
	}
	ppy := &domain.PrepaymentSchedule{
		ScheduleID: uuid.New().String(), LegalEntityID: "le-1", Description: "x", TotalAmount: 1, StartFiscalPeriod: "2026-01",
		PeriodCount: 1, DebitAccountCode: "a", CreditAccountCode: "b", Status: domain.PrepaymentStatusDraft,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if _, err := s.CreatePrepaymentSchedule(ctx, ppy, "key-1", testRequestHash); !errors.Is(err, domain.ErrIdempotencyKeyConflict) {
		t.Fatalf("same key, different command: want ErrIdempotencyKeyConflict, got %v", err)
	}
	if _, err := s.CreateAccrualSchedule(ctx, draftAccrual("no key"), "", testRequestHash); !errors.Is(err, domain.ErrIdempotencyKeyRequired) {
		t.Fatalf("no key: want ErrIdempotencyKeyRequired, got %v", err)
	}

	// Concurrent first attempts with one key: exactly one schedule, and every
	// caller is answered with it.
	const callers = 8
	var wg sync.WaitGroup
	ids := make([]string, callers)
	errs := make([]error, callers)
	created := make([]bool, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sch := draftAccrual("race accrual")
			replay, err := s.CreateAccrualSchedule(ctx, sch, "key-race", testRequestHash)
			ids[i], errs[i], created[i] = sch.ScheduleID, err, !replay
		}(i)
	}
	wg.Wait()
	winners := 0
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("caller %d got schedule %s, caller 0 got %s", i, ids[i], ids[0])
		}
		if created[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("want exactly one creating caller, got %d", winners)
	}
	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM accrual_schedules WHERE tenant_id = $1 AND description = 'race accrual'`, tenantID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("want 1 accrual schedule from the race, got %d", rows)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE command_idempotency SET request_hash = $1 WHERE tenant_id = $2`, otherRequestHash, tenantID); err == nil {
		t.Fatal("command_idempotency accepted an UPDATE; a claim must be append-only")
	}
}

// ── B2. One reversal per FX revaluation run ──────────────────────────────────

func TestPgStore_FXReversal_AtMostOnePerPriorRun_RealDB(t *testing.T) {
	pool, s, ctx, tenantID := newHardeningStore(t)
	now := time.Now().UTC()

	prior := &domain.FXRevaluationRun{
		RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7900-FX",
		Status: domain.FXRevaluationStatusPosted, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
		Items: []domain.FXRevaluationItem{
			{ItemID: uuid.New().String(), AccountCode: "1100-EURCash", AccountType: domain.AccountTypeAsset, CurrencyCode: "EUR",
				ForeignAmount: 1000, BookAmount: 1080, ClosingRate: 1.1, RevaluedAmount: 1100, AdjustmentAmount: 20},
			{ItemID: uuid.New().String(), AccountCode: "2100-EURPayable", AccountType: domain.AccountTypeLiability, CurrencyCode: "EUR",
				ForeignAmount: 500, BookAmount: 540, ClosingRate: 1.1, RevaluedAmount: 550, AdjustmentAmount: 10},
		},
	}
	if _, err := s.CreateFXRevaluationRun(ctx, prior, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateFXRevaluationRun: %v", err)
	}
	got, err := s.GetFXRevaluationRun(ctx, prior.RunID)
	if err != nil || len(got.Items) != 2 {
		t.Fatalf("set-based item insert did not round-trip: %v / %+v", err, got)
	}

	reversal := func() *domain.FXRevaluationRun {
		id := prior.RunID
		return &domain.FXRevaluationRun{
			RunID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7900-FX",
			Status: domain.FXRevaluationStatusReview, ReversalOfRunID: &id, CreatedAt: now, CreatedByPrincipalID: "preparer-2",
		}
	}
	first := reversal()
	if replay, err := s.CreateFXReversalRun(ctx, first); err != nil || replay {
		t.Fatalf("first reversal: replay=%v err=%v", replay, err)
	}

	const callers = 6
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			again := reversal()
			replay, err := s.CreateFXReversalRun(ctx, again)
			if err != nil || !replay || again.RunID != first.RunID {
				t.Errorf("second reversal: want replay of %s, got replay=%v id=%s err=%v", first.RunID, replay, again.RunID, err)
			}
		}()
	}
	wg.Wait()

	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM fx_revaluation_runs WHERE tenant_id = $1 AND reversal_of_run_id = $2`, tenantID, prior.RunID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("want exactly one reversal of %s, got %d", prior.RunID, rows)
	}
}

// ── B4. fiscal_periods transition guard ──────────────────────────────────────

func TestPgStore_FiscalPeriodGuard_RefusesIllegalTransitionsAtTheDataLayer_RealDB(t *testing.T) {
	pool, s, ctx, tenantID := newHardeningStore(t)
	raw := context.Background()

	fp := &domain.FiscalPeriod{
		FiscalPeriodID: uuid.New().String(), LegalEntityID: "le-1", PeriodName: "2026-01",
		PeriodStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), PeriodEnd: time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
		CloseStatus: domain.PeriodOpen,
	}
	if _, err := s.CreateFiscalPeriod(ctx, fp); err != nil {
		t.Fatalf("CreateFiscalPeriod: %v", err)
	}
	state := func() string {
		var st string
		if err := pool.QueryRow(raw, `SELECT close_status FROM fiscal_periods WHERE fiscal_period_id = $1`, fp.FiscalPeriodID).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	// A skipped state through the store (allowedFrom is caller-chosen) and
	// through raw SQL are both refused.
	if _, err := s.ApplyPeriodTransition(ctx, fp.FiscalPeriodID, []string{domain.PeriodOpen},
		domain.PeriodUpdate{To: domain.PeriodHardClosed, PrincipalID: "p", At: time.Now().UTC()}, "", "", "c", "p"); err == nil {
		t.Fatal("OPEN -> HARD_CLOSED was accepted")
	}
	if _, err := pool.Exec(raw, `UPDATE fiscal_periods SET close_status = 'HARD_CLOSED' WHERE fiscal_period_id = $1`, fp.FiscalPeriodID); err == nil {
		t.Fatal("raw OPEN -> HARD_CLOSED was accepted")
	}
	if _, err := pool.Exec(raw, `UPDATE fiscal_periods SET period_end = period_end + interval '1 day' WHERE fiscal_period_id = $1`, fp.FiscalPeriodID); err == nil {
		t.Fatal("a period's dates were changed in place")
	}
	if st := state(); st != domain.PeriodOpen {
		t.Fatalf("refused transitions changed the period to %s", st)
	}

	at := time.Now().UTC()
	for _, step := range [][2]string{
		{domain.PeriodOpen, domain.PeriodSoftClose},
		{domain.PeriodSoftClose, domain.PeriodCloseReview},
		{domain.PeriodCloseReview, domain.PeriodHardClosed},
	} {
		if _, err := s.ApplyPeriodTransition(ctx, fp.FiscalPeriodID, []string{step[0]},
			domain.PeriodUpdate{To: step[1], PrincipalID: "closer-1", At: at, LockedAt: &at}, "", "", "c", "closer-1"); err != nil {
			t.Fatalf("%s -> %s refused: %v", step[0], step[1], err)
		}
	}

	// "Recovery does not auto-open closed periods": no direct reopen, and no
	// AUTHORIZED_REOPEN without an approved request, even with the window set.
	if _, err := pool.Exec(raw, `UPDATE fiscal_periods SET close_status = 'OPEN' WHERE fiscal_period_id = $1`, fp.FiscalPeriodID); err == nil {
		t.Fatal("raw HARD_CLOSED -> OPEN was accepted")
	}
	if _, err := pool.Exec(raw, `UPDATE fiscal_periods SET close_status = 'AUTHORIZED_REOPEN', reopened_at = now(), reopen_expires_at = now() + interval '1 day'
		WHERE fiscal_period_id = $1`, fp.FiscalPeriodID); err == nil {
		t.Fatal("AUTHORIZED_REOPEN committed without an approved reopen request")
	}
	if st := state(); st != domain.PeriodHardClosed {
		t.Fatalf("refused reopen changed the period to %s", st)
	}

	// The governed path still works: request, independent approval, reclose.
	req := &domain.ReopenRequest{
		RequestID: uuid.New().String(), TenantID: tenantID, FiscalPeriodID: fp.FiscalPeriodID, RequestedByPrincipalID: "requester-1",
		Reason: "late invoice", ReopenUntil: time.Now().UTC().Add(48 * time.Hour), Status: domain.ReopenPending, CreatedAt: time.Now().UTC(),
	}
	if err := s.CreateReopenRequest(ctx, req, "c", "requester-1"); err != nil {
		t.Fatalf("CreateReopenRequest: %v", err)
	}
	if _, _, err := s.ApproveReopenRequest(ctx, req.RequestID, "cfo-1", "approved", time.Now().UTC(), "c", "cfo-1"); err != nil {
		t.Fatalf("approved reopen refused by the guard: %v", err)
	}
	if st := state(); st != domain.PeriodAuthorizedReopen {
		t.Fatalf("approved reopen left the period %s", st)
	}
	if _, err := s.ApplyPeriodTransition(ctx, fp.FiscalPeriodID, []string{domain.PeriodAuthorizedReopen},
		domain.PeriodUpdate{To: domain.PeriodReclosed, PrincipalID: "closer-1", At: time.Now().UTC(), ClearReopen: true}, "", "", "c", "closer-1"); err != nil {
		t.Fatalf("AUTHORIZED_REOPEN -> RECLOSED refused: %v", err)
	}
}

// ── ACC-18 set-based coverage ────────────────────────────────────────────────

func TestPgStore_ListPostedJournalLineage_ClassifiesInOneQuery_RealDB(t *testing.T) {
	_, s, ctx, _ := newHardeningStore(t)
	now := time.Now().UTC()

	sch := draftAccrual("lineage accrual")
	if _, err := s.CreateAccrualSchedule(ctx, sch, uuid.NewString(), testRequestHash); err != nil {
		t.Fatalf("CreateAccrualSchedule: %v", err)
	}
	instances := make([]*domain.RecognitionInstance, 3)
	for i, period := range []string{"2026-01", "2026-02", "2026-03"} {
		instances[i] = &domain.RecognitionInstance{
			RecognitionInstanceID: uuid.New().String(), ScheduleID: sch.ScheduleID, FiscalPeriod: period,
			RecognizedAmount: 300, JournalID: "journal-" + period, RecognizedAt: now, RecognizedByPrincipalID: "preparer-1",
		}
		if _, err := s.CreateRecognitionInstance(ctx, instances[i]); err != nil {
			t.Fatalf("CreateRecognitionInstance: %v", err)
		}
	}
	edge := func(fromID, journalID string) {
		if err := s.RecordLineageEdge(ctx, &domain.LineageEdge{EdgeID: uuid.New().String(), LegalEntityID: "le-1",
			FromType: "accrual_recognition", FromID: fromID, ToType: "journal", ToID: journalID, RecordedAt: now}); err != nil {
			t.Fatalf("RecordLineageEdge: %v", err)
		}
	}
	edge(instances[0].RecognitionInstanceID, instances[0].JournalID)
	// An edge into the right journal from the wrong source proves nothing.
	edge(uuid.New().String(), instances[2].JournalID)
	if err := s.CreateQuarantinedLineageGap(ctx, &domain.QuarantinedLineageGap{QuarantineID: uuid.New().String(), LegalEntityID: "le-1",
		FromType: "accrual_recognition", FromID: instances[1].RecognitionInstanceID, ToType: "journal", ToID: instances[1].JournalID,
		Reason: "legacy journal", QuarantinedAt: now, QuarantinedByPrincipalID: "auditor-1"}); err != nil {
		t.Fatalf("CreateQuarantinedLineageGap: %v", err)
	}

	// A migration batch quarantined after posting still has a journal on the
	// ledger, so it stays in the population.
	batch := &domain.MigrationBatch{
		BatchID: uuid.New().String(), LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP",
		SourceExtractHash: "sha256:x", ExpectedRowCount: 1, Status: domain.MigrationBatchStatusLoaded, CreatedAt: now, CreatedByPrincipalID: "preparer-1",
		Entries: []domain.MigrationCrosswalkEntry{{EntryID: uuid.New().String(), SourceReferenceID: "R1", TargetAccountCode: "1000-Cash"}},
	}
	if err := s.CreateMigrationBatch(ctx, batch); err != nil {
		t.Fatalf("CreateMigrationBatch: %v", err)
	}
	for _, step := range []func() error{
		func() error { return s.MarkMigrationBatchValidated(ctx, batch.BatchID, now) },
		func() error { return s.ApproveMigrationBatch(ctx, batch.BatchID, "approver-1", now) },
		func() error { return s.MarkMigrationBatchPosted(ctx, batch.BatchID, "journal-migration", now) },
		func() error {
			return s.QuarantineMigrationBatch(ctx, batch.BatchID, domain.MigrationBatchStatusPosted, "reconciliation mismatch")
		},
	} {
		if err := step(); err != nil {
			t.Fatalf("migration batch fixture: %v", err)
		}
	}

	got, err := s.ListPostedJournalLineage(ctx, "le-1")
	if err != nil {
		t.Fatalf("ListPostedJournalLineage: %v", err)
	}
	byFrom := map[string]domain.PostedJournalLineage{}
	for _, l := range got {
		byFrom[l.FromID] = l
	}
	if len(got) != 4 {
		t.Fatalf("want 3 recognitions + 1 quarantined-after-posting batch, got %d: %+v", len(got), got)
	}
	if l := byFrom[instances[0].RecognitionInstanceID]; !l.HasEdge || l.Quarantined {
		t.Fatalf("recognition with its edge: got %+v", l)
	}
	if l := byFrom[instances[1].RecognitionInstanceID]; l.HasEdge || !l.Quarantined {
		t.Fatalf("quarantined recognition: got %+v", l)
	}
	if l := byFrom[instances[2].RecognitionInstanceID]; l.HasEdge || l.Quarantined {
		t.Fatalf("recognition with only a foreign edge must be a gap: got %+v", l)
	}
	if l, ok := byFrom[batch.BatchID]; !ok || l.JournalID != "journal-migration" || l.HasEdge {
		t.Fatalf("quarantined-after-posting batch: got %+v (present=%v)", l, ok)
	}

	other, err := s.ListPostedJournalLineage(ctx, "le-2")
	if err != nil || len(other) != 0 {
		t.Fatalf("another legal entity saw %d refs (err %v)", len(other), err)
	}
}
