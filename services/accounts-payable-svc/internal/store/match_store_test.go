package store_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
	"zoiko.io/accounts-payable-svc/internal/store"
)

// AP-06 store suite. TEST_DATABASE_URL (owner) applies the migrations and runs the
// verification SQL; when TEST_APP_DATABASE_URL names a NOSUPERUSER NOBYPASSRLS role
// the STORE runs as that role, so row-level security really binds it.

type mfx struct {
	owner  *pgxpool.Pool
	s      *store.PgStore
	tenant string
	le     string
	ctx    context.Context
}

func matchSetup(t *testing.T) mfx {
	t.Helper()
	owner := openTestPool(t)
	storePool := owner
	if appDSN := os.Getenv("TEST_APP_DATABASE_URL"); appDSN != "" {
		requireThrowawayDatabase(t, appDSN)
		app, err := pgxpool.New(context.Background(), appDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Close)
		storePool = app
	}
	tenant := uuid.NewString()
	return mfx{owner: owner, s: store.New(storePool, zap.NewNop()), tenant: tenant, le: uuid.NewString(),
		ctx: svcmiddleware.WithTenant(context.Background(), tenant)}
}

func (f mfx) other() mfx {
	tn := uuid.NewString()
	return mfx{owner: f.owner, s: f.s, tenant: tn, le: uuid.NewString(), ctx: svcmiddleware.WithTenant(context.Background(), tn)}
}

func (f mfx) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (f mfx) events(t *testing.T, invoiceID, eventType string) int {
	return f.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, invoiceID, eventType)
}

func strp(s string) *string { return &s }

// newPOInvoice creates a PO-backed invoice with one line (10 @ 5.00) answering PO line L1.
func (f mfx) newPOInvoice(t *testing.T) *domain.VendorInvoice {
	t.Helper()
	inv := newTestInvoice(f.tenant)
	inv.LegalEntityID = f.le
	inv.Amount, inv.NetAmount = 50, 50
	inv.PurchaseOrderID = strp("po-1")
	inv.Lines = []domain.VendorInvoiceLine{{Description: "widgets", Quantity: 10, UnitPrice: 5, NetAmount: 50, POLineReference: strp("L1")}}
	if _, err := f.s.CreateInvoice(f.ctx, inv, nil); err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	got, err := f.s.GetInvoice(f.ctx, inv.InvoiceID)
	if err != nil || got == nil || len(got.Lines) != 1 {
		t.Fatalf("GetInvoice: %v %+v", err, got)
	}
	return got
}

func (f mfx) validate(t *testing.T, inv *domain.VendorInvoice) {
	t.Helper()
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusReceived, domain.InvoiceStatusValidated, "validator"); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func evidenceFor(inv *domain.VendorInvoice, mod func(*domain.MatchEvidence)) domain.MatchEvidence {
	e := domain.MatchEvidence{
		Invoice:       *inv,
		PO:            domain.POEvidence{PurchaseOrderID: "po-1", Currency: inv.CurrencyCode, Revision: 3, HasRevision: true, HasLines: true, Lines: []domain.POLineEvidence{{LineID: "L1", Quantity: 10, UnitPrice: 5}}},
		Receipts:      domain.ReceiptEvidence{HasLines: true, Received: map[string]float64{"L1": 10}},
		PriorInvoiced: map[string]float64{},
		Policy:        domain.DefaultMatchPolicy(inv.LegalEntityID),
	}
	if mod != nil {
		mod(&e)
	}
	return e
}

func (f mfx) save(t *testing.T, inv *domain.VendorInvoice, actor string, mod func(*domain.MatchEvidence)) (*domain.MatchResultView, error) {
	t.Helper()
	e := evidenceFor(inv, mod)
	rev := e.PO.Revision
	return f.s.SaveMatchRun(f.ctx, domain.SaveMatchRunInput{
		TenantID: f.tenant, InvoiceID: inv.InvoiceID, Command: "RunInvoiceMatch", Actor: actor, CorrelationID: "corr-" + uuid.NewString(),
		Policy: e.Policy, PurchaseOrderID: "po-1", PORevision: &rev, Outcome: domain.EvaluateMatch(e),
	})
}

// two variances (price and over-receipt): both waivable.
func twoVariances(e *domain.MatchEvidence) {
	e.Invoice.Lines = append([]domain.VendorInvoiceLine(nil), e.Invoice.Lines...)
	e.Invoice.Lines[0].UnitPrice, e.Invoice.Lines[0].NetAmount = 5.5, 55
	e.Receipts.Received["L1"] = 6
}

// ── runs, the invoice's match dimension, events ──────────────────────────────

func TestMatchRun_Matched_ClearsTheInvoice_AtomicallyWithHistoryAndEvents(t *testing.T) {
	f := matchSetup(t)
	inv := f.newPOInvoice(t)
	f.validate(t, inv)
	before, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID)

	v, err := f.save(t, inv, "matcher", nil)
	if err != nil || !v.Created || v.Run.Result != domain.MatchMatched || v.Run.RunNumber != 1 || !v.Cleared || v.InvoiceMatchState != domain.MatchMatched {
		t.Fatalf("expected a created, cleared MATCHED run, got %v %+v", err, v)
	}
	got, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID)
	if got.MatchState != domain.MatchMatched || !got.MatchCleared || got.MatchRunID == nil || *got.MatchRunID != v.Run.RunID || !got.MatchRequired || got.Version != before.Version+1 {
		t.Fatalf("the invoice's match dimension must follow the run, got %+v", got)
	}
	if v.Run.PORevision == nil || *v.Run.PORevision != 3 || v.Run.PolicyVersion != 0 || v.Run.Mode != domain.MatchThreeWay || v.Run.RequestedBy != "matcher" || len(v.Run.InputHash) != 64 {
		t.Fatalf("the run must freeze PO revision, policy and input hash, got %+v", v.Run)
	}
	if len(v.Lines) != 1 || v.Lines[0].Result != domain.MatchMatched || len(v.Exceptions) != 0 {
		t.Fatalf("expected one MATCHED line fact and no exceptions, got %+v", v)
	}
	if f.events(t, inv.InvoiceID, domain.EventInvoiceMatchStarted) != 1 || f.events(t, inv.InvoiceID, domain.EventInvoiceMatched) != 1 ||
		f.events(t, inv.InvoiceID, domain.EventInvoiceMatchExceptionRaised) != 0 {
		t.Fatal("expected InvoiceMatchStarted and InvoiceMatched, and no exception event")
	}
	if f.count(t, `SELECT count(*) FROM invoice_history WHERE invoice_id = $1 AND command = 'RunInvoiceMatch'`, inv.InvoiceID) != 1 {
		t.Fatal("the run must leave a history row")
	}
	// The legacy approve path now succeeds because the match is cleared.
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver"); err != nil {
		t.Fatalf("a cleared match must allow approval, got %v", err)
	}
}

// The control the earlier code only pretended to have: match_required was never set,
// so a PO-backed invoice with NO match run could be approved. Now it cannot — in
// the store, and in the database for every other path.
func TestApproval_OfAPOBackedInvoice_RequiresAClearedMatch(t *testing.T) {
	f := matchSetup(t)
	inv := f.newPOInvoice(t)
	f.validate(t, inv)

	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("no match run: approval must be refused, got %v", err)
	}
	// An EXCEPTION run does not clear it either.
	if _, err := f.save(t, inv, "matcher", twoVariances); err != nil {
		t.Fatal(err)
	}
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("an EXCEPTION run: approval must be refused, got %v", err)
	}
	// Defence in depth: the database refuses it whatever code path writes the row.
	_, err := f.owner.Exec(context.Background(), `UPDATE vendor_invoices SET approval_state = 'PENDING' WHERE invoice_id = $1`, inv.InvoiceID)
	if err == nil {
		_, err = f.owner.Exec(context.Background(), `UPDATE vendor_invoices SET approval_state = 'APPROVED' WHERE invoice_id = $1`, inv.InvoiceID)
	}
	if err == nil {
		t.Fatal("the database must refuse approving an uncleared PO-backed invoice")
	}
	if _, err := f.owner.Exec(context.Background(), `UPDATE vendor_invoices SET match_cleared = true WHERE invoice_id = $1`, inv.InvoiceID); err == nil {
		t.Fatal("match_cleared requires a run id and a cleared match state")
	}
	// An invoice with no PO is unaffected.
	plain := newTestInvoice(f.tenant)
	plain.LegalEntityID = f.le
	if _, err := f.s.CreateInvoice(f.ctx, plain, nil); err != nil {
		t.Fatal(err)
	}
	f.validate(t, plain)
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, plain.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver"); err != nil {
		t.Fatalf("an invoice with no PO needs no match, got %v", err)
	}
}

func TestMatchRun_Replay_IsNoOp_ChangedEvidenceSupersedes_AndTheChainIsKept(t *testing.T) {
	f := matchSetup(t)
	inv := f.newPOInvoice(t)
	first, err := f.save(t, inv, "matcher", nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.save(t, inv, "matcher", nil)
	if err != nil || again.Created || again.Run.RunID != first.Run.RunID {
		t.Fatalf("a re-performance on unchanged evidence returns the live run, got %v %+v", err, again)
	}
	if f.count(t, `SELECT count(*) FROM invoice_match_runs WHERE invoice_id = $1`, inv.InvoiceID) != 1 ||
		f.events(t, inv.InvoiceID, domain.EventInvoiceMatchStarted) != 1 {
		t.Fatal("a no-op re-performance writes no run and no event")
	}

	// The receipt evidence changed: a new run supersedes the old one.
	second, err := f.save(t, inv, "matcher", func(e *domain.MatchEvidence) { e.Receipts.Received["L1"] = 6 })
	if err != nil || !second.Created || second.Run.RunNumber != 2 || second.Run.Result != domain.MatchException || second.Cleared {
		t.Fatalf("expected a new EXCEPTION run #2, got %v %+v", err, second)
	}
	runs, _ := f.s.ListMatchRuns(f.ctx, f.tenant, inv.InvoiceID)
	if len(runs) != 2 || runs[0].SupersededAt != nil || runs[1].SupersededAt == nil || runs[1].SupersededByRunID == nil || *runs[1].SupersededByRunID != second.Run.RunID {
		t.Fatalf("expected the supersession chain (live #2, superseded #1), got %+v", runs)
	}
	got, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID)
	if got.MatchState != domain.MatchException || got.MatchCleared || *got.MatchRunID != second.Run.RunID {
		t.Fatalf("the invoice must follow the newer run and lose its clearance, got %+v", got)
	}
	if f.events(t, inv.InvoiceID, domain.EventInvoiceMatchSuperseded) != 1 || f.events(t, inv.InvoiceID, domain.EventInvoiceMatchExceptionRaised) != 1 {
		t.Fatal("expected a supersession event and an exception event")
	}
	latest, _ := f.s.GetMatchResult(f.ctx, f.tenant, inv.InvoiceID)
	if latest.Run.RunID != second.Run.RunID || len(latest.Exceptions) != 1 {
		t.Fatalf("GetMatchResult returns the live run with its exceptions, got %+v", latest)
	}
}

func TestMatchRun_Concurrent_SameEvidence_ProducesExactlyOneRun(t *testing.T) {
	f := matchSetup(t)
	inv := f.newPOInvoice(t)
	var wg sync.WaitGroup
	created := make([]bool, 6)
	errs := make([]error, 6)
	for i := range created {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := f.save(t, inv, "matcher", nil)
			errs[i] = err
			if v != nil {
				created[i] = v.Created
			}
		}(i)
	}
	wg.Wait()
	n := 0
	for i := range created {
		if errs[i] != nil {
			t.Fatalf("unexpected error: %v", errs[i])
		}
		if created[i] {
			n++
		}
	}
	if n != 1 || f.count(t, `SELECT count(*) FROM invoice_match_runs WHERE invoice_id = $1`, inv.InvoiceID) != 1 {
		t.Fatalf("exactly one concurrent run may be created, got %d", n)
	}
}

func TestMatchRun_Guards(t *testing.T) {
	f := matchSetup(t)
	// No PO: matching does not apply.
	plain := newTestInvoice(f.tenant)
	plain.LegalEntityID = f.le
	f.s.CreateInvoice(f.ctx, plain, nil) //nolint:errcheck
	if _, err := f.s.SaveMatchRun(f.ctx, domain.SaveMatchRunInput{TenantID: f.tenant, InvoiceID: plain.InvoiceID, Actor: "m",
		Outcome: domain.MatchOutcome{Result: domain.MatchMatched, InputHash: "h"}}); !errors.Is(err, domain.ErrMatchNotApplicable) {
		t.Fatalf("expected ErrMatchNotApplicable, got %v", err)
	}
	// Stale expected_version.
	inv := f.newPOInvoice(t)
	stale := 99
	e := evidenceFor(inv, nil)
	if _, err := f.s.SaveMatchRun(f.ctx, domain.SaveMatchRunInput{TenantID: f.tenant, InvoiceID: inv.InvoiceID, ExpectedVersion: &stale, Actor: "m",
		Policy: e.Policy, PurchaseOrderID: "po-1", Outcome: domain.EvaluateMatch(e)}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("expected ErrStaleVersion, got %v", err)
	}
	// An approved invoice can no longer be re-matched or superseded.
	if _, err := f.save(t, inv, "matcher", nil); err != nil {
		t.Fatal(err)
	}
	f.validate(t, inv)
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.save(t, inv, "matcher", func(e *domain.MatchEvidence) { e.Receipts.Received["L1"] = 1 }); !errors.Is(err, domain.ErrMatchInvoiceFinal) {
		t.Fatalf("an approved invoice cannot be re-matched, got %v", err)
	}
	if _, err := f.s.SupersedeMatchRun(f.ctx, domain.SupersedeInput{TenantID: f.tenant, InvoiceID: inv.InvoiceID, Actor: "x", Reason: "r"}); !errors.Is(err, domain.ErrMatchInvoiceFinal) {
		t.Fatalf("an approved invoice's run cannot be superseded, got %v", err)
	}
	// Unknown / malformed ids.
	if _, err := f.s.GetMatchResult(f.ctx, f.tenant, uuid.NewString()); !errors.Is(err, domain.ErrMatchRunNotFound) {
		t.Fatalf("expected ErrMatchRunNotFound, got %v", err)
	}
	if _, err := f.s.GetMatchResult(f.ctx, f.tenant, "not-a-uuid"); !errors.Is(err, domain.ErrMatchRunNotFound) {
		t.Fatalf("a malformed id is not found, not an outage, got %v", err)
	}
	if _, err := f.s.GetMatchException(f.ctx, f.tenant, "not-a-uuid"); !errors.Is(err, domain.ErrMatchExceptionNotFound) {
		t.Fatalf("a malformed exception id is not found, got %v", err)
	}
}

func TestSupersedeMatchRun_InvalidatesClearance_NextRunIsNumberedOn(t *testing.T) {
	f := matchSetup(t)
	inv := f.newPOInvoice(t)
	f.validate(t, inv)
	first, _ := f.save(t, inv, "matcher", nil)
	out, err := f.s.SupersedeMatchRun(f.ctx, domain.SupersedeInput{TenantID: f.tenant, InvoiceID: inv.InvoiceID, Actor: "controller", Reason: "PO amended", CorrelationID: "c"})
	if err != nil || out.MatchState != domain.MatchNotMatched || out.MatchCleared || out.MatchRunID != nil {
		t.Fatalf("expected NOT_MATCHED and uncleared, got %v %+v", err, out)
	}
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a superseded match no longer clears approval, got %v", err)
	}
	if _, err := f.s.SupersedeMatchRun(f.ctx, domain.SupersedeInput{TenantID: f.tenant, InvoiceID: inv.InvoiceID, Actor: "controller", Reason: "again"}); !errors.Is(err, domain.ErrMatchRunNotFound) {
		t.Fatalf("there is no live run left to supersede, got %v", err)
	}
	// Identical evidence again is a NEW run (the old one was invalidated, not replaced).
	again, err := f.save(t, inv, "matcher", nil)
	if err != nil || !again.Created || again.Run.RunNumber != 2 || again.Run.RunID == first.Run.RunID {
		t.Fatalf("expected run #2, got %v %+v", err, again)
	}
	if f.events(t, inv.InvoiceID, domain.EventInvoiceMatchSuperseded) != 1 {
		t.Fatal("expected one supersession event")
	}
}

// ── exceptions, variance approval, segregation of duties ─────────────────────

func TestVarianceApproval_ClearsTheInvoiceOnlyWhenEveryFindingIsApproved_UnderSoD(t *testing.T) {
	f := matchSetup(t)
	inv := f.newPOInvoice(t) // created_by_principal_id = "test-admin"
	f.validate(t, inv)
	v, err := f.save(t, inv, "matcher", twoVariances)
	if err != nil || v.Run.Result != domain.MatchException || len(v.Exceptions) != 2 {
		t.Fatalf("expected EXCEPTION with two findings, got %v %+v", err, v)
	}
	act := func(id, kind, actor string) (*domain.MatchExceptionRecord, *domain.VendorInvoice, error) {
		return f.s.ResolveMatchException(f.ctx, domain.ExceptionAction{TenantID: f.tenant, ExceptionID: id, Kind: kind, Actor: actor,
			Reason: "agreed with supplier", Ref: "CR-77", RouteTo: "ap-lead", CorrelationID: "c"})
	}
	x0, x1 := v.Exceptions[0].ExceptionID, v.Exceptions[1].ExceptionID

	// Negative path 3: nobody who ran the match, or created the invoice, waives its variance.
	if _, _, err := act(x0, domain.ActionApproveVariance, "matcher"); !errors.Is(err, domain.ErrMatchSelfWaiver) {
		t.Fatalf("the run requester cannot approve, got %v", err)
	}
	if _, _, err := act(x0, domain.ActionApproveVariance, "test-admin"); !errors.Is(err, domain.ErrMatchSelfWaiver) {
		t.Fatalf("the invoice creator cannot approve, got %v", err)
	}
	// ...and the database enforces it too, whatever the application does.
	if _, err := f.owner.Exec(context.Background(), `UPDATE invoice_match_exceptions SET status = 'VARIANCE_APPROVED', resolved_by = 'matcher', resolved_at = NOW(), resolution_reason = 'x' WHERE exception_id = $1`, x0); err == nil {
		t.Fatal("the trigger must refuse a self-waiver")
	}

	// First approval: still not cleared.
	e0, invA, err := act(x0, domain.ActionApproveVariance, "controller")
	if err != nil || e0.Status != domain.ExceptionVarianceApproved || e0.ResolvedBy != "controller" || e0.ResolutionRef != "CR-77" || invA.MatchCleared {
		t.Fatalf("one approved of two must not clear the invoice, got %v %+v %+v", err, e0, invA)
	}
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("approval still blocked, got %v", err)
	}
	if _, _, err := act(x0, domain.ActionApproveVariance, "controller2"); !errors.Is(err, domain.ErrExceptionTransition) {
		t.Fatalf("an approved variance is final, got %v", err)
	}
	// Second approval clears it.
	_, invB, err := act(x1, domain.ActionApproveVariance, "controller2")
	if err != nil || !invB.MatchCleared || invB.MatchState != domain.MatchWithinTolerance {
		t.Fatalf("all findings approved must clear the invoice, got %v %+v", err, invB)
	}
	if f.events(t, inv.InvoiceID, domain.EventInvoiceMatchVarianceApproved) != 2 || f.events(t, inv.InvoiceID, domain.EventInvoiceMatched) != 1 {
		t.Fatal("expected two variance events and one InvoiceMatched on clearing")
	}
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver"); err != nil {
		t.Fatalf("with every variance independently approved the invoice may be approved, got %v", err)
	}
	if f.count(t, `SELECT count(*) FROM invoice_history WHERE invoice_id = $1 AND command = 'RecordApprovedVariance'`, inv.InvoiceID) != 2 {
		t.Fatal("each decision leaves a history row")
	}
}

func TestExceptions_NotWaivable_Acknowledge_Route_Transitions(t *testing.T) {
	f := matchSetup(t)
	inv := f.newPOInvoice(t)
	// Missing receipt: evidence is missing, so the finding cannot be waived.
	v, err := f.save(t, inv, "matcher", func(e *domain.MatchEvidence) { e.Receipts.Received = map[string]float64{} })
	if err != nil || v.Run.Result != domain.MatchIncomplete || len(v.Exceptions) != 1 || v.Exceptions[0].Waivable {
		t.Fatalf("expected one non-waivable INCOMPLETE finding, got %v %+v", err, v)
	}
	id := v.Exceptions[0].ExceptionID
	do := func(kind, actor string) (*domain.MatchExceptionRecord, error) {
		x, _, err := f.s.ResolveMatchException(f.ctx, domain.ExceptionAction{TenantID: f.tenant, ExceptionID: id, Kind: kind, Actor: actor, Reason: "r", RouteTo: "ap-lead"})
		return x, err
	}
	if _, err := do(domain.ActionApproveVariance, "controller"); !errors.Is(err, domain.ErrExceptionNotWaivable) {
		t.Fatalf("missing evidence can never be waived, got %v", err)
	}
	if _, err := f.owner.Exec(context.Background(), `UPDATE invoice_match_exceptions SET status = 'VARIANCE_APPROVED', resolved_by = 'z', resolved_at = NOW(), resolution_reason = 'x' WHERE exception_id = $1`, id); err == nil {
		t.Fatal("the database must refuse waiving a non-waivable finding")
	}
	x, err := do(domain.ActionAcknowledge, "clerk")
	if err != nil || x.Status != domain.ExceptionAcknowledged || x.AcknowledgedBy != "clerk" {
		t.Fatalf("acknowledge: %v %+v", err, x)
	}
	if _, err := do(domain.ActionAcknowledge, "clerk"); !errors.Is(err, domain.ErrExceptionTransition) {
		t.Fatalf("acknowledging twice, got %v", err)
	}
	x, err = do(domain.ActionRoute, "clerk")
	if err != nil || x.Status != domain.ExceptionRouted || x.RoutedTo != "ap-lead" || x.RoutedBy != "clerk" {
		t.Fatalf("route: %v %+v", err, x)
	}
	if _, err := do(domain.ActionRoute, "clerk"); !errors.Is(err, domain.ErrExceptionTransition) {
		t.Fatalf("routing twice, got %v", err)
	}
	if got, _ := f.s.GetMatchResult(f.ctx, f.tenant, inv.InvoiceID); got.Cleared || got.InvoiceMatchState != domain.MatchIncomplete {
		t.Fatalf("acknowledging or routing never clears anything, got %+v", got)
	}
	// A superseded run's exceptions are history: they can no longer be acted on.
	if _, err := f.save(t, inv, "matcher", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := do(domain.ActionAcknowledge, "clerk"); !errors.Is(err, domain.ErrMatchRunSuperseded) {
		t.Fatalf("expected ErrMatchRunSuperseded, got %v", err)
	}
	if list, _ := f.s.ListMatchExceptions(f.ctx, domain.ExceptionFilter{TenantID: f.tenant, LegalEntityID: f.le}); len(list) != 0 {
		t.Fatalf("only live runs' exceptions are listed, got %d", len(list))
	}
}

func TestListMatchExceptions_FiltersAndLiveOnly(t *testing.T) {
	f := matchSetup(t)
	a, b := f.newPOInvoice(t), f.newPOInvoice(t)
	va, _ := f.save(t, a, "matcher", twoVariances)
	f.save(t, b, "matcher", func(e *domain.MatchEvidence) { e.Receipts.Received = map[string]float64{} })                                                                   //nolint:errcheck
	f.s.ResolveMatchException(f.ctx, domain.ExceptionAction{TenantID: f.tenant, ExceptionID: va.Exceptions[0].ExceptionID, Kind: domain.ActionAcknowledge, Actor: "clerk"}) //nolint:errcheck

	all, err1 := f.s.ListMatchExceptions(f.ctx, domain.ExceptionFilter{TenantID: f.tenant, LegalEntityID: f.le})
	open, err2 := f.s.ListMatchExceptions(f.ctx, domain.ExceptionFilter{TenantID: f.tenant, LegalEntityID: f.le, Status: domain.ExceptionOpen})
	byInv, err3 := f.s.ListMatchExceptions(f.ctx, domain.ExceptionFilter{TenantID: f.tenant, LegalEntityID: f.le, InvoiceID: b.InvoiceID})
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("list failed: %v %v %v", err1, err2, err3)
	}
	if len(all) != 3 || len(open) != 2 || len(byInv) != 1 {
		t.Fatalf("expected 3 total, 2 open, 1 for invoice b; got %d / %d / %d", len(all), len(open), len(byInv))
	}
}

// ── immutability ─────────────────────────────────────────────────────────────

func TestImmutability_Runs_Lines_Policies_ExceptionFindings(t *testing.T) {
	f := matchSetup(t)
	ctx := context.Background()
	inv := f.newPOInvoice(t)
	v, err := f.save(t, inv, "matcher", twoVariances)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.s.CreateMatchPolicy(f.ctx, f.tenant, domain.MatchPolicy{LegalEntityID: f.le, Mode: domain.MatchThreeWay, PriceTolerancePct: 1, CreatedBy: "policy-admin"})
	if err != nil {
		t.Fatal(err)
	}
	runID, excID := v.Run.RunID, v.Exceptions[0].ExceptionID
	refused := map[string]string{
		"rewrite a run's result":        `UPDATE invoice_match_runs SET result = 'MATCHED' WHERE run_id = '` + runID + `'`,
		"rewrite a run's frozen policy": `UPDATE invoice_match_runs SET policy_snapshot = '{}'::jsonb WHERE run_id = '` + runID + `'`,
		"rewrite a run's input hash":    `UPDATE invoice_match_runs SET input_hash = 'x' WHERE run_id = '` + runID + `'`,
		"delete a run":                  `DELETE FROM invoice_match_runs WHERE run_id = '` + runID + `'`,
		"edit a line fact":              `UPDATE invoice_match_lines SET result = 'MATCHED' WHERE run_id = '` + runID + `'`,
		"delete a line fact":            `DELETE FROM invoice_match_lines WHERE run_id = '` + runID + `'`,
		"edit a policy version":         `UPDATE match_policy_versions SET price_tolerance_pct = 50 WHERE legal_entity_id = '` + f.le + `'`,
		"delete a policy version":       `DELETE FROM match_policy_versions WHERE legal_entity_id = '` + f.le + `'`,
		"rewrite a finding's amount":    `UPDATE invoice_match_exceptions SET difference = 0 WHERE exception_id = '` + excID + `'`,
		"rewrite a finding's category":  `UPDATE invoice_match_exceptions SET category = 'OTHER' WHERE exception_id = '` + excID + `'`,
		"make a finding non-waivable":   `UPDATE invoice_match_exceptions SET waivable = false WHERE exception_id = '` + excID + `'`,
		"delete a finding":              `DELETE FROM invoice_match_exceptions WHERE exception_id = '` + excID + `'`,
		"a non-positive policy version": `INSERT INTO match_policy_versions (tenant_id, legal_entity_id, policy_version, mode, created_by) VALUES ('` + f.tenant + `', '` + f.le + `', 0, 'TWO_WAY', 'x')`,
		"a negative tolerance":          `INSERT INTO match_policy_versions (tenant_id, legal_entity_id, policy_version, mode, price_tolerance_pct, created_by) VALUES ('` + f.tenant + `', '` + f.le + `', 9, 'TWO_WAY', -1, 'x')`,
	}
	for name, sql := range refused {
		if _, err := f.owner.Exec(ctx, sql); err == nil {
			t.Fatalf("%s: the database must refuse it", name)
		}
	}
	_ = p
	// A run's only permitted change is supersession, and it is final.
	if _, err := f.owner.Exec(ctx, `UPDATE invoice_match_runs SET superseded_at = NOW(), supersede_reason = 'x' WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("supersession must be allowed: %v", err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE invoice_match_runs SET supersede_reason = 'y' WHERE run_id = $1`, runID); err == nil {
		t.Fatal("a superseded run cannot change again")
	}
}

// ── policy ───────────────────────────────────────────────────────────────────

func TestMatchPolicy_Versions_AppendOnly_ConcurrentSafe_TenantScoped(t *testing.T) {
	f := matchSetup(t)
	if _, err := f.s.GetMatchPolicy(f.ctx, f.tenant, f.le, 0); !errors.Is(err, domain.ErrMatchPolicyNotFound) {
		t.Fatalf("no policy configured yet, got %v", err)
	}
	p1, err := f.s.CreateMatchPolicy(f.ctx, f.tenant, domain.MatchPolicy{LegalEntityID: f.le, Mode: domain.MatchThreeWay, PriceTolerancePct: 1, Reason: "initial", CreatedBy: "pm"})
	if err != nil || p1.PolicyVersion != 1 || p1.Mode != domain.MatchThreeWay || p1.PriceTolerancePct != 1 || p1.CreatedBy != "pm" {
		t.Fatalf("expected version 1, got %v %+v", err, p1)
	}
	// Concurrent creates get distinct, gapless versions.
	var wg sync.WaitGroup
	vers := make([]int, 5)
	for i := range vers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := f.s.CreateMatchPolicy(f.ctx, f.tenant, domain.MatchPolicy{LegalEntityID: f.le, Mode: domain.MatchTwoWay, QtyTolerancePct: float64(i), CreatedBy: "pm"})
			if err == nil {
				vers[i] = p.PolicyVersion
			}
		}(i)
	}
	wg.Wait()
	seen := map[int]bool{}
	for _, v := range vers {
		if v < 2 || v > 6 || seen[v] {
			t.Fatalf("expected distinct versions 2..6, got %v", vers)
		}
		seen[v] = true
	}
	latest, _ := f.s.GetMatchPolicy(f.ctx, f.tenant, f.le, 0)
	v1, _ := f.s.GetMatchPolicy(f.ctx, f.tenant, f.le, 1)
	if latest.PolicyVersion != 6 || v1.PolicyVersion != 1 || v1.PriceTolerancePct != 1 {
		t.Fatalf("latest/specific version reads wrong: %+v / %+v", latest, v1)
	}
	if _, err := f.s.GetMatchPolicy(f.ctx, f.tenant, f.le, 99); !errors.Is(err, domain.ErrMatchPolicyNotFound) {
		t.Fatalf("unknown version, got %v", err)
	}
	if _, err := f.s.CreateMatchPolicy(f.ctx, f.tenant, domain.MatchPolicy{LegalEntityID: f.le, Mode: "FOUR_WAY", CreatedBy: "pm"}); !errors.Is(err, domain.ErrMatchPolicyInvalid) {
		t.Fatalf("an invalid policy is refused, got %v", err)
	}
	o := f.other()
	if _, err := o.s.GetMatchPolicy(o.ctx, o.tenant, f.le, 0); !errors.Is(err, domain.ErrMatchPolicyNotFound) {
		t.Fatalf("another tenant sees no policy, got %v", err)
	}
	// A frozen policy keeps a run reproducible: the run carries its own snapshot.
	inv := f.newPOInvoice(t)
	e := evidenceFor(inv, func(e *domain.MatchEvidence) { e.Policy = *p1 })
	v, err := f.s.SaveMatchRun(f.ctx, domain.SaveMatchRunInput{TenantID: f.tenant, InvoiceID: inv.InvoiceID, Actor: "m", Policy: *p1, PurchaseOrderID: "po-1", Outcome: domain.EvaluateMatch(e)})
	if err != nil || v.Run.PolicyVersion != 1 || v.Run.PolicySnapshot.PriceTolerancePct != 1 {
		t.Fatalf("the run must snapshot its policy, got %v %+v", err, v)
	}
}

// ── isolation & atomicity ────────────────────────────────────────────────────

func TestTenantIsolation_Matching(t *testing.T) {
	f := matchSetup(t)
	inv := f.newPOInvoice(t)
	v, _ := f.save(t, inv, "matcher", twoVariances)
	o := f.other()

	if _, err := o.s.GetMatchResult(o.ctx, o.tenant, inv.InvoiceID); !errors.Is(err, domain.ErrMatchRunNotFound) {
		t.Fatalf("another tenant sees no run, got %v", err)
	}
	if runs, _ := o.s.ListMatchRuns(o.ctx, o.tenant, inv.InvoiceID); len(runs) != 0 {
		t.Fatal("another tenant lists no runs")
	}
	if list, _ := o.s.ListMatchExceptions(o.ctx, domain.ExceptionFilter{TenantID: o.tenant, LegalEntityID: f.le}); len(list) != 0 {
		t.Fatal("another tenant lists no exceptions")
	}
	if _, err := o.s.GetMatchException(o.ctx, o.tenant, v.Exceptions[0].ExceptionID); !errors.Is(err, domain.ErrMatchExceptionNotFound) {
		t.Fatalf("another tenant cannot read the exception, got %v", err)
	}
	if _, _, err := o.s.ResolveMatchException(o.ctx, domain.ExceptionAction{TenantID: o.tenant, ExceptionID: v.Exceptions[0].ExceptionID, Kind: domain.ActionAcknowledge, Actor: "attacker"}); !errors.Is(err, domain.ErrMatchExceptionNotFound) {
		t.Fatalf("another tenant cannot act on the exception, got %v", err)
	}
	if _, err := o.s.SaveMatchRun(o.ctx, domain.SaveMatchRunInput{TenantID: o.tenant, InvoiceID: inv.InvoiceID, Actor: "attacker",
		Outcome: domain.MatchOutcome{Result: domain.MatchMatched, InputHash: "h"}}); !errors.Is(err, domain.ErrInvoiceNotFound) {
		t.Fatalf("another tenant cannot match the invoice, got %v", err)
	}
	if _, err := o.s.SupersedeMatchRun(o.ctx, domain.SupersedeInput{TenantID: o.tenant, InvoiceID: inv.InvoiceID, Actor: "attacker", Reason: "r"}); !errors.Is(err, domain.ErrInvoiceNotFound) {
		t.Fatalf("another tenant cannot supersede, got %v", err)
	}
	got, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID)
	if got.MatchState != domain.MatchException || got.MatchCleared {
		t.Fatalf("the owner's invoice is untouched, got %+v", got)
	}
}

// The run, its line facts, its exceptions and the invoice change commit together.
func TestSaveMatchRun_IsAtomic(t *testing.T) {
	f := matchSetup(t)
	inv := f.newPOInvoice(t)
	f.validate(t, inv)
	before, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID)
	ctx := context.Background()
	if _, err := f.owner.Exec(ctx, `ALTER TABLE invoice_match_exceptions RENAME TO invoice_match_exceptions_tmp`); err != nil {
		t.Fatal(err)
	}
	_, saveErr := f.save(t, inv, "matcher", twoVariances)
	if _, err := f.owner.Exec(ctx, `ALTER TABLE invoice_match_exceptions_tmp RENAME TO invoice_match_exceptions`); err != nil {
		t.Fatal(err)
	}
	if saveErr == nil {
		t.Fatal("the run must fail when its exceptions cannot be written")
	}
	after, _ := f.s.GetInvoice(f.ctx, inv.InvoiceID)
	if after.Version != before.Version || after.MatchState != before.MatchState || after.MatchRunID != nil {
		t.Fatalf("the invoice must be exactly as before, got %+v", after)
	}
	if f.count(t, `SELECT count(*) FROM invoice_match_runs WHERE invoice_id = $1`, inv.InvoiceID) != 0 ||
		f.count(t, `SELECT count(*) FROM invoice_match_lines WHERE invoice_id = $1`, inv.InvoiceID) != 0 ||
		f.events(t, inv.InvoiceID, domain.EventInvoiceMatchStarted) != 0 {
		t.Fatal("no run, line fact or event may survive the rollback")
	}
	if v, err := f.save(t, inv, "matcher", twoVariances); err != nil || !v.Created {
		t.Fatalf("once healthy, the same run succeeds, got %v", err)
	}
}

var _ = time.Second

// An invoice validated BEFORE AP-06 existed has match_required = false. The gate
// derives the requirement from the PO reference, so such an in-flight invoice cannot
// slip through to approval unmatched.
func TestApproval_InFlightInvoiceValidatedBeforeAP06_StillNeedsAMatch(t *testing.T) {
	f := matchSetup(t)
	inv := f.newPOInvoice(t)
	f.validate(t, inv)
	if _, err := f.owner.Exec(context.Background(), `UPDATE vendor_invoices SET match_required = false WHERE invoice_id = $1`, inv.InvoiceID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.TransitionInvoice(f.ctx, f.tenant, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "approver"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("an in-flight PO-backed invoice must still be refused without a match, got %v", err)
	}
}
