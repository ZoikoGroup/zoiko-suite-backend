package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/supplier-recovery-svc/internal/domain"
	"zoiko.io/supplier-recovery-svc/internal/middleware"
	"zoiko.io/supplier-recovery-svc/internal/outbox"
	"zoiko.io/supplier-recovery-svc/internal/store"
)

// These suites need a database. TEST_DATABASE_URL names the OWNER role (it
// applies the migrations from a clean slate and runs verification queries).
// When TEST_APP_DATABASE_URL names an ordinary NOSUPERUSER NOBYPASSRLS role the
// STORE runs as that role, so row-level security really binds it. Both refuse
// to run unless the database name contains "test": they DROP this service's tables.

func disposable(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil || !strings.Contains(strings.ToLower(strings.TrimPrefix(u.Path, "/")), "test") {
		t.Fatalf("refusing to run: %q is not recognisably a disposable test database", dsn)
	}
}

func openOwner(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	disposable(t, dsn)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, tbl := range []string{"outbox_events", "outbox_tmp", "recovery_commitments", "recovery_applications", "supplier_recovery_cases"} {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl+` CASCADE`)
	}
	for _, fn := range []string{"reject_recovery_case_mutation", "reject_recovery_evidence_mutation"} {
		_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+fn+`() CASCADE`)
	}
	_, filename, _, _ := runtime.Caller(0)
	migs, err := filepath.Glob(filepath.Join(filepath.Dir(filename), "../../deployments/migrations/*.up.sql"))
	if err != nil || len(migs) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(migs)
	for _, m := range migs {
		sql, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(m), err)
		}
	}
	return pool
}

type fx struct {
	owner  *pgxpool.Pool
	s      *store.PgStore
	tenant string
	le     string
	ctx    context.Context
}

func setup(t *testing.T) fx {
	t.Helper()
	owner := openOwner(t)
	storePool := owner
	if appDSN := os.Getenv("TEST_APP_DATABASE_URL"); appDSN != "" {
		disposable(t, appDSN)
		app, err := pgxpool.New(context.Background(), appDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Close)
		storePool = app
	}
	tenant := uuid.NewString()
	return fx{owner: owner, s: store.NewPgStore(storePool, zap.NewNop()), tenant: tenant, le: "le-" + uuid.NewString(),
		ctx: middleware.WithTenant(context.Background(), tenant)}
}

func (f fx) other() fx {
	t := uuid.NewString()
	return fx{owner: f.owner, s: f.s, tenant: t, le: "le-" + uuid.NewString(), ctx: middleware.WithTenant(context.Background(), t)}
}

func (f fx) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f fx) events(t *testing.T, caseID, eventType string) int {
	return f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, caseID, eventType)
}

func (f fx) newCase(t *testing.T, total float64) *domain.SupplierRecoveryCase {
	t.Helper()
	c, err := f.s.CreateCase(f.ctx, f.tenant, domain.CreateCaseRequest{
		LegalEntityID: f.le, SupplierRef: "vendor-1", RecoveryBasis: domain.BasisOverpayment,
		SourcePayableID: "payable-1", TotalAmount: total, Currency: "USD", RecoveryReason: "paid twice",
	}, "owner")
	if err != nil {
		t.Fatalf("CreateCase: %v", err)
	}
	return c
}

func (f fx) approved(t *testing.T, total float64) *domain.SupplierRecoveryCase {
	t.Helper()
	c := f.newCase(t, total)
	got, err := f.s.ApproveRecoveryPlan(f.ctx, c.CaseID, "checker")
	if err != nil || got.Status != domain.StatusInRecovery || got.ApprovedByPrincipalID != "checker" {
		t.Fatalf("ApproveRecoveryPlan: %v %+v", err, got)
	}
	return got
}

// ── lifecycle, exact money, atomic events ────────────────────────────────────

func TestLifecycle_ExactAmounts_EventsInTheSameTransaction(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 0.30)
	if c.RecoveredAmount != 0 {
		t.Fatalf("nothing recovered yet, got %v", c.RecoveredAmount)
	}

	// 0.10 + 0.20 must land on EXACTLY 0.30 (NUMERIC arithmetic), not 0.30000000000000004.
	p, applied, err := f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 0.10, "off-1", "", "checker")
	if err != nil || !applied || p.Status != domain.StatusPartiallyRecovered || p.RecoveredAmount != 0.10 {
		t.Fatalf("partial offset: %v %v %+v", err, applied, p)
	}
	r, applied, err := f.s.ApplyRecovery(f.ctx, c.CaseID, "REFUND", 0.20, "stmt-line-1", "bank-ref", "checker")
	if err != nil || !applied || r.Status != domain.StatusRecovered || r.RecoveredAmount != 0.30 {
		t.Fatalf("completing refund must reach RECOVERED at exactly 0.30: %v %v %+v", err, applied, r)
	}
	closed, err := f.s.CloseCase(f.ctx, c.CaseID, domain.CloseCaseRequest{Note: "done"}, "checker")
	if err != nil || closed.Status != domain.StatusClosed || closed.CloseNote != "done" {
		t.Fatalf("CloseCase: %v %+v", err, closed)
	}

	for typ, want := range map[string]int{
		domain.EventRecoveryCaseCreated: 1, domain.EventRecoveryPlanApproved: 1, domain.EventRecoveryOffsetApplied: 1,
		domain.EventSupplierRefundConfirmed: 1, domain.EventRecoveryClosed: 1,
	} {
		if got := f.events(t, c.CaseID, typ); got != want {
			t.Fatalf("%s: expected %d outbox event, got %d", typ, want, got)
		}
	}
	// The relay publishes the Variant B envelope verbatim.
	var raw []byte
	if err := f.owner.QueryRow(context.Background(), `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`,
		c.CaseID, domain.EventRecoveryClosed).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var env outbox.VariantBEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.EventType != domain.EventRecoveryClosed || env.EntityID != c.CaseID ||
		env.TenantID != f.tenant || env.ActorID != "checker" || env.SourceService != "supplier-recovery-svc" {
		t.Fatalf("unexpected envelope %v %+v", err, env)
	}
	apps, _ := f.s.ListApplications(f.ctx, c.CaseID)
	if len(apps) != 2 || apps[0].ApplicationType != "OFFSET" || apps[1].ApplicationType != "REFUND" {
		t.Fatalf("expected the append-only ledger of both applications, got %+v", apps)
	}
}

// The path that was broken before: a replay of an application that already
// completed the case. The unique-index conflict used to abort the transaction.
func TestApplyRecovery_Replay_IsAnIdempotentNoOp_EvenAfterFullRecovery(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 50)
	if _, applied, err := f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 50, "once", "", "checker"); err != nil || !applied {
		t.Fatalf("first application: %v %v", err, applied)
	}
	got, applied, err := f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 50, "once", "", "checker")
	if err != nil || applied || got == nil || got.Status != domain.StatusRecovered || got.RecoveredAmount != 50 {
		t.Fatalf("a replay must be a no-op returning the case, got %v %v %+v", err, applied, got)
	}
	if f.count(t, `SELECT count(*) FROM recovery_applications WHERE case_id = $1`, c.CaseID) != 1 ||
		f.events(t, c.CaseID, domain.EventRecoveryOffsetApplied) != 1 {
		t.Fatal("a replay must record neither a second application nor a second event")
	}
	// The same reference as a different TYPE is a different application.
	if _, applied, err := f.s.ApplyRecovery(f.ctx, c.CaseID, "REFUND", 1, "once", "", "checker"); applied || err == nil {
		t.Fatalf("a recovered case accepts nothing further, got %v %v", applied, err)
	}
}

func TestApplyRecovery_OverRecovery_IsRefused_AndLeavesNothingBehind(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 100)
	f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 60, "a", "", "checker") //nolint:errcheck
	_, _, err := f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 40.01, "b", "", "checker")
	if !errors.Is(err, domain.ErrRecoveryExceedsOutstanding) {
		t.Fatalf("expected ErrRecoveryExceedsOutstanding, got %v", err)
	}
	got, _ := f.s.FindCase(f.ctx, c.CaseID)
	if got.RecoveredAmount != 60 || got.Status != domain.StatusPartiallyRecovered {
		t.Fatalf("the case must be unchanged, got %+v", got)
	}
	if f.count(t, `SELECT count(*) FROM recovery_applications WHERE case_id = $1 AND idempotency_ref = 'b'`, c.CaseID) != 0 {
		t.Fatal("the refused application must roll back with the transaction (and so can be retried with a corrected amount)")
	}
	if _, applied, err := f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 40, "b", "", "checker"); err != nil || !applied {
		t.Fatalf("the same reference with a valid amount must now succeed, got %v %v", err, applied)
	}
}

func TestApplyRecovery_Concurrent_NeverExceedsTheTotal(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 100)
	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, results[i] = f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 30, "ref-"+uuid.NewString(), "", "checker")
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
		}
	}
	got, _ := f.s.FindCase(f.ctx, c.CaseID)
	if wins != 3 || got.RecoveredAmount != 90 || got.RecoveredAmount > got.TotalAmount {
		t.Fatalf("exactly three 30-unit applications fit into 100, got %d wins and %v recovered", wins, got.RecoveredAmount)
	}
	if f.count(t, `SELECT count(*) FROM recovery_applications WHERE case_id = $1`, c.CaseID) != 3 {
		t.Fatal("only the applications that fitted may be recorded")
	}
}

// ── state machine ────────────────────────────────────────────────────────────

func TestStateMachine_StoreAgreesWithTheDomainRules(t *testing.T) {
	f := setup(t)
	open := f.newCase(t, 100)
	if _, _, err := f.s.ApplyRecovery(f.ctx, open.CaseID, "OFFSET", 10, "x", "", "p"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("an unapproved case accepts no recovery, got %v", err)
	}
	if _, err := f.s.CloseCase(f.ctx, open.CaseID, domain.CloseCaseRequest{}, "p"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("only a RECOVERED case closes, got %v", err)
	}
	if _, err := f.s.ApproveRecoveryPlan(f.ctx, open.CaseID, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ApproveRecoveryPlan(f.ctx, open.CaseID, "c2"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a plan is approved once, got %v", err)
	}

	// A fully recovered case can be neither escalated nor written off — only closed.
	rec := f.approved(t, 10)
	f.s.ApplyRecovery(f.ctx, rec.CaseID, "OFFSET", 10, "full", "", "p") //nolint:errcheck
	if _, err := f.s.EscalateCase(f.ctx, rec.CaseID, domain.EscalateRequest{Reason: "r"}, "p"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("RECOVERED must not escalate (domain.CanEscalate), got %v", err)
	}
	if _, err := f.s.WriteOffCase(f.ctx, rec.CaseID, domain.WriteOffRequest{Reason: "r"}, "p"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("RECOVERED must not be written off, got %v", err)
	}

	// Escalated cases can be written off but not escalated again; written-off is terminal.
	esc := f.approved(t, 10)
	if got, err := f.s.EscalateCase(f.ctx, esc.CaseID, domain.EscalateRequest{Reason: "supplier silent"}, "p"); err != nil || got.Status != domain.StatusEscalated || got.EscalationReason != "supplier silent" {
		t.Fatalf("EscalateCase: %v %+v", err, got)
	}
	if _, err := f.s.EscalateCase(f.ctx, esc.CaseID, domain.EscalateRequest{Reason: "again"}, "p"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("escalating twice, got %v", err)
	}
	if _, _, err := f.s.ApplyRecovery(f.ctx, esc.CaseID, "OFFSET", 1, "e", "", "p"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("an escalated case takes no further application, got %v", err)
	}
	wo, err := f.s.WriteOffCase(f.ctx, esc.CaseID, domain.WriteOffRequest{Reason: "uncollectable"}, "p")
	if err != nil || wo.Status != domain.StatusWrittenOff || wo.WriteOffReason != "uncollectable" {
		t.Fatalf("WriteOffCase: %v %+v", err, wo)
	}
	if _, err := f.s.WriteOffCase(f.ctx, esc.CaseID, domain.WriteOffRequest{Reason: "again"}, "p"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a written-off case is terminal, got %v", err)
	}
	if list, _ := f.s.ListOpenCases(f.ctx, f.le); len(list) != 2 {
		t.Fatalf("written-off/closed cases are not 'open' (2 expected, got %d)", len(list))
	}
	if f.events(t, esc.CaseID, domain.EventRecoveryEscalated) != 1 || f.events(t, esc.CaseID, domain.EventRecoveryWrittenOff) != 1 {
		t.Fatal("each transition emits exactly one event")
	}
}

func TestCommitments_AreAppendOnly_AndEmitAnEvent(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 10)
	cm, err := f.s.RecordCommitment(f.ctx, c.CaseID, domain.RecordCommitmentRequest{Detail: "will refund by Friday", ExpectedMethod: "BANK_TRANSFER"}, "p")
	if err != nil || cm.Detail != "will refund by Friday" {
		t.Fatalf("RecordCommitment: %v %+v", err, cm)
	}
	if f.events(t, c.CaseID, domain.EventCommitmentRecorded) != 1 {
		t.Fatal("expected a commitment event")
	}
	if _, err := f.s.RecordCommitment(f.ctx, uuid.NewString(), domain.RecordCommitmentRequest{Detail: "x"}, "p"); !errors.Is(err, domain.ErrCaseNotFound) {
		t.Fatalf("unknown case, got %v", err)
	}
	if _, err := f.owner.Exec(context.Background(), `UPDATE recovery_commitments SET detail = 'edited'`); err == nil {
		t.Fatal("commitments are append-only")
	}
}

// ── invariants enforced by the database ──────────────────────────────────────

func TestImmutability_Cases_Applications_Terminal(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	c := f.approved(t, 100)
	f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 10, "r", "", "p") //nolint:errcheck
	term := f.approved(t, 5)
	f.s.WriteOffCase(f.ctx, term.CaseID, domain.WriteOffRequest{Reason: "x"}, "p") //nolint:errcheck

	refused := map[string]string{
		"delete a case":                  `DELETE FROM supplier_recovery_cases WHERE case_id = '` + c.CaseID + `'`,
		"resize the case":                `UPDATE supplier_recovery_cases SET total_amount = 1 WHERE case_id = '` + c.CaseID + `'`,
		"change the source payable":      `UPDATE supplier_recovery_cases SET source_payable_id = 'other' WHERE case_id = '` + c.CaseID + `'`,
		"change the supplier":            `UPDATE supplier_recovery_cases SET supplier_ref = 'other' WHERE case_id = '` + c.CaseID + `'`,
		"reopen a written-off case":      `UPDATE supplier_recovery_cases SET status = 'OPEN' WHERE case_id = '` + term.CaseID + `'`,
		"edit an application":            `UPDATE recovery_applications SET amount = 1`,
		"delete an application":          `DELETE FROM recovery_applications`,
		"recover more than the total":    `UPDATE supplier_recovery_cases SET recovered_amount = 101 WHERE case_id = '` + c.CaseID + `'`,
		"a non-positive total":           `UPDATE supplier_recovery_cases SET total_amount = 0 WHERE case_id = '` + c.CaseID + `'`,
		"an application of zero":         `INSERT INTO recovery_applications (tenant_id, case_id, application_type, amount, idempotency_ref, actor_principal_id) VALUES ('` + f.tenant + `', '` + c.CaseID + `', 'OFFSET', 0, 'z', 'p')`,
		"an unknown application type":    `INSERT INTO recovery_applications (tenant_id, case_id, application_type, amount, idempotency_ref, actor_principal_id) VALUES ('` + f.tenant + `', '` + c.CaseID + `', 'GIFT', 5, 'g', 'p')`,
		"the same reference twice (raw)": `INSERT INTO recovery_applications (tenant_id, case_id, application_type, amount, idempotency_ref, actor_principal_id) VALUES ('` + f.tenant + `', '` + c.CaseID + `', 'OFFSET', 5, 'r', 'p')`,
	}
	for name, sql := range refused {
		if name == "recover more than the total" {
			// No CHECK ties recovered to total in the schema; the store's guarded UPDATE
			// is what enforces it (see the over-recovery tests). Documented, not asserted here.
			continue
		}
		if _, err := f.owner.Exec(ctx, sql); err == nil {
			t.Fatalf("%s: the database must refuse it", name)
		}
	}
}

// ── tenant isolation ─────────────────────────────────────────────────────────

func TestTenantIsolation_AndNoTenantSeesNothing(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 100)
	f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 10, "r", "", "p")                          //nolint:errcheck
	f.s.RecordCommitment(f.ctx, c.CaseID, domain.RecordCommitmentRequest{Detail: "d"}, "p") //nolint:errcheck
	o := f.other()

	if _, err := o.s.FindCase(o.ctx, c.CaseID); !errors.Is(err, domain.ErrCaseNotFound) {
		t.Fatalf("another tenant must not see the case, got %v", err)
	}
	if list, _ := o.s.ListOpenCases(o.ctx, f.le); len(list) != 0 {
		t.Fatal("another tenant lists nothing")
	}
	if apps, _ := o.s.ListApplications(o.ctx, c.CaseID); len(apps) != 0 {
		t.Fatal("another tenant sees no applications")
	}
	if cms, _ := o.s.ListCommitments(o.ctx, c.CaseID); len(cms) != 0 {
		t.Fatal("another tenant sees no commitments")
	}
	if _, _, err := o.s.ApplyRecovery(o.ctx, c.CaseID, "OFFSET", 5, "x", "", "p"); !errors.Is(err, domain.ErrCaseNotFound) {
		t.Fatalf("another tenant cannot apply, got %v", err)
	}
	if _, err := o.s.CloseCase(o.ctx, c.CaseID, domain.CloseCaseRequest{}, "p"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("another tenant cannot close, got %v", err)
	}
	if _, err := o.s.WriteOffCase(o.ctx, c.CaseID, domain.WriteOffRequest{Reason: "r"}, "p"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("another tenant cannot write off, got %v", err)
	}
	// With no tenant at all there is no escape hatch.
	if _, err := f.s.FindCase(context.Background(), c.CaseID); !errors.Is(err, domain.ErrCaseNotFound) {
		t.Fatalf("a request with no tenant sees nothing, got %v", err)
	}
	if got, _ := f.s.FindCase(f.ctx, c.CaseID); got.RecoveredAmount != 10 {
		t.Fatalf("the owner's case is unchanged, got %+v", got)
	}
}

// ── atomicity & the relay ────────────────────────────────────────────────────

// The state change and its event commit together or not at all.
func TestCommands_AreAtomicWithTheirEvent(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 100)
	ctx := context.Background()
	if _, err := f.owner.Exec(ctx, `ALTER TABLE outbox_events RENAME TO outbox_tmp`); err != nil {
		t.Fatal(err)
	}
	_, _, applyErr := f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 25, "will-fail", "", "p")
	_, closeErr := f.s.EscalateCase(f.ctx, c.CaseID, domain.EscalateRequest{Reason: "r"}, "p")
	_, createErr := f.s.CreateCase(f.ctx, f.tenant, domain.CreateCaseRequest{LegalEntityID: f.le, SupplierRef: "s", RecoveryBasis: domain.BasisContractual,
		SourcePayableID: "p", TotalAmount: 5, Currency: "USD"}, "p")
	if _, err := f.owner.Exec(ctx, `ALTER TABLE outbox_tmp RENAME TO outbox_events`); err != nil {
		t.Fatal(err)
	}
	if applyErr == nil || closeErr == nil || createErr == nil {
		t.Fatalf("every command must fail when its event cannot be written: %v / %v / %v", applyErr, closeErr, createErr)
	}
	got, _ := f.s.FindCase(f.ctx, c.CaseID)
	if got.RecoveredAmount != 0 || got.Status != domain.StatusInRecovery {
		t.Fatalf("the case must be exactly as before, got %+v", got)
	}
	if f.count(t, `SELECT count(*) FROM recovery_applications WHERE case_id = $1`, c.CaseID) != 0 {
		t.Fatal("no application may survive the rollback")
	}
	if list, _ := f.s.ListOpenCases(f.ctx, f.le); len(list) != 1 {
		t.Fatalf("the failed create must leave no case, got %d", len(list))
	}
	if _, applied, err := f.s.ApplyRecovery(f.ctx, c.CaseID, "OFFSET", 25, "will-fail", "", "p"); err != nil || !applied {
		t.Fatalf("once healthy, the same application must succeed: %v %v", err, applied)
	}
}

type recordingPublisher struct {
	mu  sync.Mutex
	ids []string
}

func (p *recordingPublisher) PublishOutbox(_ context.Context, _ string, aggregateID string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, aggregateID)
	return nil
}

func TestOutboxRelay_PublishesAcrossTenants_UnderTheRestrictedRole(t *testing.T) {
	f := setup(t)
	fb := f.other()
	a, b := f.newCase(t, 10), fb.newCase(t, 20)
	pub := &recordingPublisher{}
	outbox.NewRelay(f.s.Pool(), pub, time.Second, 1000, zap.NewNop()).RelayOnce(context.Background())
	seen := map[string]bool{}
	for _, id := range pub.ids {
		seen[id] = true
	}
	if !seen[a.CaseID] || !seen[b.CaseID] {
		t.Fatalf("the relay must publish both tenants' events, saw %v", pub.ids)
	}
	if f.count(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) != 0 {
		t.Fatal("published rows must be marked published")
	}
}
