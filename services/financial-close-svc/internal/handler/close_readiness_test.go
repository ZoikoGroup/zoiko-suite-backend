package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"zoiko.io/financial-close-svc/internal/domain"
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
