package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"zoiko.io/expense-claim-svc/internal/domain"
	"zoiko.io/expense-claim-svc/internal/employeemaster"
	"zoiko.io/expense-claim-svc/internal/middleware"
	"zoiko.io/expense-claim-svc/internal/outbox"
	"zoiko.io/expense-claim-svc/internal/payableopenitem"
	"zoiko.io/expense-claim-svc/internal/payablerelay"
	"zoiko.io/expense-claim-svc/internal/payeeidentity"
	"zoiko.io/expense-claim-svc/internal/store"
)

// These suites need a database. TEST_DATABASE_URL names the OWNER role (it applies
// the migrations from a clean slate and runs the verification queries). When
// TEST_APP_DATABASE_URL names an ordinary NOSUPERUSER NOBYPASSRLS role the STORE
// runs as that role instead — the role the service has once DB_USER names one — so
// the row-level-security policies, including the background relays' app.system_relay
// access, actually bind it. Both refuse to run unless the database name contains
// "test": they DROP this service's tables.

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
	for _, tbl := range []string{"payable_requests", "accounting_posting_requests", "expense_claim_idempotency", "expense_claim_submissions",
		"expense_claim_events", "expense_lines", "expense_claims", "outbox_events", "apr_tmp"} {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl+` CASCADE`)
	}
	for _, fn := range []string{"reject_expense_claim_mutation", "reject_expense_line_mutation", "reject_evidence_mutation",
		"reject_payable_request_mutation", "reject_accounting_posting_mutation"} {
		_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS `+fn+`() CASCADE`)
	}
	_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS expense_claim_transition_allowed(text, text) CASCADE`)
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
	return fx{owner: owner, s: store.NewPgStore(storePool, zap.NewNop()), tenant: tenant, le: uuid.NewString(),
		ctx: middleware.WithTenant(context.Background(), tenant)}
}

// other returns a second tenant sharing the same pools.
func (f fx) other() fx {
	t := uuid.NewString()
	return fx{owner: f.owner, s: f.s, tenant: t, le: uuid.NewString(), ctx: middleware.WithTenant(context.Background(), t)}
}

func (f fx) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func params(id, principal string) domain.CommandParams {
	return domain.CommandParams{ClaimID: id, PrincipalID: principal, CorrelationID: "corr-" + uuid.NewString()}
}

func (f fx) newClaim(t *testing.T) *domain.ExpenseClaim {
	t.Helper()
	c, err := f.s.CreateClaim(f.ctx, f.tenant, domain.CreateExpenseClaimRequest{
		LegalEntityID: f.le, ClaimantPrincipalID: "claimant", Currency: "USD", BusinessPurpose: "client dinner",
		ProjectCostCenter: "CC-1", PaymentPreferenceRef: "party-1",
	}, "claimant", "corr-create", nil)
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	return c
}

func (f fx) addLine(t *testing.T, id string, amount float64, receipt string) *domain.ExpenseLine {
	t.Helper()
	l, err := f.s.AddExpenseLine(f.ctx, id, domain.AddExpenseLineRequest{
		Merchant: "Acme", ExpenseDate: time.Now().UTC(), Amount: amount, Currency: "USD", Category: "MEALS", ReceiptDocumentID: receipt,
	}, "claimant", nil)
	if err != nil {
		t.Fatalf("AddExpenseLine: %v", err)
	}
	return l
}

// pending drives a claim to PENDING_APPROVAL through the real store commands.
func (f fx) pending(t *testing.T, amount float64) *domain.ExpenseClaim {
	t.Helper()
	c := f.newClaim(t)
	f.addLine(t, c.ClaimID, amount, "")
	if _, err := f.s.SubmitClaim(f.ctx, params(c.ClaimID, "claimant")); err != nil {
		t.Fatalf("SubmitClaim: %v", err)
	}
	got, err := f.s.RouteForApproval(f.ctx, domain.RoutingParams{CommandParams: params(c.ClaimID, "claimant"),
		PolicyResult: domain.PolicyWithinThreshold, PolicyVersionID: "pv-1"})
	if err != nil || got.Status != domain.StatusPendingApproval {
		t.Fatalf("RouteForApproval: %v %+v", err, got)
	}
	return got
}

func idem(key string) *domain.IdemKey {
	return &domain.IdemKey{Key: key, Operation: "ApproveExpenseClaim", RequestHash: "hash-" + key}
}

func (f fx) approve(t *testing.T, c *domain.ExpenseClaim, key string) (*domain.ExpenseClaim, error) {
	t.Helper()
	v := c.Version
	p := params(c.ClaimID, "approver")
	p.ExpectedVersion = &v
	if key != "" {
		p.Idem = idem(key)
	}
	return f.s.ApproveClaim(f.ctx, domain.ApproveParams{CommandParams: p, DueDate: time.Now().UTC().Add(14 * 24 * time.Hour).Truncate(24 * time.Hour),
		Posting: domain.PostingConfig{ExpenseKey: "EXP", PayableKey: "PAY", TaxRecoverableKey: "TAXREC"}})
}

// ── lifecycle & atomic financial consequence ─────────────────────────────────

func TestLifecycle_SubmitSnapshotApprove_WritesEverythingAtomically(t *testing.T) {
	f := setup(t)
	c := f.newClaim(t)
	if c.Status != domain.StatusDraft || c.Version != 1 || c.PolicyAssessmentResult != domain.PolicyNotAssessed {
		t.Fatalf("expected a fresh DRAFT v1, got %+v", c)
	}
	l, err := f.s.AddExpenseLine(f.ctx, c.ClaimID, domain.AddExpenseLineRequest{Merchant: "Acme", ExpenseDate: time.Now().UTC(), Amount: 80, Currency: "USD", Category: "MEALS", ClaimTaxRecovery: true}, "claimant", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.addLine(t, c.ClaimID, 20, "")
	// Tax recoverability only ever comes from a determination.
	if err := f.s.SetLineTaxDetermination(f.ctx, l.LineID, "det-1", 80, 8); err != nil {
		t.Fatal(err)
	}

	sub, err := f.s.SubmitClaim(f.ctx, params(c.ClaimID, "claimant"))
	if err != nil || sub.Status != domain.StatusSubmitted || sub.SubmittedVersion != 1 || sub.Version != 2 {
		t.Fatalf("expected SUBMITTED, submission v1, claim v2, got %v %+v", err, sub)
	}
	// The snapshot is immutable evidence whose hash can be recomputed from the row.
	subs, err := f.s.ListSubmissions(f.ctx, c.ClaimID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("expected one submission, got %v %d", err, len(subs))
	}
	canonical, hash, _ := domain.SnapshotHash(subs[0].Snapshot)
	sum := sha256.Sum256(canonical)
	if subs[0].SnapshotHash != hash || hash != hex.EncodeToString(sum[:]) {
		t.Fatal("the stored snapshot hash must be recomputable from the stored snapshot")
	}
	var snap domain.SubmissionSnapshot
	_ = json.Unmarshal(subs[0].Snapshot, &snap)
	if snap.TotalAmount != 100 || len(snap.Lines) != 2 || snap.VersionNo != 1 {
		t.Fatalf("unexpected snapshot %+v", snap)
	}

	routed, err := f.s.RouteForApproval(f.ctx, domain.RoutingParams{CommandParams: params(c.ClaimID, "claimant"), PolicyResult: domain.PolicyWithinThreshold, PolicyVersionID: "pv-1"})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := f.approve(t, routed, "approve-1")
	if err != nil {
		t.Fatalf("ApproveClaim: %v", err)
	}
	if approved.Status != domain.StatusApproved || approved.ApprovedByPrincipalID == nil || *approved.ApprovedByPrincipalID != "approver" ||
		approved.ApprovedAt == nil || approved.PayableState != domain.PayablePending {
		t.Fatalf("expected APPROVED by the approver with a PENDING payable, got %+v", approved)
	}

	// The durable AP-08 request, written in the approval transaction.
	var amount float64
	var state, ccy string
	if err := f.owner.QueryRow(context.Background(), `SELECT amount::float8, state, currency FROM payable_requests WHERE claim_id = $1`, c.ClaimID).Scan(&amount, &state, &ccy); err != nil {
		t.Fatal(err)
	}
	if amount != 100 || state != "PENDING" || ccy != "USD" {
		t.Fatalf("expected a PENDING 100 USD payable request, got %v %s %s", amount, state, ccy)
	}

	// The ACC-04 request: balanced, mapping keys only, recoverable tax only from the determination.
	var payload []byte
	if err := f.owner.QueryRow(context.Background(), `SELECT request_payload FROM accounting_posting_requests WHERE aggregate_id = $1`, c.ClaimID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var p domain.GLPostingRequest
	_ = json.Unmarshal(payload, &p)
	var dr, cr float64
	keys := map[string]float64{}
	for _, ln := range p.Lines {
		dr += ln.DebitAmount
		cr += ln.CreditAmount
		keys[ln.MappingKey] += ln.DebitAmount + ln.CreditAmount
	}
	if dr != 100 || cr != 100 || keys["EXP"] != 92 || keys["TAXREC"] != 8 || keys["PAY"] != 100 || p.SourceEventID != domain.ApprovalSourceEventID(c.ClaimID) {
		t.Fatalf("expected a balanced 100 posting (92 expense + 8 recoverable tax) keyed by the claim, got %+v", p)
	}

	// History and outbox: spec names plus aliases, in the same transaction.
	for _, typ := range []string{"ExpenseClaimCreated", "ExpenseClaimSubmitted", "ExpenseClaimPendingApproval", "ExpenseClaimApproved",
		"ExpenseClaimPayableRequested", domain.EventAccountingRequested, domain.EventClaimApproved} {
		if f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, c.ClaimID, typ) != 1 {
			t.Fatalf("expected exactly one %s in the outbox", typ)
		}
	}
	hist, err := f.s.ListClaimEvents(f.ctx, c.ClaimID)
	if err != nil || len(hist) < 5 {
		t.Fatalf("expected the audit trail, got %v %d", err, len(hist))
	}
}

// Replay safety: a second approval is refused by the state machine, and nothing
// the first approval wrote is written again.
func TestApprove_Replay_CannotDuplicateThePayableOrThePosting(t *testing.T) {
	f := setup(t)
	c := f.pending(t, 20)
	if _, err := f.approve(t, c, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.approve(t, c, "k2"); err == nil {
		t.Fatal("a second approval of the same claim must be refused")
	}
	for table, want := range map[string]int{"payable_requests": 1, "accounting_posting_requests": 1} {
		if n := f.count(t, `SELECT count(*) FROM `+table+` WHERE `+map[string]string{"payable_requests": "claim_id", "accounting_posting_requests": "aggregate_id"}[table]+` = $1`, c.ClaimID); n != want {
			t.Fatalf("%s: expected %d row after a replay, got %d", table, want, n)
		}
	}
	if f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'ExpenseClaimApproved'`, c.ClaimID) != 1 {
		t.Fatal("a replayed approval must not emit the event again")
	}

	// A stored idempotency result also blocks the same key being used twice.
	c2 := f.pending(t, 30)
	if _, err := f.approve(t, c2, "k1"); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("a key already used by another command must conflict, got %v", err)
	}
	if got, _ := f.s.FindClaim(f.ctx, c2.ClaimID); got.Status != domain.StatusPendingApproval {
		t.Fatal("a conflicting key must roll the whole approval back")
	}
	rec, err := f.s.GetIdempotency(f.ctx, "k1")
	if err != nil || rec == nil || rec.Operation != "ApproveExpenseClaim" || rec.RequestHash != "hash-k1" {
		t.Fatalf("expected the stored idempotency record, got %v %+v", err, rec)
	}
	if rec, _ := f.other().s.GetIdempotency(f.other().ctx, "k1"); rec != nil {
		t.Fatal("idempotency keys are scoped to the tenant")
	}
}

// The approval, the payable request, the posting request and the events commit
// together or not at all.
func TestApprove_IsAtomic_AFailureLateInTheTransactionRollsEverythingBack(t *testing.T) {
	f := setup(t)
	c := f.pending(t, 20)
	ctx := context.Background()
	if _, err := f.owner.Exec(ctx, `ALTER TABLE accounting_posting_requests RENAME TO apr_tmp`); err != nil {
		t.Fatal(err)
	}
	_, err := f.approve(t, c, "k")
	if _, rerr := f.owner.Exec(ctx, `ALTER TABLE apr_tmp RENAME TO accounting_posting_requests`); rerr != nil {
		t.Fatal(rerr)
	}
	if err == nil {
		t.Fatal("the approval must fail when its posting request cannot be written")
	}
	got, _ := f.s.FindClaim(f.ctx, c.ClaimID)
	if got.Status != domain.StatusPendingApproval || got.ApprovedByPrincipalID != nil || got.Version != c.Version {
		t.Fatalf("the claim must be exactly as before, got %+v", got)
	}
	if f.count(t, `SELECT count(*) FROM payable_requests WHERE claim_id = $1`, c.ClaimID) != 0 ||
		f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type LIKE '%Approved%'`, c.ClaimID) != 0 {
		t.Fatal("no payable request or approval event may survive the rollback")
	}
	if _, err := f.approve(t, c, "k-retry"); err != nil {
		t.Fatalf("once healthy, the same approval must succeed, got %v", err)
	}
}

func TestApprove_Concurrent_OnlyOneWins_OneFinancialConsequence(t *testing.T) {
	f := setup(t)
	c := f.pending(t, 20)
	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = f.approve(t, c, "key-"+uuid.NewString())
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one concurrent approval may succeed, %d did: %v", wins, results)
	}
	if f.count(t, `SELECT count(*) FROM payable_requests WHERE claim_id = $1`, c.ClaimID) != 1 ||
		f.count(t, `SELECT count(*) FROM accounting_posting_requests WHERE aggregate_id = $1`, c.ClaimID) != 1 {
		t.Fatal("concurrent approvals must produce exactly one payable and one posting request")
	}
}

func TestCommands_StaleVersion_ReasonsAndTransitions(t *testing.T) {
	f := setup(t)
	c := f.pending(t, 20)
	stale := 1
	p := params(c.ClaimID, "approver")
	p.ExpectedVersion = &stale
	if _, err := f.s.RejectClaim(f.ctx, p); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}
	p.ExpectedVersion, p.Reason = nil, "not a business expense"
	rej, err := f.s.RejectClaim(f.ctx, p)
	if err != nil || rej.Status != domain.StatusRejected || rej.RejectionReason != "not a business expense" {
		t.Fatalf("expected REJECTED with the reason, got %v %+v", err, rej)
	}
	if _, err := f.s.CancelClaim(f.ctx, params(c.ClaimID, "claimant")); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a rejected claim is terminal, got %v", err)
	}
	if f.count(t, `SELECT count(*) FROM payable_requests`) != 0 || f.count(t, `SELECT count(*) FROM accounting_posting_requests`) != 0 {
		t.Fatal("a rejection has no financial consequence")
	}
}

func TestReturn_PreservesTheSubmissionAndResubmitsAsTheNextVersion(t *testing.T) {
	f := setup(t)
	c := f.pending(t, 20)
	first, _ := f.s.ListSubmissions(f.ctx, c.ClaimID)
	p := params(c.ClaimID, "approver")
	p.Reason = "wrong cost center"
	ret, err := f.s.ReturnClaim(f.ctx, p)
	if err != nil || ret.Status != domain.StatusReturned || ret.ReturnReason != "wrong cost center" {
		t.Fatalf("expected RETURNED, got %v %+v", err, ret)
	}
	lines, _ := f.s.ListLines(f.ctx, c.ClaimID)
	if _, err := f.s.VoidExpenseLine(f.ctx, c.ClaimID, lines[0].LineID, "wrong", "claimant", "c", nil); err != nil {
		t.Fatal(err)
	}
	f.addLine(t, c.ClaimID, 22, "")
	if _, err := f.s.SubmitClaim(f.ctx, params(c.ClaimID, "claimant")); err != nil {
		t.Fatal(err)
	}
	subs, _ := f.s.ListSubmissions(f.ctx, c.ClaimID)
	if len(subs) != 2 || subs[0].SnapshotHash != first[0].SnapshotHash || subs[1].VersionNo != 2 {
		t.Fatalf("version 1 must be untouched and version 2 added, got %+v", subs)
	}
	all, _ := f.s.ListLines(f.ctx, c.ClaimID)
	if len(all) != 2 || all[0].VoidedAt == nil {
		t.Fatal("a voided line is kept as evidence, never deleted")
	}
	if len(domain.ActiveLines(all)) != 1 {
		t.Fatal("only the corrected line counts")
	}
}

func TestSubmit_NeedsLines_AndResumeWritesNoSecondSnapshot(t *testing.T) {
	f := setup(t)
	empty := f.newClaim(t)
	if _, err := f.s.SubmitClaim(f.ctx, params(empty.ClaimID, "claimant")); !errors.Is(err, domain.ErrNoLines) {
		t.Fatalf("expected ErrNoLines, got %v", err)
	}
	c := f.newClaim(t)
	f.addLine(t, c.ClaimID, 10, "")
	f.s.SubmitClaim(f.ctx, params(c.ClaimID, "claimant")) //nolint:errcheck
	again, err := f.s.SubmitClaim(f.ctx, params(c.ClaimID, "claimant"))
	if err != nil || again.Status != domain.StatusSubmitted {
		t.Fatalf("resuming a SUBMITTED claim is a no-op, got %v", err)
	}
	if subs, _ := f.s.ListSubmissions(f.ctx, c.ClaimID); len(subs) != 1 {
		t.Fatalf("expected a single snapshot, got %d", len(subs))
	}
}

// ── invariants enforced by the database ──────────────────────────────────────

// Negative path 2 at the database: the same receipt cannot live on two claims.
func TestReceipt_UniqueAcrossClaims_UntilVoided_AndMalformedIdIsNotAnOutage(t *testing.T) {
	f := setup(t)
	doc := uuid.NewString()
	c1, c2 := f.newClaim(t), f.newClaim(t)
	l1 := f.addLine(t, c1.ClaimID, 30, doc)
	_, err := f.s.AddExpenseLine(f.ctx, c2.ClaimID, domain.AddExpenseLineRequest{Merchant: "x", ExpenseDate: time.Now(), Amount: 5, Currency: "USD", ReceiptDocumentID: doc}, "p", nil)
	if !errors.Is(err, domain.ErrDuplicateReceipt) {
		t.Fatalf("expected ErrDuplicateReceipt, got %v", err)
	}
	if in, claim, line, _ := f.s.IsReceiptInUse(f.ctx, doc); !in || claim != c1.ClaimID || line != l1.LineID {
		t.Fatalf("the duplicate assessment must name the holder, got %v %s %s", in, claim, line)
	}
	if in, _, _, _ := f.other().s.IsReceiptInUse(f.other().ctx, doc); in {
		t.Fatal("another tenant must not learn which claim holds a receipt")
	}
	if _, err := f.s.VoidExpenseLine(f.ctx, c1.ClaimID, l1.LineID, "wrong claim", "p", "c", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AddExpenseLine(f.ctx, c2.ClaimID, domain.AddExpenseLineRequest{Merchant: "x", ExpenseDate: time.Now(), Amount: 5, Currency: "USD", ReceiptDocumentID: doc}, "p", nil); err != nil {
		t.Fatalf("a voided line frees its receipt, got %v", err)
	}
	if _, err := f.s.AddExpenseLine(f.ctx, c2.ClaimID, domain.AddExpenseLineRequest{Merchant: "x", ExpenseDate: time.Now(), Amount: 5, Currency: "USD", ReceiptDocumentID: "not-a-uuid"}, "p", nil); !errors.Is(err, domain.ErrDocumentNotFound) {
		t.Fatalf("a malformed receipt id is the caller's mistake, got %v", err)
	}
}

func TestLines_CurrencyMustMatch_AndClaimsMustBeEditable(t *testing.T) {
	f := setup(t)
	c := f.newClaim(t)
	if _, err := f.s.AddExpenseLine(f.ctx, c.ClaimID, domain.AddExpenseLineRequest{Merchant: "x", ExpenseDate: time.Now(), Amount: 5, Currency: "EUR"}, "p", nil); !errors.Is(err, domain.ErrCurrencyMismatch) {
		t.Fatalf("expected ErrCurrencyMismatch, got %v", err)
	}
	p := f.pending(t, 10)
	if _, err := f.s.AddExpenseLine(f.ctx, p.ClaimID, domain.AddExpenseLineRequest{Merchant: "x", ExpenseDate: time.Now(), Amount: 5, Currency: "USD"}, "p", nil); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a claim under review accepts no lines, got %v", err)
	}
	if _, err := f.s.AddExpenseLine(f.other().ctx, c.ClaimID, domain.AddExpenseLineRequest{Merchant: "x", ExpenseDate: time.Now(), Amount: 5, Currency: "USD"}, "p", nil); !errors.Is(err, domain.ErrClaimNotFound) {
		t.Fatalf("another tenant cannot add to the claim, got %v", err)
	}
}

// The state-machine table in Go and the trigger in Postgres must agree for every
// pair of statuses (the migration says "a store test asserts parity").
func TestStateMachine_TriggerMatchesTheDomainTransitionTable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, from := range domain.AllStatuses {
		for _, to := range domain.AllStatuses {
			id := uuid.NewString()
			if _, err := f.owner.Exec(ctx, `INSERT INTO expense_claims (claim_id, tenant_id, legal_entity_id, claimant_principal_id, currency, status)
				VALUES ($1, $2, $3, 'c', 'USD', $4)`, id, f.tenant, f.le, string(from)); err != nil {
				t.Fatalf("insert %s: %v", from, err)
			}
			_, err := f.owner.Exec(ctx, `UPDATE expense_claims SET status = $2, version = version + 1 WHERE claim_id = $1`, id, string(to))
			want := (from != to && domain.CanTransition(from, to)) || (from == to && !domain.IsTerminal(from))
			if (err == nil) != want {
				t.Fatalf("%s -> %s: domain says allowed=%v, database err=%v", from, to, want, err)
			}
		}
	}
}

func TestImmutability_Claim_Lines_Submissions_Idempotency(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	c := f.newClaim(t)
	l := f.addLine(t, c.ClaimID, 10, "")
	f.s.SubmitClaim(f.ctx, params(c.ClaimID, "claimant")) //nolint:errcheck
	key := idem("immut")
	p := f.pending(t, 10)
	if _, err := f.approve(t, p, "immut"); err != nil {
		t.Fatal(err)
	}
	_ = key

	refused := map[string]string{
		"delete a claim":              `DELETE FROM expense_claims WHERE claim_id = '` + c.ClaimID + `'`,
		"change the claimant":         `UPDATE expense_claims SET claimant_principal_id = 'someone' WHERE claim_id = '` + p.ClaimID + `'`,
		"change the currency":         `UPDATE expense_claims SET currency = 'EUR' WHERE claim_id = '` + c.ClaimID + `'`,
		"rewrite the approver":        `UPDATE expense_claims SET approved_by_principal_id = 'someone' WHERE claim_id = '` + p.ClaimID + `'`,
		"move versions backwards":     `UPDATE expense_claims SET version = 0 WHERE claim_id = '` + c.ClaimID + `'`,
		"delete a line":               `DELETE FROM expense_lines WHERE line_id = '` + l.LineID + `'`,
		"edit a submitted line":       `UPDATE expense_lines SET amount = 1 WHERE line_id = '` + l.LineID + `'`,
		"edit a submission":           `UPDATE expense_claim_submissions SET snapshot = '{}'::jsonb`,
		"delete a submission":         `DELETE FROM expense_claim_submissions`,
		"edit an idempotency record":  `UPDATE expense_claim_idempotency SET response = '{}'::jsonb`,
		"delete a history row":        `DELETE FROM expense_claim_events`,
		"delete a payable request":    `DELETE FROM payable_requests`,
		"rewrite the payable amount":  `UPDATE payable_requests SET amount = 1`,
		"delete a posting request":    `DELETE FROM accounting_posting_requests`,
		"rewrite the posting payload": `UPDATE accounting_posting_requests SET request_payload = '{}'::jsonb`,
	}
	for name, sql := range refused {
		if _, err := f.owner.Exec(ctx, sql); err == nil {
			t.Fatalf("%s: the database must refuse it", name)
		}
	}
	if got, _ := f.s.FindClaim(f.ctx, c.ClaimID); got.Currency != "USD" || got.Status != domain.StatusSubmitted {
		t.Fatalf("the claim must be untouched, got %+v", got)
	}
}

// Migration 000007: a refused or exhausted posting can be requeued — and only
// requeued — while POSTED stays final and the request itself stays immutable.
func TestPostingTrigger_RequeueOnly_PostedIsFinal(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	c := f.pending(t, 20)
	if _, err := f.approve(t, c, "k"); err != nil {
		t.Fatal(err)
	}
	set := func(sql string) error {
		_, err := f.owner.Exec(ctx, sql+` WHERE aggregate_id = '`+c.ClaimID+`'`)
		return err
	}

	if err := set(`UPDATE accounting_posting_requests SET status = 'FAILED'`); err != nil {
		t.Fatalf("PENDING -> FAILED must be allowed: %v", err)
	}
	if err := set(`UPDATE accounting_posting_requests SET status = 'POSTED'`); err == nil {
		t.Fatal("FAILED -> POSTED must be refused: a failed request goes back through the ledger")
	}
	if err := set(`UPDATE accounting_posting_requests SET request_payload = '{}'::jsonb`); err == nil {
		t.Fatal("the request basis stays immutable even on a requeue")
	}
	if err := set(`UPDATE accounting_posting_requests SET status = 'PENDING'`); err != nil {
		t.Fatalf("FAILED -> PENDING (the requeue) must be allowed: %v", err)
	}
	if err := set(`UPDATE accounting_posting_requests SET status = 'QUARANTINED'`); err != nil {
		t.Fatal(err)
	}
	if err := set(`UPDATE accounting_posting_requests SET status = 'FAILED'`); err == nil {
		t.Fatal("QUARANTINED may only move to PENDING")
	}
	set(`UPDATE accounting_posting_requests SET status = 'PENDING'`) //nolint:errcheck
	if err := set(`UPDATE accounting_posting_requests SET status = 'POSTED'`); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"PENDING", "FAILED", "QUARANTINED"} {
		if err := set(`UPDATE accounting_posting_requests SET status = '` + to + `'`); err == nil {
			t.Fatalf("POSTED -> %s must be refused: a ledger posting is never repeated", to)
		}
	}
}

// ── tenant isolation ─────────────────────────────────────────────────────────

func TestTenantIsolation_EveryReadAndCommand(t *testing.T) {
	f := setup(t)
	c := f.pending(t, 20)
	o := f.other()
	if _, err := o.s.FindClaim(o.ctx, c.ClaimID); !errors.Is(err, domain.ErrClaimNotFound) {
		t.Fatalf("another tenant must not see the claim, got %v", err)
	}
	if lines, _ := o.s.ListLines(o.ctx, c.ClaimID); len(lines) != 0 {
		t.Fatal("another tenant must see no lines")
	}
	if evs, _ := o.s.ListClaimEvents(o.ctx, c.ClaimID); len(evs) != 0 {
		t.Fatal("another tenant must see no history")
	}
	if subs, _ := o.s.ListSubmissions(o.ctx, c.ClaimID); len(subs) != 0 {
		t.Fatal("another tenant must see no submissions")
	}
	if ps, _ := o.s.ListPostingRequests(o.ctx, c.ClaimID); len(ps) != 0 {
		t.Fatal("another tenant must see no posting requests")
	}
	if _, err := o.s.CancelClaim(o.ctx, params(c.ClaimID, "attacker")); !errors.Is(err, domain.ErrClaimNotFound) {
		t.Fatalf("another tenant cannot cancel, got %v", err)
	}
	if n, _ := o.s.RequeuePostings(o.ctx, c.ClaimID); n != 0 {
		t.Fatal("another tenant cannot requeue")
	}
	if req, _ := o.s.FindPayableRequest(o.ctx, c.ClaimID); req != nil {
		t.Fatal("another tenant cannot see the payable request")
	}
	if got, err := f.s.FindClaim(f.ctx, c.ClaimID); err != nil || got.Status != domain.StatusPendingApproval {
		t.Fatalf("the owner still sees an unchanged claim, got %v %+v", err, got)
	}
}

// ── ACC-04 posting dispatch ──────────────────────────────────────────────────

func TestDispatchPostings_CrossTenant_PostsOnce_Quarantines_BacksOff_CapsAndRequeues(t *testing.T) {
	f := setup(t)
	fb := f.other()
	a, b, c := f.pending(t, 10), fb.pending(t, 20), f.pending(t, 30)
	for _, x := range []struct {
		f fx
		c *domain.ExpenseClaim
	}{{f, a}, {fb, b}, {f, c}} {
		if _, err := x.f.approve(t, x.c, "k-"+x.c.ClaimID); err != nil {
			t.Fatal(err)
		}
	}
	srcA, srcB := domain.ApprovalSourceEventID(a.ClaimID), domain.ApprovalSourceEventID(b.ClaimID)

	var mu sync.Mutex
	calls := map[string]int{}
	post := func(p domain.PostingRequest) domain.PostingOutcome {
		mu.Lock()
		defer mu.Unlock()
		calls[p.SourceEventID]++
		switch p.SourceEventID {
		case srcA:
			return domain.PostingOutcome{Status: domain.PostingPosted, ExecutionID: "exec-A", JournalID: "journal-A"}
		case srcB:
			return domain.PostingOutcome{Status: domain.PostingQuarantined, Error: "no ACC-02 mapping"}
		default:
			return domain.PostingOutcome{Status: domain.PostingPending, Error: "ledger answered 503", NextAttempt: time.Now().Add(time.Hour)}
		}
	}
	if n, err := f.s.DispatchPostings(context.Background(), 100, post); err != nil || n != 3 {
		t.Fatalf("expected 3 outcomes recorded across both tenants, got %d %v", n, err)
	}
	pa, _ := f.s.ListPostingRequests(f.ctx, a.ClaimID)
	pb, _ := fb.s.ListPostingRequests(fb.ctx, b.ClaimID)
	pc, _ := f.s.ListPostingRequests(f.ctx, c.ClaimID)
	if pa[0].Status != domain.PostingPosted || pa[0].PostingExecutionID != "exec-A" || pa[0].JournalID != "journal-A" || pa[0].PostedAt == nil {
		t.Fatalf("tenant A's request should be POSTED with the ledger's ids, got %+v", pa[0])
	}
	if pb[0].Status != domain.PostingQuarantined || !strings.Contains(pb[0].LastError, "no ACC-02 mapping") {
		t.Fatalf("a refusal should be QUARANTINED with the reason, got %+v", pb[0])
	}
	if pc[0].Status != domain.PostingPending || pc[0].Attempts != 1 {
		t.Fatalf("a transient failure counts an attempt and stays PENDING, got %+v", pc[0])
	}

	// Posted and quarantined are not picked again; the backed-off one is not yet due.
	if n, _ := f.s.DispatchPostings(context.Background(), 100, post); n != 0 {
		t.Fatalf("nothing is due, got %d", n)
	}
	if calls[srcA] != 1 || calls[srcB] != 1 {
		t.Fatalf("each request is submitted once until due again, got %v", calls)
	}

	// Requeue: tenant-scoped, never a POSTED one, and the ledger then takes it.
	if n, _ := f.s.RequeuePostings(f.ctx, a.ClaimID); n != 0 {
		t.Fatal("a POSTED request must never be requeued")
	}
	if n, _ := f.s.RequeuePostings(f.ctx, b.ClaimID); n != 0 {
		t.Fatal("tenant A cannot requeue tenant B's request")
	}
	if n, err := fb.s.RequeuePostings(fb.ctx, b.ClaimID); err != nil || n != 1 {
		t.Fatalf("expected 1 requeued, got %d %v", n, err)
	}
	if _, err := f.s.DispatchPostings(context.Background(), 100, func(p domain.PostingRequest) domain.PostingOutcome {
		if p.SourceEventID == srcB {
			return domain.PostingOutcome{Status: domain.PostingPosted, ExecutionID: "exec-B"}
		}
		return domain.PostingOutcome{Status: domain.PostingPending, Error: "not mine", NextAttempt: time.Now().Add(time.Hour)}
	}); err != nil {
		t.Fatal(err)
	}
	if pb, _ = fb.s.ListPostingRequests(fb.ctx, b.ClaimID); pb[0].Status != domain.PostingPosted {
		t.Fatalf("the requeued request should now be POSTED, got %+v", pb[0])
	}

	// A request that keeps failing transiently is capped: visible FAILED, not retried forever.
	for i := 0; i < store.MaxPostingAttempts; i++ {
		f.owner.Exec(context.Background(), `UPDATE accounting_posting_requests SET next_attempt_at = NOW() WHERE status = 'PENDING'`) //nolint:errcheck
		if _, err := f.s.DispatchPostings(context.Background(), 100, func(p domain.PostingRequest) domain.PostingOutcome {
			return domain.PostingOutcome{Status: domain.PostingPending, Error: "ledger unreachable", NextAttempt: time.Now().Add(time.Minute)}
		}); err != nil {
			t.Fatal(err)
		}
	}
	if pc, _ = f.s.ListPostingRequests(f.ctx, c.ClaimID); pc[0].Status != domain.PostingFailed {
		t.Fatalf("expected FAILED after the retry cap, got %+v", pc[0])
	}
	if got, _ := f.s.FindClaim(f.ctx, c.ClaimID); got.Status != domain.StatusApproved {
		t.Fatal("an accounting failure must never undo the approval")
	}
}

// ── payable relay (real relay, real store, fake peers) ───────────────────────

type fakeEmployee struct{ err error }

func (e *fakeEmployee) VerifyActiveClaimant(context.Context, string, string, string) error {
	return e.err
}

var _ employeemaster.Client = (*fakeEmployee)(nil)

type fakePayee struct {
	dest     *payeeidentity.Destination
	err      error
	partyRef []string
}

func (p *fakePayee) GetActiveDestination(_ context.Context, _, _, _, partyRef string) (*payeeidentity.Destination, error) {
	p.partyRef = append(p.partyRef, partyRef)
	return p.dest, p.err
}

var _ payeeidentity.Client = (*fakePayee)(nil)

type fakePayable struct {
	created []payableopenitem.CreatePayableRequest
	err     error
	status  string
}

func (p *fakePayable) CreatePayableFromApprovedSource(_ context.Context, _, _ string, req payableopenitem.CreatePayableRequest) (*payableopenitem.PayableOpenItem, error) {
	if p.err != nil {
		return nil, p.err
	}
	p.created = append(p.created, req)
	return &payableopenitem.PayableOpenItem{PayableID: "payable-" + req.SourceReference, Status: "OPEN"}, nil
}

func (p *fakePayable) GetPayable(_ context.Context, _, _, id string) (*payableopenitem.PayableOpenItem, error) {
	return &payableopenitem.PayableOpenItem{PayableID: id, Status: p.status}, nil
}

var _ payableopenitem.Client = (*fakePayable)(nil)

func (f fx) approved(t *testing.T, amount float64) *domain.ExpenseClaim {
	t.Helper()
	c := f.pending(t, amount)
	a, err := f.approve(t, c, "k-"+c.ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f fx) relay(emp *fakeEmployee, payee *fakePayee, pay *fakePayable) *payablerelay.Relay {
	return payablerelay.New(f.s, emp, payee, pay, zap.NewNop())
}

func TestRelay_CreatesTheAP08Payable_ThenTheClaimIsReimbursable(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 42.5)
	payee := &fakePayee{dest: &payeeidentity.Destination{DestinationID: "dest-1", LegalEntityID: f.le, Status: "ACTIVE"}}
	pay := &fakePayable{}
	f.relay(&fakeEmployee{}, payee, pay).ProcessClaim(f.ctx, c.ClaimID)

	if len(pay.created) != 1 || pay.created[0].SourceReference != c.ClaimID || pay.created[0].OriginalAmount != 42.5 ||
		pay.created[0].SourceType != payableopenitem.SourceExpenseClaim || pay.created[0].PayeeRef != "party-1" || pay.created[0].Currency != "USD" {
		t.Fatalf("expected one AP-08 payable keyed by the claim (idempotent source_reference), got %+v", pay.created)
	}
	got, _ := f.s.FindClaim(f.ctx, c.ClaimID)
	if got.Status != domain.StatusReimbursable || got.PayableState != domain.PayableCreated || got.PayableID != "payable-"+c.ClaimID || got.PayeeDestinationID != "dest-1" {
		t.Fatalf("expected REIMBURSABLE with the payable and the controlled destination recorded, got %+v", got)
	}
	// A repeat hand-off is a no-op; the completed request is frozen.
	f.relay(&fakeEmployee{}, payee, pay).ProcessClaim(f.ctx, c.ClaimID)
	if len(pay.created) != 1 {
		t.Fatalf("a completed request is never re-sent, got %d calls", len(pay.created))
	}
	if _, err := f.owner.Exec(context.Background(), `UPDATE payable_requests SET amount = 1 WHERE claim_id = $1`, c.ClaimID); err == nil {
		t.Fatal("a completed payable request cannot be modified")
	}
}

// A claim with no controlled payee is NOT payable: it blocks with a stable reason,
// never falls back to the claimant's own identity, and heals once a payee exists.
func TestRelay_NoControlledPayee_Blocks_ThenHeals(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 20)
	payee := &fakePayee{err: domain.ErrNoControlledPayee}
	pay := &fakePayable{}
	r := f.relay(&fakeEmployee{}, payee, pay)

	r.ProcessClaim(f.ctx, c.ClaimID)
	got, _ := f.s.FindClaim(f.ctx, c.ClaimID)
	if got.PayableState != domain.PayableBlocked || got.PayableBlockedReason != domain.BlockedNoControlledPayee || got.Status != domain.StatusApproved || len(pay.created) != 0 {
		t.Fatalf("expected BLOCKED NO_CONTROLLED_PAYEE with nothing created, got %+v", got)
	}
	r.ProcessClaim(f.ctx, c.ClaimID)
	if f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'ExpenseClaimPayableBlocked'`, c.ClaimID) != 1 {
		t.Fatal("the blocked event is written once per reason, not on every re-check")
	}

	payee.err, payee.dest = nil, &payeeidentity.Destination{DestinationID: "dest-9", LegalEntityID: f.le, Status: "ACTIVE"}
	f.owner.Exec(context.Background(), `UPDATE payable_requests SET next_attempt_at = NOW()`) //nolint:errcheck
	r.RunOnce(f.ctx)
	if got, _ = f.s.FindClaim(f.ctx, c.ClaimID); got.Status != domain.StatusReimbursable || got.PayableState != domain.PayableCreated || got.PayableBlockedReason != "" {
		t.Fatalf("the claim must heal once a payee is onboarded, got %+v", got)
	}
}

func TestRelay_ClaimantNotActive_Blocks_AndFallbackPartyRef(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 20)
	f.relay(&fakeEmployee{err: domain.ErrClaimantNotEligible}, &fakePayee{}, &fakePayable{}).ProcessClaim(f.ctx, c.ClaimID)
	if got, _ := f.s.FindClaim(f.ctx, c.ClaimID); got.PayableBlockedReason != domain.BlockedClaimantNotActive || got.PayableState != domain.PayableBlocked {
		t.Fatalf("claimant status ambiguity blocks payment, got %+v", got)
	}

	// With no payment_preference_ref the lookup key is the claimant's own reference;
	// ORG-10 still has to hold an ACTIVE destination for it.
	f2 := setup(t)
	cl, _ := f2.s.CreateClaim(f2.ctx, f2.tenant, domain.CreateExpenseClaimRequest{LegalEntityID: f2.le, ClaimantPrincipalID: "claimant-x", Currency: "USD", BusinessPurpose: "p"}, "claimant-x", "c", nil)
	f2.addLine(t, cl.ClaimID, 10, "")
	f2.s.SubmitClaim(f2.ctx, params(cl.ClaimID, "claimant-x")) //nolint:errcheck
	routed, _ := f2.s.RouteForApproval(f2.ctx, domain.RoutingParams{CommandParams: params(cl.ClaimID, "x"), PolicyResult: domain.PolicyWithinThreshold})
	if _, err := f2.approve(t, routed, "kk"); err != nil {
		t.Fatal(err)
	}
	payee := &fakePayee{err: domain.ErrNoControlledPayee}
	f2.relay(&fakeEmployee{}, payee, &fakePayable{}).ProcessClaim(f2.ctx, cl.ClaimID)
	if len(payee.partyRef) != 1 || payee.partyRef[0] != "claimant-x" {
		t.Fatalf("expected the claimant's own reference as the lookup key, got %v", payee.partyRef)
	}
}

func TestRelay_TransientFailures_BackOff_AndAreVisibleOnce(t *testing.T) {
	f := setup(t)
	c := f.approved(t, 20)
	r := f.relay(&fakeEmployee{}, &fakePayee{dest: &payeeidentity.Destination{DestinationID: "d", LegalEntityID: f.le, Status: "ACTIVE"}}, &fakePayable{err: domain.ErrPayableServiceUnavailable})
	r.ProcessClaim(f.ctx, c.ClaimID)
	r.ProcessClaim(f.ctx, c.ClaimID)
	var attempts int
	f.owner.QueryRow(context.Background(), `SELECT attempts FROM payable_requests WHERE claim_id = $1`, c.ClaimID).Scan(&attempts) //nolint:errcheck
	if attempts != 2 {
		t.Fatalf("each failed attempt is counted, got %d", attempts)
	}
	if f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'ExpenseClaimPayableCreateFailed'`, c.ClaimID) != 1 {
		t.Fatal("the visibility event is emitted on the first failure only")
	}
	if got, _ := f.s.FindClaim(f.ctx, c.ClaimID); got.Status != domain.StatusApproved {
		t.Fatal("an AP-08 outage never undoes the approval")
	}
	if due, _ := f.s.ListDuePayableRequests(context.Background(), 10); len(due) != 0 {
		t.Fatalf("a backed-off request is not yet due, got %d", len(due))
	}
}

// The relay runs with no request tenant: under a NOBYPASSRLS role it must still
// see every tenant's due work (app.system_relay), and each hand-off is then
// applied inside the owning tenant's scope.
func TestRelay_RunOnce_AcrossTenants_AndSettlementClosesTheClaim(t *testing.T) {
	f := setup(t)
	fb := f.other()
	a, b := f.approved(t, 20), fb.approved(t, 30)
	dest := func(le string) *payeeidentity.Destination {
		return &payeeidentity.Destination{DestinationID: "d", LegalEntityID: le, Status: "ACTIVE"}
	}
	pay := &fakePayable{}
	// One relay serves both tenants: its payee lookup is keyed by entity.
	relay := payablerelay.New(f.s, &fakeEmployee{}, entityPayee{f.le: dest(f.le), fb.le: dest(fb.le)}, pay, zap.NewNop())
	relay.RunOnce(context.Background())
	if len(pay.created) != 2 {
		t.Fatalf("both tenants' due requests must be processed, got %d", len(pay.created))
	}
	ga, _ := f.s.FindClaim(f.ctx, a.ClaimID)
	gb, _ := fb.s.FindClaim(fb.ctx, b.ClaimID)
	if ga.Status != domain.StatusReimbursable || gb.Status != domain.StatusReimbursable {
		t.Fatalf("both claims must be REIMBURSABLE, got %s / %s", ga.Status, gb.Status)
	}

	pay.status = "OPEN"
	relay.ReconcileSettlements(context.Background())
	if got, _ := f.s.FindClaim(f.ctx, a.ClaimID); got.Status != domain.StatusReimbursable {
		t.Fatal("an unsettled payable does not close the claim")
	}
	pay.status = payableopenitem.StatusSettled
	relay.ReconcileSettlements(context.Background())
	for _, x := range []struct {
		f fx
		c *domain.ExpenseClaim
	}{{f, a}, {fb, b}} {
		got, _ := x.f.s.FindClaim(x.f.ctx, x.c.ClaimID)
		if got.Status != domain.StatusClosed || got.ClosedAt == nil {
			t.Fatalf("a SETTLED payable must close the claim, got %+v", got)
		}
	}
}

type entityPayee map[string]*payeeidentity.Destination

func (e entityPayee) GetActiveDestination(_ context.Context, _, _, le, _ string) (*payeeidentity.Destination, error) {
	if d, ok := e[le]; ok {
		return d, nil
	}
	return nil, domain.ErrNoControlledPayee
}

// ── outbox plumbing under the restricted role ────────────────────────────────

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
	fb := f.other()
	a, b := f.newClaim(t), fb.newClaim(t)
	pub := &recordingPublisher{}
	outbox.NewRelay(f.s.Pool(), pub, time.Second, 1000, zap.NewNop()).RelayOnce(context.Background())
	seen := map[string]bool{}
	for _, id := range pub.ids {
		seen[id] = true
	}
	if !seen[a.ClaimID] || !seen[b.ClaimID] {
		t.Fatalf("the relay must publish both tenants' events, saw %v", pub.ids)
	}
	if f.count(t, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`) != 0 {
		t.Fatal("published rows must be marked published")
	}
}
