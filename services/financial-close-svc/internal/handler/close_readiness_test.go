package handler_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

// These tests pin what blocks a period close (ACC-14, ACC-06) using the
// worked example the change was made for — October 2026 for entity le-1:
//
//   INV-101  receivable posted to the GL, customer pays in November
//   BILL-55  payable posted to the GL, paid in a November payment run
//   INV-102  £5,000 invoice whose accounting never reached the ledger
//
// The close used to block on INV-101 and BILL-55 (unpaid) and pass INV-102.

var (
	octStart = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	octEnd   = time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
)

func octoberStore() *stubStore {
	s := newStubStore()
	s.periods["fp-oct"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-oct", TenantID: testTenantID, LegalEntityID: "le-1",
		PeriodName: "2026-10", PeriodStart: octStart, PeriodEnd: octEnd, CloseStatus: "OPEN",
	}
	return s
}

func controlRun(ledger, status string, at time.Time, subledgerTotal, glBalance float64) domain.SubledgerControlRun {
	account := map[string]string{"AR": "1100", "AP": "2100"}[ledger]
	return domain.SubledgerControlRun{
		ControlRunID: ledger + "-" + status + "-" + at.Format("0102150405"), TenantID: testTenantID,
		LegalEntityID: "le-1", FiscalPeriod: "2026-10", Subledger: ledger, ControlAccountCode: account,
		SubledgerTotalAmount: subledgerTotal, GLControlBalanceAmount: glBalance,
		DifferenceAmount: subledgerTotal - glBalance, Status: status, RunAt: at,
	}
}

// bothMatched: AR and AP each agree with their GL control account. INV-101's
// £1,200 is in the AR total AND the GL, which is exactly why it must not block.
func bothMatched(s *stubStore) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s.controlRuns = append(s.controlRuns,
		controlRun("AR", "MATCHED", at, 1200, 1200),
		controlRun("AP", "MATCHED", at, 800, 800))
}

func lock(t *testing.T, s *stubStore, cl *stubClients) (int, domain.ReadinessCheckResponse, string) {
	t.Helper()
	rr := doReq(newGatedRouter(s, cl), http.MethodPost, "/v1/close/periods/fp-oct/lock", nil, "controller-1")
	var resp domain.ReadinessCheckResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr.Code, resp, rr.Body.String()
}

func requireBlockedWith(t *testing.T, code int, resp domain.ReadinessCheckResponse, body string, wants ...string) {
	t.Helper()
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("expected the close to be blocked (422), got %d: %s", code, body)
	}
	joined := strings.Join(resp.BlockingIssues, "\n")
	for _, want := range wants {
		if !strings.Contains(joined, want) {
			t.Fatalf("blocking issues do not mention %q:\n%s", want, joined)
		}
	}
}

// INV-101 and BILL-55: the books are complete and agree; unpaid items are
// correct month-end balances and must not stop the close.
func TestClose_UnpaidButPostedItemsDoNotBlock(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	code, _, body := lock(t, s, &stubClients{})
	if code != http.StatusOK {
		t.Fatalf("a period whose books are complete must close, got %d: %s", code, body)
	}
	if !strings.Contains(body, `"LOCKED"`) {
		t.Fatalf("expected the period LOCKED: %s", body)
	}
}

// INV-102, variant 1: GL accepted the event and failed to commit it. The
// posting backlog names it.
func TestClose_PostingBacklogBlocks(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	cl := &stubClients{backlog: domain.PostingBacklog{Count: 1, Samples: []domain.PostingBacklogItem{
		{Reference: "inv-102-issued", Status: "FAILED", FailureReason: "journal write failed"},
	}}}
	code, resp, body := lock(t, s, cl)
	requireBlockedWith(t, code, resp, body,
		"unposted_accounting_events: 1 accounting event", "inv-102-issued (FAILED: journal write failed)")
}

// INV-102, variant 2: the event never reached GL at all (rejected at intake),
// so GL has no backlog for it. The AR subledger is £5,000 ahead of the GL
// control account, and the ACC-06 run says so.
func TestClose_SubledgerDisagreementBlocks(t *testing.T) {
	s := octoberStore()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s.controlRuns = append(s.controlRuns,
		controlRun("AR", "EXCEPTION", at, 6200, 1200),
		controlRun("AP", "MATCHED", at, 800, 800))
	code, resp, body := lock(t, s, &stubClients{})
	requireBlockedWith(t, code, resp, body,
		"subledger_control_exception: AR subledger total 6200.00", "GL control account 1100 balance 1200.00", "difference 5000.00")
}

func TestClose_MissingControlRunBlocks(t *testing.T) {
	s := octoberStore()
	s.controlRuns = append(s.controlRuns, controlRun("AR", "MATCHED", time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), 1200, 1200))
	code, resp, body := lock(t, s, &stubClients{})
	requireBlockedWith(t, code, resp, body, "subledger_control_not_run: no AP subledger-to-GL control run for 2026-10")
	for _, issue := range resp.BlockingIssues {
		if strings.Contains(issue, " AR ") {
			t.Fatalf("AR was matched and must not be reported: %s", issue)
		}
	}
}

// Only the latest run for a subledger counts.
func TestClose_LatestControlRunDecides(t *testing.T) {
	early := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	late := early.Add(3 * time.Hour)

	t.Run("exception since fixed and re-run does not block", func(t *testing.T) {
		s := octoberStore()
		s.controlRuns = append(s.controlRuns,
			controlRun("AR", "EXCEPTION", early, 6200, 1200),
			controlRun("AR", "MATCHED", late, 6200, 6200),
			controlRun("AP", "MATCHED", early, 800, 800))
		if code, _, body := lock(t, s, &stubClients{}); code != http.StatusOK {
			t.Fatalf("a superseded exception must not block, got %d: %s", code, body)
		}
	})
	t.Run("a match later broken by new postings blocks", func(t *testing.T) {
		s := octoberStore()
		s.controlRuns = append(s.controlRuns,
			controlRun("AR", "MATCHED", early, 1200, 1200),
			controlRun("AR", "EXCEPTION", late, 6200, 1200),
			controlRun("AP", "MATCHED", early, 800, 800))
		code, resp, body := lock(t, s, &stubClients{})
		requireBlockedWith(t, code, resp, body, "subledger_control_exception: AR")
	})
}

// Everything GL accepted up to the end of the period's last day is in scope.
func TestClose_PostingBacklogCutoffIsTheDayAfterPeriodEnd(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	cl := &stubClients{}
	lock(t, s, cl)
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if !cl.backlogCutoff.Equal(want) {
		t.Fatalf("posting backlog cutoff = %v, want %v", cl.backlogCutoff, want)
	}
}

func TestClose_PostingBacklogTooLargeBlocks(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	code, resp, body := lock(t, s, &stubClients{backlog: domain.PostingBacklog{TooLarge: true}})
	requireBlockedWith(t, code, resp, body, "unposted_accounting_events: the posting backlog exceeds")
}

// "Could not check" must never read as "nothing to report".
func TestClose_FailsClosedWhenItCannotCheck(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*stubStore, *stubClients)
		wantCode int
		wantText string
	}{
		{"GL unreachable", func(_ *stubStore, cl *stubClients) { cl.backlogErr = domain.ErrGLServiceUnavailable },
			http.StatusServiceUnavailable, "general-ledger-svc"},
		{"principal may not read the backlog", func(_ *stubStore, cl *stubClients) { cl.backlogErr = domain.ErrPostingBacklogForbidden },
			http.StatusForbidden, "posting_backlog_not_permitted"},
		{"control runs unreadable", func(s *stubStore, _ *stubClients) { s.listRunsErr = domain.ErrStoreUnavailable },
			http.StatusServiceUnavailable, "subledger control runs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := octoberStore()
			bothMatched(s)
			cl := &stubClients{}
			c.mutate(s, cl)
			code, _, body := lock(t, s, cl)
			if code != c.wantCode || !strings.Contains(body, c.wantText) {
				t.Fatalf("got %d %s; want %d mentioning %q", code, body, c.wantCode, c.wantText)
			}
			if s.periods["fp-oct"].CloseStatus != "OPEN" {
				t.Fatal("the period was closed although a check could not run")
			}
		})
	}
}

// SUBLEDGER_CONTROL_GATE_MODE=off: no runs required, everything else still is.
func TestClose_SubledgerGateOffSkipsOnlyTheRuns(t *testing.T) {
	s := octoberStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	if rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-oct/lock", nil, "controller-1"); rr.Code != http.StatusOK {
		t.Fatalf("gate off: a period with no control runs should close, got %d: %s", rr.Code, rr.Body.String())
	}

	s2 := octoberStore()
	r2 := newRouter(s2, &stubPublisher{}, &stubAuthZ{}, &stubClients{backlog: domain.PostingBacklog{Count: 2}})
	if rr := doReq(r2, http.MethodPost, "/v1/close/periods/fp-oct/lock", nil, "controller-1"); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("gate off must not disable the posting backlog check, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── The entity's close checklist (ACC-14 checklist items) ───────────────────

func requireControl(s *stubStore, subledger, book string) *domain.CloseRequirement {
	cr := &domain.CloseRequirement{RequirementID: subledger + "-" + book, TenantID: testTenantID,
		LegalEntityID: "le-1", Kind: domain.CloseRequirementSubledgerControl, Subledger: subledger, BookID: book}
	s.closeRequirements = append(s.closeRequirements, cr)
	return cr
}

func bookRun(ledger, book, status string, at time.Time, actual, expected float64) domain.SubledgerControlRun {
	run := controlRun(ledger, status, at, actual, expected)
	run.BookID = book
	run.ControlRunID = ledger + "-" + book + "-" + status
	return run
}

// The company runs fixed assets and inventory: every control its checklist
// names must have matched, and then it closes.
func TestClose_ChecklistControlsAllMatched_Closes(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	requireControl(s, "ASSETS", "STATUTORY")
	requireControl(s, "INVENTORY_VALUE", "")
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	s.controlRuns = append(s.controlRuns,
		bookRun("ASSETS", "STATUTORY", "MATCHED", at, 90000, 90000),
		bookRun("INVENTORY_VALUE", "", "MATCHED", at, 12000, 12000))
	if code, _, body := lock(t, s, &stubClients{}); code != http.StatusOK {
		t.Fatalf("all required controls matched, expected the close: %d %s", code, body)
	}
}

// Required controls are blockers whether never run or failed.
func TestClose_ChecklistControlMissingBlocks(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	requireControl(s, "INVENTORY_VALUE", "")
	code, resp, body := lock(t, s, &stubClients{})
	requireBlockedWith(t, code, resp, body, "subledger_control_not_run: no INVENTORY_VALUE subledger-to-GL control run for 2026-10")
}

// An ASSETS run proves one book. A matched TAX-book run says nothing about
// the STATUTORY book the checklist requires.
func TestClose_AssetRunForAnotherBookDoesNotCount(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	requireControl(s, "ASSETS", "STATUTORY")
	s.controlRuns = append(s.controlRuns,
		bookRun("ASSETS", "TAX", "MATCHED", time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC), 70000, 70000))
	code, resp, body := lock(t, s, &stubClients{})
	requireBlockedWith(t, code, resp, body, "no ASSETS (book STATUTORY) subledger-to-GL control run")
}

// Completeness controls compare counts; the blocker says so in those terms.
// October's depreciation was posted for 2 of 5 assets.
func TestClose_CompletenessControlExceptionBlocks(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	requireControl(s, "DEPRECIATION_COMPLETENESS", "")
	s.controlRuns = append(s.controlRuns,
		bookRun("DEPRECIATION_COMPLETENESS", "", "EXCEPTION", time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC), 2, 5))
	code, resp, body := lock(t, s, &stubClients{})
	requireBlockedWith(t, code, resp, body, "subledger_control_exception: DEPRECIATION_COMPLETENESS: depreciation posted for 2 of 5 eligible assets")
}

// A removed checklist item is no longer required; the removal is evidence,
// not a blocker.
func TestClose_RemovedChecklistItemIsNotRequired(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	cr := requireControl(s, "STOCK_COUNT", "")
	at := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	cr.RemovedAt, cr.RemovedByPrincipalID, cr.RemovalReason = &at, "controller-2", "no stock held since September"
	if code, _, body := lock(t, s, &stubClients{}); code != http.StatusOK {
		t.Fatalf("a removed requirement still blocked: %d %s", code, body)
	}
}

// A bank-account exclusion is not a subledger control and requires nothing here.
func TestClose_BankExclusionIsNotASubledgerRequirement(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	s.closeRequirements = append(s.closeRequirements, &domain.CloseRequirement{RequirementID: "x", TenantID: testTenantID,
		LegalEntityID: "le-1", Kind: domain.CloseRequirementBankAccountExclusion, BankAccountID: "acct-petty", Reason: "petty cash"})
	if code, _, body := lock(t, s, &stubClients{}); code != http.StatusOK {
		t.Fatalf("an exclusion must not add a subledger requirement: %d %s", code, body)
	}
}

func TestClose_UnreadableChecklistFailsClosed(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	s.listRequirementsErr = domain.ErrStoreUnavailable
	code, _, body := lock(t, s, &stubClients{})
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "close checklist") {
		t.Fatalf("got %d %s; want 503 naming the close checklist", code, body)
	}
	if s.periods["fp-oct"].CloseStatus != "OPEN" {
		t.Fatal("closed without being able to read the checklist")
	}
}

func TestClose_InventoryIntegrityFindingsAreNamed(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	requireControl(s, "INVENTORY_QUANTITY", "")
	requireControl(s, "STOCK_COUNT", "")
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	s.controlRuns = append(s.controlRuns,
		bookRun("INVENTORY_QUANTITY", "", "EXCEPTION", at, 4, 0),
		bookRun("STOCK_COUNT", "", "EXCEPTION", at, 2, 0))
	code, resp, body := lock(t, s, &stubClients{})
	requireBlockedWith(t, code, resp, body,
		"INVENTORY_QUANTITY: 4 inventory items have a negative quantity on hand",
		"STOCK_COUNT: 2 stock-count variances are not approved")
}

// ── Bank reconciliation (ACC-14 "bank recon", BNK-05) ───────────────────────

var (
	barclays = domain.BankAccountRef{BankAccountID: "acct-barclays", LegalEntityID: "le-1", AccountName: "Barclays operating",
		MaskedAccountNumber: "****4021", AccountStatus: "ACTIVE", CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	hsbc = domain.BankAccountRef{BankAccountID: "acct-hsbc", LegalEntityID: "le-1", AccountName: "HSBC payroll",
		MaskedAccountNumber: "****7788", AccountStatus: "ACTIVE", CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
)

func certified(account, date string) domain.BankAccountReconStatus {
	d := &domain.BankReconRunDigest{RunID: account + "-" + date, StatementDate: date, Status: "CERTIFIED"}
	return domain.BankAccountReconStatus{BankAccountID: account, LatestCertified: d, LatestRun: d}
}

// The worked example: Barclays reconciled for 30 Oct; HSBC's 30 Oct run is
// stuck with £15,400 of unbooked items, its last certification is 23 Oct.
// The close must name HSBC, say why, and leave Barclays alone.
func TestClose_UnreconciledBankAccountBlocks(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	hsbcStatus := certified("acct-hsbc", "2026-10-23")
	hsbcStatus.LatestRun = &domain.BankReconRunDigest{RunID: "hsbc-30", StatementDate: "2026-10-30", Status: "EXCEPTIONS_OPEN"}
	cl := &stubClients{
		bankAccounts: []domain.BankAccountRef{barclays, hsbc},
		bankRecon:    []domain.BankAccountReconStatus{certified("acct-barclays", "2026-10-30"), hsbcStatus},
	}
	code, resp, body := lock(t, s, cl)
	requireBlockedWith(t, code, resp, body,
		"bank_reconciliation_stale: latest certified reconciliation for HSBC payroll (****7788) is for the 2026-10-23 statement",
		"on or after 2026-10-27", "the 2026-10-30 run is EXCEPTIONS_OPEN")
	for _, issue := range resp.BlockingIssues {
		if strings.Contains(issue, "Barclays") {
			t.Fatalf("Barclays is reconciled and must not be reported: %s", issue)
		}
	}
	if !cl.bankReconStart.Equal(octStart) || !cl.bankReconEnd.Equal(octEnd) {
		t.Fatalf("asked for %v..%v, want the period %v..%v", cl.bankReconStart, cl.bankReconEnd, octStart, octEnd)
	}
}

// 31 Oct 2026 is a Saturday: the last statement is Friday 30 Oct, or even
// Thursday 29th after a bank holiday. Within the 4-day cut-off, it proves the month.
func TestClose_StatementWithinCutoffProvesTheMonth(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	cl := &stubClients{bankAccounts: []domain.BankAccountRef{barclays}, bankRecon: []domain.BankAccountReconStatus{certified("acct-barclays", "2026-10-27")}}
	if code, _, body := lock(t, s, cl); code != http.StatusOK {
		t.Fatalf("a certified 27 Oct reconciliation is within the cut-off: %d %s", code, body)
	}
}

func TestClose_CutoffIsConfigurable(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	cl := &stubClients{bankAccounts: []domain.BankAccountRef{barclays}, bankRecon: []domain.BankAccountReconStatus{certified("acct-barclays", "2026-10-30")}}
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubAuthZ{}, cl, testSigningKey, zap.NewNop()).SetBankReconciliationGate(true, 0))
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-oct/lock", nil, "controller-1")
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "on or after 2026-10-31") {
		t.Fatalf("cut-off 0 requires the 31st itself: got %d %s", rr.Code, rr.Body.String())
	}
}

func TestClose_AccountWithNoReconciliationBlocks(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	cl := &stubClients{bankAccounts: []domain.BankAccountRef{barclays}}
	code, resp, body := lock(t, s, cl)
	requireBlockedWith(t, code, resp, body, "bank_reconciliation_missing: Barclays operating (****4021) has no reconciliation run for 2026-10")
}

// Scope: only operational accounts that existed by period end, and not those
// the checklist excludes as immaterial.
func TestClose_BankAccountScope(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	closed, draft, suspended, opened := barclays, barclays, hsbc, barclays
	closed.BankAccountID, closed.AccountStatus, closed.AccountName = "acct-closed", "CLOSED", "Closed"
	draft.BankAccountID, draft.AccountStatus, draft.AccountName = "acct-draft", "DRAFT", "Draft"
	suspended.BankAccountID, suspended.AccountStatus, suspended.AccountName = "acct-suspended", "SUSPENDED", "Suspended"
	opened.BankAccountID, opened.AccountName = "acct-november", "Opened in November"
	opened.CreatedAt = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	petty := barclays
	petty.BankAccountID, petty.AccountName = "acct-petty", "Petty cash"
	s.closeRequirements = append(s.closeRequirements, &domain.CloseRequirement{RequirementID: "ex-1", TenantID: testTenantID,
		LegalEntityID: "le-1", Kind: domain.CloseRequirementBankAccountExclusion, BankAccountID: "acct-petty", Reason: "float under £200"})

	cl := &stubClients{bankAccounts: []domain.BankAccountRef{closed, draft, suspended, opened, petty}}
	code, resp, _ := lock(t, s, cl)
	if code != http.StatusUnprocessableEntity || len(resp.BlockingIssues) != 1 || !strings.Contains(resp.BlockingIssues[0], "Suspended") {
		t.Fatalf("only the suspended account (it still holds money) is in scope; got %d %v", code, resp.BlockingIssues)
	}
}

func TestClose_BankGateFailsClosed(t *testing.T) {
	for name, cl := range map[string]*stubClients{
		"treasury down":            {bankAccountsErr: domain.ErrTreasuryUnavailable},
		"bank reconciliation down": {bankAccounts: []domain.BankAccountRef{barclays}, bankReconErr: domain.ErrBankReconciliationUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			s := octoberStore()
			bothMatched(s)
			code, _, body := lock(t, s, cl)
			if code != http.StatusServiceUnavailable {
				t.Fatalf("got %d %s, want 503", code, body)
			}
			if s.periods["fp-oct"].CloseStatus != "OPEN" {
				t.Fatal("closed without being able to check cash")
			}
		})
	}
}

// BANK_RECON_GATE_MODE=off: neither service is asked.
func TestClose_BankGateOffAsksNobody(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	cl := &stubClients{bankAccountsErr: domain.ErrTreasuryUnavailable}
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubAuthZ{}, cl, testSigningKey, zap.NewNop()).SetBankReconciliationGate(false, 4))
	if rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-oct/lock", nil, "controller-1"); rr.Code != http.StatusOK || cl.bankCalls != 0 {
		t.Fatalf("gate off: got %d with %d bank calls", rr.Code, cl.bankCalls)
	}
}

// ── Evidence: what the close relied on (ZS-CONTROL-001 §22) ─────────────────

// A successful close pins the exact runs that proved it — controls, bank
// reconciliations, waived accounts, checklist and gate settings — and signs
// that record so it cannot be altered unnoticed.
func TestClose_EvidencePinsWhatTheCloseReliedOn(t *testing.T) {
	s := octoberStore()
	bothMatched(s)
	requireControl(s, "ASSETS", "STATUTORY")
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	assetsRun := bookRun("ASSETS", "STATUTORY", "MATCHED", at, 90000, 90000)
	s.controlRuns = append(s.controlRuns,
		// A superseded EXCEPTION earlier: the evidence must name the run that
		// actually satisfied the control, not merely some run.
		bookRun("ASSETS", "STATUTORY", "EXCEPTION", at.Add(-time.Hour), 1, 2), assetsRun)
	petty := barclays
	petty.BankAccountID, petty.AccountName = "acct-petty", "Petty cash"
	s.closeRequirements = append(s.closeRequirements, &domain.CloseRequirement{RequirementID: "ex-petty", TenantID: testTenantID,
		LegalEntityID: "le-1", Kind: domain.CloseRequirementBankAccountExclusion, BankAccountID: "acct-petty", Reason: "float under £200"})
	cl := &stubClients{bankAccounts: []domain.BankAccountRef{barclays, petty},
		bankRecon: []domain.BankAccountReconStatus{certified("acct-barclays", "2026-10-30")}}

	if code, _, body := lock(t, s, cl); code != http.StatusOK {
		t.Fatalf("expected the close: %d %s", code, body)
	}
	if len(s.evidence) != 1 {
		t.Fatalf("evidence rows: %d", len(s.evidence))
	}
	ev := s.evidence[0]

	// Signed: hash = sha256(manifest bytes), signature = HMAC(key, hash).
	sum := sha256.Sum256([]byte(ev.RelianceManifest))
	if ev.RelianceHash != hex.EncodeToString(sum[:]) {
		t.Fatal("reliance_hash does not match the stored manifest")
	}
	mac := hmac.New(sha256.New, testSigningKey)
	mac.Write(sum[:])
	if ev.RelianceSignature != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("reliance_signature does not verify")
	}

	var rel domain.CloseReliance
	if err := json.Unmarshal([]byte(ev.RelianceManifest), &rel); err != nil {
		t.Fatal(err)
	}
	if rel.SubledgerControlGate != "enforce" || rel.BankReconciliationGate != "enforce" || rel.BankReconciliationCutoffDays != 4 {
		t.Fatalf("gate settings not recorded: %+v", rel)
	}
	runs := map[string]string{}
	for _, c := range rel.SubledgerControls {
		runs[c.Subledger+"@"+c.BookID] = c.ControlRunID
	}
	if runs["ASSETS@STATUTORY"] != assetsRun.ControlRunID || runs["AR@"] == "" || runs["AP@"] == "" || len(runs) != 3 {
		t.Fatalf("control runs pinned %v; want AR, AP and the MATCHED ASSETS@STATUTORY run %s", runs, assetsRun.ControlRunID)
	}
	if len(rel.BankReconciliations) != 1 || rel.BankReconciliations[0].RunID != "acct-barclays-2026-10-30" {
		t.Fatalf("bank reconciliations pinned %+v", rel.BankReconciliations)
	}
	if len(rel.ExcludedBankAccounts) != 1 || rel.ExcludedBankAccounts[0].RequirementID != "ex-petty" ||
		rel.ExcludedBankAccounts[0].Reason != "float under £200" {
		t.Fatalf("waived accounts pinned %+v", rel.ExcludedBankAccounts)
	}
	if len(rel.ChecklistRequirementIDs) != 2 {
		t.Fatalf("checklist pinned %v; want both active items", rel.ChecklistRequirementIDs)
	}

	// Readable back through the API, with the exact signed text.
	rr := doReq(newGatedRouter(s, cl), http.MethodGet, "/v1/close/periods/fp-oct/evidence", nil, "auditor-1")
	var views []domain.CloseEvidenceView
	if err := json.Unmarshal(rr.Body.Bytes(), &views); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("evidence endpoint: %d %s", rr.Code, rr.Body.String())
	}
	if len(views) != 1 || views[0].RelianceManifestText != ev.RelianceManifest || views[0].Reliance == nil ||
		views[0].Reliance.SubledgerControls[0].ControlRunID == "" {
		t.Fatalf("evidence view %+v", views)
	}
}

// A gate switched off is part of the record, not an absence in it.
func TestClose_EvidenceRecordsAGateThatWasOff(t *testing.T) {
	s := octoberStore()
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{}, testSigningKey, zap.NewNop()).
		SetSubledgerControlGateEnforced(false).SetBankReconciliationGate(false, 4))
	if rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-oct/lock", nil, "controller-1"); rr.Code != http.StatusOK {
		t.Fatalf("lock: %d %s", rr.Code, rr.Body.String())
	}
	var rel domain.CloseReliance
	_ = json.Unmarshal([]byte(s.evidence[0].RelianceManifest), &rel)
	if rel.SubledgerControlGate != "off" || rel.BankReconciliationGate != "off" || len(rel.SubledgerControls) != 0 {
		t.Fatalf("a close with gates off must say so: %+v", rel)
	}
}
