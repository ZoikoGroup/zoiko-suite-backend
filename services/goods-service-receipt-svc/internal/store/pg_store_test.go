package store

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

	"zoiko.io/goods-service-receipt-svc/internal/domain"
	"zoiko.io/goods-service-receipt-svc/internal/idempotency"
	"zoiko.io/goods-service-receipt-svc/internal/middleware"
	"zoiko.io/goods-service-receipt-svc/internal/outbox"
)

// These suites need a database: TEST_DATABASE_URL names the OWNER role (it applies
// the migrations from a clean slate and runs the verification queries). When
// TEST_APP_DATABASE_URL names an ordinary NOSUPERUSER NOBYPASSRLS role the STORE
// runs as that role instead — the role the service has once DB_USER names one —
// so the row-level-security policies actually bind it. Both refuse to run unless
// the database name contains "test": they DROP this service's tables.

func openOwnerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	requireDisposable(t, dsn)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	for _, tbl := range []string{"po_progress_pushes", "accounting_posting_requests", "receipt_accounting_events", "receipt_reversals",
		"receipt_evidence", "goods_service_receipts", "idempotency_keys", "outbox_events"} {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl+` CASCADE`)
	}
	for _, fn := range []string{"reject_receipt_mutation", "reject_evidence_mutation", "guard_posting_request"} {
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

func requireDisposable(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil || !strings.Contains(strings.ToLower(strings.TrimPrefix(u.Path, "/")), "test") {
		t.Fatalf("refusing to run: %q is not recognisably a disposable test database", dsn)
	}
}

type fixture struct {
	owner  *pgxpool.Pool
	s      *PgStore
	tenant string
	ctx    context.Context
}

func setup(t *testing.T) fixture {
	t.Helper()
	owner := openOwnerPool(t)
	storePool := owner
	if appDSN := os.Getenv("TEST_APP_DATABASE_URL"); appDSN != "" {
		requireDisposable(t, appDSN)
		app, err := pgxpool.New(context.Background(), appDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Close)
		storePool = app
	}
	tenant := uuid.NewString()
	return fixture{owner: owner, s: NewPgStore(storePool, zap.NewNop()), tenant: tenant,
		ctx: middleware.WithTenant(context.Background(), tenant)}
}

func (f fixture) as(tenant string) context.Context {
	return middleware.WithTenant(context.Background(), tenant)
}

func cmd(principal string) domain.Command {
	return domain.Command{PrincipalID: principal, CorrelationID: "corr-" + uuid.NewString()}
}

func (f fixture) newReceipt(t *testing.T, line *string, qty, amount float64) *domain.GoodsServiceReceipt {
	t.Helper()
	req := domain.CreateReceiptRequest{
		LegalEntityID: uuid.NewString(), PurchaseOrderID: uuid.NewString(), ReceiptType: domain.ReceiptTypeGoods, Quantity: qty,
		UnitOfMeasure: "EA", Amount: amount, CurrencyCode: "USD", ReceiptDate: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC),
		Location: "wh-1",
	}
	if line != nil {
		req.POLineID = *line
	}
	r, err := f.s.CreateReceipt(f.ctx, f.tenant, req, cmd("receiver"))
	if err != nil {
		t.Fatalf("CreateReceipt: %v", err)
	}
	return r
}

// onPO creates a further receipt against the same PO (and entity) as base.
func (f fixture) onPO(t *testing.T, base *domain.GoodsServiceReceipt, line *string, qty, amount float64) *domain.GoodsServiceReceipt {
	t.Helper()
	req := domain.CreateReceiptRequest{
		LegalEntityID: base.LegalEntityID, PurchaseOrderID: base.PurchaseOrderID, ReceiptType: domain.ReceiptTypeGoods, Quantity: qty,
		UnitOfMeasure: "EA", Amount: amount, CurrencyCode: "USD", ReceiptDate: base.ReceiptDate,
	}
	if line != nil {
		req.POLineID = *line
	}
	r, err := f.s.CreateReceipt(f.ctx, f.tenant, req, cmd("receiver"))
	if err != nil {
		t.Fatalf("CreateReceipt: %v", err)
	}
	return r
}

func lineLimits(ordered, open float64) domain.ConfirmLimits {
	return domain.ConfirmLimits{LineOrderedQuantity: ordered, LineOpenReceiptQuantity: open}
}

func (f fixture) confirm(t *testing.T, r *domain.GoodsServiceReceipt, lim domain.ConfirmLimits) *domain.ConfirmResult {
	t.Helper()
	v := r.Version
	rev := 2
	c := cmd("confirmer")
	c.ExpectedVersion = &v
	res, err := f.s.ConfirmReceipt(f.ctx, r.ReceiptID, domain.ConfirmInput{PORevision: &rev, Limits: lim}, c)
	if err != nil {
		t.Fatalf("ConfirmReceipt: %v", err)
	}
	return res
}

func (f fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func strp(s string) *string { return &s }

// ── lifecycle & atomic side effects ──────────────────────────────────────────

func TestLifecycle_ConfirmQueuesPushAndBalancedGRNIPostingAndEventsAtomically(t *testing.T) {
	f := setup(t)
	line := uuid.NewString()
	r := f.newReceipt(t, &line, 10, 1000)
	if r.Status != domain.StatusDraft || r.Version != 1 || r.ProgressPushStatus != domain.PushNotApplicable {
		t.Fatalf("expected a fresh DRAFT at v1, got %+v", r)
	}

	res := f.confirm(t, r, lineLimits(15, 15))
	got := res.Receipt
	if got.Status != domain.StatusConfirmed || got.Version != 2 || got.ConfirmedByPrincipalID == nil || got.ConfirmedAt == nil ||
		got.PORevision == nil || *got.PORevision != 2 || got.ProgressPushStatus != domain.PushPending {
		t.Fatalf("expected CONFIRMED v2 with the PO revision and a PENDING push, got %+v", got)
	}
	if res.Accounting == nil || res.Accounting.Status != domain.AccountingPending || res.Accounting.Direction != domain.DirectionAccrue {
		t.Fatalf("expected a PENDING ACCRUE request, got %+v", res.Accounting)
	}

	// The posting request is balanced and names mapping keys, never accounts.
	var payload []byte
	if err := f.owner.QueryRow(context.Background(), `SELECT request_payload FROM accounting_posting_requests WHERE receipt_id = $1`, r.ReceiptID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var p postingRequest
	_ = json.Unmarshal(payload, &p)
	var dr, cr float64
	for _, l := range p.Lines {
		dr += l.DebitAmount
		cr += l.CreditAmount
		if l.MappingKey == "" {
			t.Fatal("a posting line must carry an ACC-02 mapping key")
		}
	}
	if dr != 1000 || cr != 1000 || p.FiscalPeriod != "2026-10" || p.SourceEventID != r.ReceiptID || p.TransactionCurrency != "USD" {
		t.Fatalf("expected a balanced 1000 posting for 2026-10 keyed by the receipt id, got %+v", p)
	}

	if f.count(t, `SELECT count(*) FROM po_progress_pushes WHERE receipt_id = $1 AND delta_sign = 1 AND quantity = 10`, r.ReceiptID) != 1 {
		t.Fatal("expected exactly one +10 progress push")
	}
	for _, typ := range []string{domain.EventReceiptCreated, domain.AliasReceiptCreated, domain.EventReceiptConfirmed, domain.AliasReceiptConfirmed} {
		if f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, r.ReceiptID, typ) != 1 {
			t.Fatalf("expected exactly one %s event in the outbox", typ)
		}
	}
}

// Negative path 4: GRNI emitted twice on replay.
func TestConfirm_Replay_NeverQueuesTheConsequenceTwice(t *testing.T) {
	f := setup(t)
	line := uuid.NewString()
	r := f.newReceipt(t, &line, 10, 1000)
	f.confirm(t, r, lineLimits(15, 15))

	if _, err := f.s.ConfirmReceipt(f.ctx, r.ReceiptID, domain.ConfirmInput{Limits: lineLimits(15, 15)}, cmd("confirmer")); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a second confirmation must be refused, got %v", err)
	}
	for table, want := range map[string]int{"accounting_posting_requests": 1, "po_progress_pushes": 1} {
		if n := f.count(t, `SELECT count(*) FROM `+table+` WHERE receipt_id = $1`, r.ReceiptID); n != want {
			t.Fatalf("%s: expected %d row after a replay, got %d", table, want, n)
		}
	}
	if f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, r.ReceiptID, domain.EventReceiptConfirmed) != 1 {
		t.Fatal("a replayed confirmation must not emit the event again")
	}

	// And even bypassing the state machine, the unique source-event key swallows a
	// duplicate request: the same source event queues nothing.
	tx, err := f.s.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", f.tenant); err != nil {
		t.Fatal(err)
	}
	again, err := f.s.requestAccounting(f.ctx, tx, r, cmd("x"), domain.DirectionAccrue, r.ReceiptID, 1000, r.ReceiptDate)
	if err != nil || again != nil {
		t.Fatalf("a duplicate source event must queue nothing, got %v %v", again, err)
	}
}

func TestConfirm_StaleVersion_And_NotConfirmable_ChangeNothing(t *testing.T) {
	f := setup(t)
	r := f.newReceipt(t, nil, 10, 1000)
	stale := 7
	c := cmd("confirmer")
	c.ExpectedVersion = &stale
	if _, err := f.s.ConfirmReceipt(f.ctx, r.ReceiptID, domain.ConfirmInput{Limits: domain.ConfirmLimits{POTotalAmount: 5000}}, c); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}
	if f.count(t, `SELECT count(*) FROM accounting_posting_requests`) != 0 {
		t.Fatal("a refused confirmation must queue nothing")
	}
	rej, err := f.s.RejectReceipt(f.ctx, r.ReceiptID, domain.RejectReceiptRequest{Reason: "damaged"}, cmd("confirmer"))
	if err != nil || rej.Status != domain.StatusRejected || rej.Version != 2 || rej.RejectionReason != "damaged" {
		t.Fatalf("expected REJECTED v2, got %v %+v", err, rej)
	}
	if _, err := f.s.ConfirmReceipt(f.ctx, r.ReceiptID, domain.ConfirmInput{Limits: domain.ConfirmLimits{POTotalAmount: 5000}}, cmd("confirmer")); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a rejected receipt cannot be confirmed, got %v", err)
	}
}

// ── tolerance ────────────────────────────────────────────────────────────────

// Negative path 1: the over-tolerance check is authoritative inside the
// confirmation transaction, and an approved exception is the only way past it.
func TestConfirm_HeaderLevel_Tolerance_And_ApprovedException(t *testing.T) {
	f := setup(t)
	lim := domain.ConfirmLimits{POTotalAmount: 1500}
	a := f.newReceipt(t, nil, 10, 1000)
	f.confirm(t, a, lim) // 1000 of 1500
	b := f.onPO(t, a, nil, 10, 1000)
	if _, err := f.s.ConfirmReceipt(f.ctx, b.ReceiptID, domain.ConfirmInput{Limits: lim}, cmd("confirmer")); !errors.Is(err, domain.ErrOverReceiptTolerance) {
		t.Fatalf("expected ErrOverReceiptTolerance (2000 > 1500), got %v", err)
	}
	if got, _ := f.s.FindReceipt(f.ctx, b.ReceiptID); got.Status != domain.StatusDraft {
		t.Fatal("a refused confirmation must leave the receipt a DRAFT")
	}
	if _, err := f.s.ConfirmReceipt(f.ctx, b.ReceiptID, domain.ConfirmInput{Limits: lim, ToleranceExceptionRef: "EXC-9"}, cmd("confirmer")); err != nil {
		t.Fatalf("an approved exception must let it through: %v", err)
	}
	got, _ := f.s.FindReceipt(f.ctx, b.ReceiptID)
	if got.ToleranceExceptionRef != "EXC-9" {
		t.Fatalf("the exception reference must be recorded as evidence, got %q", got.ToleranceExceptionRef)
	}

	// The configured percentage widens the ceiling.
	c := f.newReceipt(t, nil, 1, 100)
	pct := domain.ConfirmLimits{POTotalAmount: 100, TolerancePct: 10}
	f.confirm(t, c, pct) // 100 <= 110
	d := f.onPO(t, c, nil, 1, 20)
	if _, err := f.s.ConfirmReceipt(f.ctx, d.ReceiptID, domain.ConfirmInput{Limits: pct}, cmd("confirmer")); !errors.Is(err, domain.ErrOverReceiptTolerance) {
		t.Fatalf("120 > 110: expected ErrOverReceiptTolerance, got %v", err)
	}
}

func TestConfirm_LineLevel_CountsUndeliveredPushes_AndUsesToleranceOfOrdered(t *testing.T) {
	f := setup(t)
	line := uuid.NewString()
	a := f.newReceipt(t, &line, 10, 1000)
	b := f.onPO(t, a, &line, 10, 1000)
	f.confirm(t, a, lineLimits(15, 15)) // 10 pending toward AP-03
	if _, err := f.s.ConfirmReceipt(f.ctx, b.ReceiptID, domain.ConfirmInput{Limits: lineLimits(15, 15)}, cmd("confirmer")); !errors.Is(err, domain.ErrOverReceiptTolerance) {
		t.Fatalf("open 15 less 10 pending cannot take 10: expected ErrOverReceiptTolerance, got %v", err)
	}
	// 20% of the ordered 15 = 3 extra: 5 available + 3 = 8 < 10, still refused ...
	tol := lineLimits(15, 15)
	tol.TolerancePct = 20
	if _, err := f.s.ConfirmReceipt(f.ctx, b.ReceiptID, domain.ConfirmInput{Limits: tol}, cmd("confirmer")); !errors.Is(err, domain.ErrOverReceiptTolerance) {
		t.Fatalf("expected refusal at 8 available, got %v", err)
	}
	// ... 50% = 7.5 extra: 5 + 7.5 = 12.5 >= 10 passes.
	tol.TolerancePct = 50
	if _, err := f.s.ConfirmReceipt(f.ctx, b.ReceiptID, domain.ConfirmInput{Limits: tol}, cmd("confirmer")); err != nil {
		t.Fatalf("within tolerance: %v", err)
	}
}

// Two competing confirmations for the last open capacity of a line: the advisory
// lock serializes them, so exactly one wins.
func TestConfirm_Concurrent_CompetingForTheSameLine_OneWins(t *testing.T) {
	f := setup(t)
	for round := 0; round < 4; round++ {
		line := uuid.NewString()
		a := f.newReceipt(t, &line, 10, 1000)
		b := f.onPO(t, a, &line, 10, 1000)
		var wg sync.WaitGroup
		results := make([]error, 2)
		for i, r := range []*domain.GoodsServiceReceipt{a, b} {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				_, results[i] = f.s.ConfirmReceipt(f.ctx, id, domain.ConfirmInput{Limits: lineLimits(15, 15)}, cmd("confirmer"))
			}(i, r.ReceiptID)
		}
		wg.Wait()
		wins := 0
		for _, err := range results {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, domain.ErrOverReceiptTolerance):
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: exactly one of two competing 10-unit confirmations fits 15 open; %d succeeded (%v)", round, wins, results)
		}
	}
}

// ── reversal ─────────────────────────────────────────────────────────────────

func TestReverse_PartialThenFull_AccumulatesAndQueuesMirroredConsequences(t *testing.T) {
	f := setup(t)
	line := uuid.NewString()
	r := f.newReceipt(t, &line, 10, 1000)
	f.confirm(t, r, lineLimits(10, 10))

	r1, rev1, err := f.s.ReverseReceipt(f.ctx, r.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 400, Reason: "damaged"}, cmd("reverser"))
	if err != nil || r1.Status != domain.StatusPartiallyReversed || r1.ReversedAmount != 400 || r1.ReversedQuantity != 4 || rev1.ReversedQuantity != 4 || r1.Version != 3 {
		t.Fatalf("expected PARTIALLY_REVERSED 400 / 4 at v3, got %v %+v", err, r1)
	}
	r2, rev2, err := f.s.ReverseReceipt(f.ctx, r.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 600, Reason: "rest"}, cmd("reverser"))
	if err != nil || r2.Status != domain.StatusFullyReversed || r2.ReversedQuantity != 10 {
		t.Fatalf("expected FULLY_REVERSED with all 10 reversed (no rounding residue), got %v %+v", err, r2)
	}
	if _, _, err := f.s.ReverseReceipt(f.ctx, r.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 1, Reason: "x"}, cmd("reverser")); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a fully reversed receipt is terminal, got %v", err)
	}

	if f.count(t, `SELECT count(*) FROM po_progress_pushes WHERE receipt_id = $1 AND delta_sign = -1`, r.ReceiptID) != 2 {
		t.Fatal("expected two negative pushes, one per reversal")
	}
	for _, src := range []string{r.ReceiptID + ":reversal:" + rev1.ReversalID, r.ReceiptID + ":reversal:" + rev2.ReversalID} {
		if f.count(t, `SELECT count(*) FROM accounting_posting_requests WHERE source_event_id = $1 AND direction = 'REVERSE'`, src) != 1 {
			t.Fatalf("expected a REVERSE posting request for %s", src)
		}
	}
	// The reversal mirrors the accrual: debit and credit swap sides.
	var payload []byte
	_ = f.owner.QueryRow(context.Background(), `SELECT request_payload FROM accounting_posting_requests WHERE source_event_id = $1`,
		r.ReceiptID+":reversal:"+rev1.ReversalID).Scan(&payload)
	var p postingRequest
	_ = json.Unmarshal(payload, &p)
	if len(p.Lines) != 2 || p.Lines[0].MappingKey != DefaultAccountingConfig().CreditMappingKey || p.Lines[0].DebitAmount != 400 ||
		p.Lines[1].MappingKey != DefaultAccountingConfig().DebitMappingKey || p.Lines[1].CreditAmount != 400 {
		t.Fatalf("expected the accrual's mirror image for 400, got %+v", p.Lines)
	}
	if f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, r.ReceiptID, domain.EventReceiptReversed) != 2 {
		t.Fatal("expected one reversed event per reversal")
	}
}

func TestReverse_OverReversal_NotConfirmed_AndStale(t *testing.T) {
	f := setup(t)
	draft := f.newReceipt(t, nil, 10, 1000)
	if _, _, err := f.s.ReverseReceipt(f.ctx, draft.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 10, Reason: "x"}, cmd("r")); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("an unconfirmed receipt cannot be reversed, got %v", err)
	}
	f.confirm(t, draft, domain.ConfirmLimits{POTotalAmount: 5000})
	if _, _, err := f.s.ReverseReceipt(f.ctx, draft.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 1000.01, Reason: "x"}, cmd("r")); !errors.Is(err, domain.ErrOverReversal) {
		t.Fatalf("expected ErrOverReversal, got %v", err)
	}
	q := 11.0
	if _, _, err := f.s.ReverseReceipt(f.ctx, draft.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 100, ReversedQuantity: &q, Reason: "x"}, cmd("r")); !errors.Is(err, domain.ErrOverReversal) {
		t.Fatalf("reversing more units than received must be refused, got %v", err)
	}
	stale := 1
	c := cmd("r")
	c.ExpectedVersion = &stale
	if _, _, err := f.s.ReverseReceipt(f.ctx, draft.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 10, Reason: "x"}, c); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}
	got, _ := f.s.FindReceipt(f.ctx, draft.ReceiptID)
	if got.ReversedAmount != 0 || f.count(t, `SELECT count(*) FROM receipt_reversals`) != 0 {
		t.Fatal("refused reversals must change nothing")
	}
}

// ── immutability (negative path 3) ───────────────────────────────────────────

func TestImmutability_ConfirmedReceiptCannotBeDeletedOrEdited(t *testing.T) {
	f := setup(t)
	r := f.newReceipt(t, nil, 10, 1000)
	f.confirm(t, r, domain.ConfirmLimits{POTotalAmount: 5000})
	ctx := context.Background()

	// Even the table owner, as a superuser would be, is refused by the triggers.
	for name, sql := range map[string]string{
		"delete":               `DELETE FROM goods_service_receipts WHERE receipt_id = $1`,
		"edit quantity":        `UPDATE goods_service_receipts SET quantity = 1 WHERE receipt_id = $1`,
		"edit amount":          `UPDATE goods_service_receipts SET amount = 1 WHERE receipt_id = $1`,
		"edit confirmer":       `UPDATE goods_service_receipts SET confirmed_by_principal_id = 'someone' WHERE receipt_id = $1`,
		"re-point the PO line": `UPDATE goods_service_receipts SET po_line_id = gen_random_uuid() WHERE receipt_id = $1`,
		"change the exception": `UPDATE goods_service_receipts SET tolerance_exception_ref = 'x' WHERE receipt_id = $1`,
	} {
		if _, err := f.owner.Exec(ctx, sql, r.ReceiptID); err == nil {
			t.Fatalf("%s: the database must refuse it", name)
		}
	}
	if got, _ := f.s.FindReceipt(f.ctx, r.ReceiptID); got.Quantity != 10 || got.Amount != 1000 {
		t.Fatalf("the confirmed content must be untouched, got %+v", got)
	}

	// A reversal is the correction; its evidence rows and the posting request are append-only.
	if _, _, err := f.s.ReverseReceipt(f.ctx, r.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 1000, Reason: "full"}, cmd("r")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE goods_service_receipts SET status = 'CONFIRMED' WHERE receipt_id = $1`, r.ReceiptID); err == nil {
		t.Fatal("a terminal FULLY_REVERSED receipt must be frozen")
	}
	if _, err := f.owner.Exec(ctx, `DELETE FROM receipt_reversals`); err == nil {
		t.Fatal("reversal rows are append-only")
	}
	if _, err := f.owner.Exec(ctx, `DELETE FROM accounting_posting_requests`); err == nil {
		t.Fatal("posting requests are never deleted")
	}
	if _, err := f.owner.Exec(ctx, `UPDATE accounting_posting_requests SET request_payload = '{}'::jsonb`); err == nil {
		t.Fatal("the posting request itself is immutable evidence")
	}
}

// ── draft-stage commands, acceptance, evidence ───────────────────────────────

func TestAmendDraft_AcceptanceAndEvidence(t *testing.T) {
	f := setup(t)
	r := f.newReceipt(t, nil, 10, 1000)
	q := 12.0
	am, err := f.s.AmendReceiptDraft(f.ctx, r.ReceiptID, domain.AmendReceiptDraftRequest{Quantity: &q}, cmd("receiver"))
	if err != nil || am.Quantity != 12 || am.Version != 2 {
		t.Fatalf("expected quantity 12 at v2, got %v %+v", err, am)
	}

	svc, err := f.s.CreateReceipt(f.ctx, f.tenant, domain.CreateReceiptRequest{
		LegalEntityID: uuid.NewString(), PurchaseOrderID: uuid.NewString(), ReceiptType: domain.ReceiptTypeService, Quantity: 1,
		UnitOfMeasure: "EA", Amount: 500, CurrencyCode: "USD", ReceiptDate: time.Now(), RequiresIndependentAcceptance: true,
	}, cmd("receiver"))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := f.s.RecordServiceAcceptance(f.ctx, svc.ReceiptID, domain.RecordServiceAcceptanceRequest{EvidenceRef: "SIGNOFF", Notes: "ok"}, cmd("acceptor"))
	if err != nil || acc.Status != domain.StatusPendingConfirmation || acc.Version != 2 {
		t.Fatalf("expected PENDING_CONFIRMATION v2, got %v %+v", err, acc)
	}
	if _, err := f.s.RecordServiceAcceptance(f.ctx, svc.ReceiptID, domain.RecordServiceAcceptanceRequest{}, cmd("acceptor")); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("acceptance is recorded once, got %v", err)
	}
	if _, err := f.s.AmendReceiptDraft(f.ctx, svc.ReceiptID, domain.AmendReceiptDraftRequest{Quantity: &q}, cmd("receiver")); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("only a DRAFT can be amended, got %v", err)
	}
	if f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, svc.ReceiptID, domain.EventServiceAcceptanceRecorded) != 1 {
		t.Fatal("expected the ServiceAcceptanceRecorded event")
	}

	if _, err := f.s.AttachReceiptEvidence(f.ctx, svc.ReceiptID, domain.AttachReceiptEvidenceRequest{EvidenceRef: "DN-1", Description: "note"}, "receiver"); err != nil {
		t.Fatal(err)
	}
	ev, err := f.s.ListReceiptEvidence(f.ctx, svc.ReceiptID)
	if err != nil || len(ev) != 2 {
		t.Fatalf("expected the acceptance evidence plus the attached one, got %v %d", err, len(ev))
	}
	if _, err := f.s.AttachReceiptEvidence(f.ctx, uuid.NewString(), domain.AttachReceiptEvidenceRequest{EvidenceRef: "x"}, "p"); !errors.Is(err, domain.ErrReceiptNotFound) {
		t.Fatalf("evidence on an unknown receipt must be not found, got %v", err)
	}
}

func TestReceivedToDate_NetOfReversals(t *testing.T) {
	f := setup(t)
	lineA, lineB := uuid.NewString(), uuid.NewString()
	a := f.newReceipt(t, &lineA, 10, 1000)
	b := f.onPO(t, a, &lineB, 4, 200)
	h := f.onPO(t, a, nil, 1, 50) // header-level
	lim := lineLimits(100, 100)
	lim.POTotalAmount = 10000
	f.confirm(t, a, lim)
	f.confirm(t, b, lim)
	f.confirm(t, h, lim)
	if _, _, err := f.s.ReverseReceipt(f.ctx, a.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 300, Reason: "x"}, cmd("r")); err != nil {
		t.Fatal(err)
	}
	lines, err := f.s.ReceivedToDate(f.ctx, a.PurchaseOrderID)
	if err != nil || len(lines) != 2 {
		t.Fatalf("expected two lines, got %v %+v", err, lines)
	}
	by := map[string]domain.LineReceived{}
	for _, l := range lines {
		by[l.POLineID] = l
	}
	if by[lineA].ReceivedQuantity != 7 || by[lineA].ReceivedAmount != 700 || by[lineB].ReceivedQuantity != 4 || by[lineB].ReceivedAmount != 200 {
		t.Fatalf("expected line A net 7/700 and line B 4/200, got %+v", by)
	}
	if net, _ := f.s.SumNetConfirmedAmountForPO(f.ctx, a.PurchaseOrderID); net != 950 {
		t.Fatalf("expected a total net of 950 (700 + 200 + 50), got %v", net)
	}
	if pend, _ := f.s.PendingLineQuantity(f.ctx, lineA); pend != 7 {
		t.Fatalf("expected 7 pending on line A (10 queued, 3 reversed), got %v", pend)
	}
}

// ── tenant isolation ─────────────────────────────────────────────────────────

func TestTenantIsolation_AcrossEveryRead_And_Command(t *testing.T) {
	f := setup(t)
	line := uuid.NewString()
	r := f.newReceipt(t, &line, 10, 1000)
	f.confirm(t, r, lineLimits(15, 15))
	other := f.as(uuid.NewString())

	if _, err := f.s.FindReceipt(other, r.ReceiptID); !errors.Is(err, domain.ErrReceiptNotFound) {
		t.Fatalf("another tenant must not see the receipt, got %v", err)
	}
	if list, _ := f.s.ListReceiptsForPO(other, r.PurchaseOrderID); len(list) != 0 {
		t.Fatalf("another tenant must see no receipts for the PO, got %d", len(list))
	}
	if _, err := f.s.ConfirmReceipt(other, r.ReceiptID, domain.ConfirmInput{}, cmd("x")); !errors.Is(err, domain.ErrReceiptNotFound) {
		t.Fatalf("another tenant cannot confirm, got %v", err)
	}
	if _, _, err := f.s.ReverseReceipt(other, r.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 1, Reason: "x"}, cmd("x")); !errors.Is(err, domain.ErrReceiptNotFound) {
		t.Fatalf("another tenant cannot reverse, got %v", err)
	}
	if _, err := f.s.AttachReceiptEvidence(other, r.ReceiptID, domain.AttachReceiptEvidenceRequest{EvidenceRef: "x"}, "x"); !errors.Is(err, domain.ErrReceiptNotFound) {
		t.Fatalf("another tenant cannot attach evidence, got %v", err)
	}
	if n, _ := f.s.RequeueAccounting(other, r.ReceiptID); n != 0 {
		t.Fatalf("another tenant cannot requeue, got %d", n)
	}
	if ev, _ := f.s.ListAccountingRequests(other, r.ReceiptID); len(ev) != 0 {
		t.Fatalf("another tenant must not see posting requests, got %d", len(ev))
	}
	if lines, _ := f.s.ReceivedToDate(other, r.PurchaseOrderID); len(lines) != 0 {
		t.Fatalf("another tenant must see no received-to-date, got %+v", lines)
	}
	if pend, _ := f.s.PendingLineQuantity(other, line); pend != 0 {
		t.Fatalf("another tenant must not see pending pushes, got %v", pend)
	}
	if _, err := f.s.FindReceipt(context.Background(), r.ReceiptID); !errors.Is(err, domain.ErrTenantScopeMissing) {
		t.Fatalf("no tenant scope must be refused, got %v", err)
	}
	if got, err := f.s.FindReceipt(f.ctx, r.ReceiptID); err != nil || got.ReceiptID != r.ReceiptID {
		t.Fatalf("the owner must still see it, got %v", err)
	}
}

// ── GRNI dispatch ────────────────────────────────────────────────────────────

func TestDispatch_CrossTenant_PostsOnce_QuarantinesRefusals_BacksOffTransients_Requeues(t *testing.T) {
	f := setup(t)
	tenantB := uuid.NewString()
	a := f.newReceipt(t, nil, 10, 1000)
	f.confirm(t, a, domain.ConfirmLimits{POTotalAmount: 5000})
	fb := fixture{owner: f.owner, s: f.s, tenant: tenantB, ctx: f.as(tenantB)}
	b := fb.newReceipt(t, nil, 5, 500)
	fb.confirm(t, b, domain.ConfirmLimits{POTotalAmount: 5000})
	c := f.newReceipt(t, nil, 2, 200)
	f.confirm(t, c, domain.ConfirmLimits{POTotalAmount: 5000})

	var mu sync.Mutex
	calls := map[string]int{}
	post := func(_ context.Context, r PostingRequest) PostingResult {
		mu.Lock()
		defer mu.Unlock()
		calls[r.SourceEventID]++
		switch r.SourceEventID {
		case a.ReceiptID:
			return PostingResult{Final: true, Posted: true, ExecutionID: "exec-A", JournalID: "journal-A"}
		case b.ReceiptID:
			return PostingResult{Final: true, Err: "ledger refused (422): no ACC-02 mapping"}
		default:
			return PostingResult{Err: "ledger answered 503"}
		}
	}
	n, err := f.s.DispatchPending(context.Background(), 100, post)
	if err != nil || n != 1 {
		t.Fatalf("expected 1 POSTED across tenants, got %d %v", n, err)
	}
	ra, _ := f.s.ListAccountingRequests(f.ctx, a.ReceiptID)
	rb, _ := fb.s.ListAccountingRequests(fb.ctx, b.ReceiptID)
	rc, _ := f.s.ListAccountingRequests(f.ctx, c.ReceiptID)
	if ra[0].Status != domain.AccountingPosted || *ra[0].PostingExecutionID != "exec-A" || *ra[0].JournalID != "journal-A" {
		t.Fatalf("tenant A's request should be POSTED with its execution and journal, got %+v", ra[0])
	}
	if rb[0].Status != domain.AccountingQuarantined || !strings.Contains(rb[0].FailureReason, "no ACC-02 mapping") {
		t.Fatalf("a definitive refusal should be QUARANTINED with the reason, got %+v", rb[0])
	}
	if rc[0].Status != domain.AccountingPending || rc[0].Attempts != 1 {
		t.Fatalf("a transient failure counts an attempt and stays PENDING, got %+v", rc[0])
	}

	// A second pass: POSTED is never re-posted, QUARANTINED is not PENDING, and the
	// backed-off transient is not yet due.
	if _, err := f.s.DispatchPending(context.Background(), 100, post); err != nil {
		t.Fatal(err)
	}
	if calls[a.ReceiptID] != 1 || calls[b.ReceiptID] != 1 || calls[c.ReceiptID] != 1 {
		t.Fatalf("each request is submitted once until due again, got %v", calls)
	}

	// Requeue returns the QUARANTINED request to PENDING, never a POSTED one.
	if n, _ := f.s.RequeueAccounting(f.ctx, a.ReceiptID); n != 0 {
		t.Fatalf("a POSTED request must never be requeued, got %d", n)
	}
	if n, err := fb.s.RequeueAccounting(fb.ctx, b.ReceiptID); err != nil || n != 1 {
		t.Fatalf("expected 1 requeued, got %d %v", n, err)
	}
	post2 := func(_ context.Context, r PostingRequest) PostingResult {
		if r.SourceEventID == b.ReceiptID {
			return PostingResult{Final: true, Posted: true, ExecutionID: "exec-B"}
		}
		return PostingResult{Err: "not mine"}
	}
	if _, err := f.s.DispatchPending(context.Background(), 100, post2); err != nil {
		t.Fatal(err)
	}
	if rb, _ = fb.s.ListAccountingRequests(fb.ctx, b.ReceiptID); rb[0].Status != domain.AccountingPosted {
		t.Fatalf("the requeued request should now be POSTED, got %+v", rb[0])
	}
}

func TestDispatch_TransientFailures_BecomeFailedAfterTheCap(t *testing.T) {
	f := setup(t)
	r := f.newReceipt(t, nil, 10, 1000)
	f.confirm(t, r, domain.ConfirmLimits{POTotalAmount: 5000})
	for i := 0; i < MaxPostingAttempts; i++ {
		if _, err := f.owner.Exec(context.Background(), `UPDATE accounting_posting_requests SET next_attempt_at = NOW() WHERE status = 'PENDING'`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.DispatchPending(context.Background(), 10, func(context.Context, PostingRequest) PostingResult {
			return PostingResult{Err: "ledger unreachable"}
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := f.s.ListAccountingRequests(f.ctx, r.ReceiptID)
	if got[0].Status != domain.AccountingFailed || got[0].Attempts != MaxPostingAttempts {
		t.Fatalf("expected FAILED after %d attempts (visible, not silent), got %+v", MaxPostingAttempts, got[0])
	}
	if cur, _ := f.s.FindReceipt(f.ctx, r.ReceiptID); cur.Status != domain.StatusConfirmed {
		t.Fatal("an accounting failure must never undo the confirmed receipt")
	}
}

// ── AP-03 progress pushes ────────────────────────────────────────────────────

func TestPush_Delivery_DeliveredFailedTransient_AndReversalHeldBehindOriginal(t *testing.T) {
	f := setup(t)
	line := uuid.NewString()
	r := f.newReceipt(t, &line, 10, 1000)
	f.confirm(t, r, lineLimits(10, 10))
	if _, _, err := f.s.ReverseReceipt(f.ctx, r.ReceiptID, domain.ReverseReceiptRequest{ReversedAmount: 400, Reason: "x"}, cmd("r")); err != nil {
		t.Fatal(err)
	}

	var seen []domain.ProgressPush
	// The original +10 fails transiently first: the -4 reversal must be held back,
	// so AP-03 never sees received quantity go below zero.
	n, err := f.s.DeliverPendingPushes(context.Background(), 10, func(_ context.Context, p domain.ProgressPush) error {
		seen = append(seen, p)
		return domain.ErrPurchaseOrderServiceUnavailable
	})
	if err != nil || n != 0 || len(seen) != 1 || seen[0].DeltaSign != 1 || seen[0].Quantity != 10 {
		t.Fatalf("only the original +10 may be attempted first, got n=%d %+v %v", n, seen, err)
	}
	var attempts int
	var status string
	_ = f.owner.QueryRow(context.Background(), `SELECT status, attempts FROM po_progress_pushes WHERE receipt_id = $1 AND delta_sign = 1`, r.ReceiptID).Scan(&status, &attempts)
	if status != "PENDING" || attempts != 1 {
		t.Fatalf("a transient failure counts an attempt and stays PENDING, got %s/%d", status, attempts)
	}

	// Due again: the original delivers, then the reversal follows on the next pass.
	_, _ = f.owner.Exec(context.Background(), `UPDATE po_progress_pushes SET next_attempt_at = NOW()`)
	seen = nil
	deliverOK := func(_ context.Context, p domain.ProgressPush) error { seen = append(seen, p); return nil }
	if n, err := f.s.DeliverPendingPushes(context.Background(), 10, deliverOK); err != nil || n != 1 || len(seen) != 1 || seen[0].DeltaSign != 1 {
		t.Fatalf("expected the +10 delivered alone, got n=%d %+v %v", n, seen, err)
	}
	seen = nil
	if n, err := f.s.DeliverPendingPushes(context.Background(), 10, deliverOK); err != nil || n != 1 || len(seen) != 1 || seen[0].DeltaSign != -1 || seen[0].Quantity != 4 {
		t.Fatalf("expected the -4 reversal delivered next, got n=%d %+v %v", n, seen, err)
	}
	if got, _ := f.s.FindReceipt(f.ctx, r.ReceiptID); got.ProgressPushStatus != domain.PushDelivered {
		t.Fatalf("expected DELIVERED, got %s", got.ProgressPushStatus)
	}
}

// A PROGRESS_EXCEEDS_ORDER refusal from AP-03 is permanent: FAILED, visible on the
// receipt as progress_push_status, never silently dropped.
func TestPush_ProgressExceedsOrder_MarksFailedAndIsVisibleOnTheReceipt(t *testing.T) {
	f := setup(t)
	line := uuid.NewString()
	r := f.newReceipt(t, &line, 10, 1000)
	f.confirm(t, r, lineLimits(10, 10))
	if _, err := f.s.DeliverPendingPushes(context.Background(), 10, func(context.Context, domain.ProgressPush) error {
		return domain.ErrProgressExceedsOrder
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.s.FindReceipt(f.ctx, r.ReceiptID)
	if got.ProgressPushStatus != domain.PushFailed {
		t.Fatalf("expected FAILED, got %s", got.ProgressPushStatus)
	}
	var lastErr string
	_ = f.owner.QueryRow(context.Background(), `SELECT last_error FROM po_progress_pushes WHERE receipt_id = $1`, r.ReceiptID).Scan(&lastErr)
	if !strings.Contains(lastErr, "PROGRESS_EXCEEDS_ORDER") {
		t.Fatalf("the reason must be kept, got %q", lastErr)
	}
	if n, _ := f.s.DeliverPendingPushes(context.Background(), 10, func(context.Context, domain.ProgressPush) error { return nil }); n != 0 {
		t.Fatal("a FAILED push must not be retried automatically")
	}
}

// ── outbox & idempotency plumbing under the restricted role ──────────────────

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

func TestOutboxRelay_PublishesAcrossTenantsAndMarksPublished(t *testing.T) {
	f := setup(t)
	a := f.newReceipt(t, nil, 10, 1000)
	tenantB := uuid.NewString()
	fb := fixture{owner: f.owner, s: f.s, tenant: tenantB, ctx: f.as(tenantB)}
	b := fb.newReceipt(t, nil, 5, 500)

	pub := &recordingPublisher{}
	outbox.NewRelay(f.s.pool, pub, time.Second, 1000, zap.NewNop()).RelayOnce(context.Background())
	seen := map[string]bool{}
	for _, id := range pub.ids {
		seen[id] = true
	}
	if !seen[a.ReceiptID] || !seen[b.ReceiptID] {
		t.Fatalf("the relay must publish both tenants' events, saw %v", pub.ids)
	}
	if f.count(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) != 0 {
		t.Fatal("published rows must be marked published")
	}
}

func TestIdempotencyStore_ProceedInFlightReplayMismatch_AndTenantScoped(t *testing.T) {
	f := setup(t)
	st := idempotency.NewPgStore(f.s.pool)
	ctx := context.Background()
	out, _, err := st.Begin(ctx, f.tenant, "k1", "hash-1", "POST", "/ap04/receipts/x/confirm")
	if err != nil || out != idempotency.Proceed {
		t.Fatalf("first use must proceed, got %v %v", out, err)
	}
	if out, _, _ := st.Begin(ctx, f.tenant, "k1", "hash-1", "POST", "/p"); out != idempotency.InFlight {
		t.Fatalf("a concurrent identical request must be InFlight, got %v", out)
	}
	if err := st.Complete(ctx, f.tenant, "k1", 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	out, rec, _ := st.Begin(ctx, f.tenant, "k1", "hash-1", "POST", "/p")
	if out != idempotency.Replay || rec.StatusCode != 200 || string(rec.Body) != `{"ok":true}` {
		t.Fatalf("a completed identical request must replay its stored result, got %v %+v", out, rec)
	}
	if out, _, _ := st.Begin(ctx, f.tenant, "k1", "hash-2", "POST", "/p"); out != idempotency.Mismatch {
		t.Fatalf("the same key with a different request must be a Mismatch, got %v", out)
	}
	if out, _, _ := st.Begin(ctx, uuid.NewString(), "k1", "hash-1", "POST", "/p"); out != idempotency.Proceed {
		t.Fatalf("keys are scoped to the tenant: another tenant proceeds, got %v", out)
	}
}
