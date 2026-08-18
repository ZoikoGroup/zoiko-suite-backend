package clients_test

// Tests for the trial balance, driven against a stub general-ledger-svc.
//
// A real HTTP server rather than an interface stub, because the two defects
// being guarded against are both in the REQUEST this service makes — which
// statuses it asks for, and whether it notices a full page — and an interface
// stub would have nothing to observe.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/clients"
	"zoiko.io/financial-close-svc/internal/domain"
)

type journal struct {
	JournalID    string `json:"journal_id"`
	Status       string `json:"status"`
	FiscalPeriod string `json:"fiscal_period"`
}

type line struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
}

// fakeLedger serves /v1/journals (filtered by the status query parameter, as
// the real service does) and /v1/journals/{id}.
type fakeLedger struct {
	journals map[string]journal
	lines    map[string][]line
	// statusesAsked records every status this service requested, so a test can
	// assert on what was actually asked for.
	statusesAsked []string
}

func (f *fakeLedger) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/journals", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/journals" {
			f.serveOne(w, r)
			return
		}
		status := r.URL.Query().Get("status")
		f.statusesAsked = append(f.statusesAsked, status)

		out := []journal{}
		for _, j := range f.journals {
			if status == "" || j.Status == status {
				out = append(out, j)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/v1/journals/", f.serveOne)
	return mux
}

func (f *fakeLedger) serveOne(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/v1/journals/"):]
	j, ok := f.journals[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"journal_id": j.JournalID,
		"lines":      f.lines[id],
	})
}

func newClients(t *testing.T, ledgerURL string) *clients.Clients {
	t.Helper()
	return clients.New("http://authz.invalid", ledgerURL, "http://ap.invalid", "http://ar.invalid", "http://vault.invalid", zap.NewNop())
}

// TestCompileTrialBalance_IncludesReversedOriginals is the regression test for
// an accounting defect that mis-stated the trial balance by exactly double the
// value of every reversal.
//
// A reversal in general-ledger-svc erases nothing: the original moves to
// REVERSED and keeps its lines, and a NEW journal is posted FINALIZED carrying
// the exact inverse. Both are real postings and together they net to zero.
// Compiling from FINALIZED alone dropped the original while keeping its
// inverse, so a reversed 100.00 debit read as a 100.00 CREDIT — and that
// balance is what gets hashed, signed and locked as the period's evidence.
func TestCompileTrialBalance_IncludesReversedOriginals(t *testing.T) {
	f := &fakeLedger{
		journals: map[string]journal{
			// An ordinary posting that stands.
			"j-standing": {JournalID: "j-standing", Status: "FINALIZED", FiscalPeriod: "2026-01"},
			// A posting that was reversed...
			"j-original": {JournalID: "j-original", Status: "REVERSED", FiscalPeriod: "2026-01"},
			// ...and the journal that reversed it.
			"j-reversal": {JournalID: "j-reversal", Status: "FINALIZED", FiscalPeriod: "2026-01"},
		},
		lines: map[string][]line{
			"j-standing": {
				{AccountCode: "1000-Cash", DebitAmount: 250},
				{AccountCode: "4000-Rev", CreditAmount: 250},
			},
			"j-original": {
				{AccountCode: "1000-Cash", DebitAmount: 100},
				{AccountCode: "4000-Rev", CreditAmount: 100},
			},
			"j-reversal": {
				{AccountCode: "1000-Cash", CreditAmount: 100},
				{AccountCode: "4000-Rev", DebitAmount: 100},
			},
		},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	balances, err := newClients(t, srv.URL).CompileTrialBalance(t.Context(), "tenant-1", "le-1", "2026-01")
	if err != nil {
		t.Fatalf("CompileTrialBalance: %v", err)
	}

	// The reversed pair nets to zero, so only the standing journal remains.
	if got := balances["1000-Cash"]; got != 250 {
		t.Fatalf("1000-Cash = %v, want 250. The reversed original and its inverse must cancel; "+
			"150 means the original was dropped and only its inverse counted", got)
	}
	if got := balances["4000-Rev"]; got != -250 {
		t.Fatalf("4000-Rev = %v, want -250", got)
	}

	// And the balance must actually balance.
	var total float64
	for _, v := range balances {
		total += v
	}
	if total != 0 {
		t.Fatalf("the trial balance does not balance: net %v", total)
	}

	// It must have asked for both posted states by name.
	asked := map[string]bool{}
	for _, s := range f.statusesAsked {
		asked[s] = true
	}
	if !asked["FINALIZED"] || !asked["REVERSED"] {
		t.Fatalf("expected both FINALIZED and REVERSED to be requested, asked for: %v", f.statusesAsked)
	}
}

// A full page means there may be journals this service never saw. The trial
// balance it would compile is wrong, not merely short — and it is about to be
// hashed, signed and locked as permanent evidence, so the only honest answer is
// to refuse. A close that fails loudly can be retried; a close that silently
// omitted journals cannot be detected afterwards.
func TestCompileTrialBalance_FullPage_RefusesRatherThanSignPartialBalance(t *testing.T) {
	f := &fakeLedger{journals: map[string]journal{}, lines: map[string][]line{}}
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("j-%04d", i)
		f.journals[id] = journal{JournalID: id, Status: "FINALIZED", FiscalPeriod: "2026-01"}
		f.lines[id] = []line{{AccountCode: "1000-Cash", DebitAmount: 1}}
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	_, err := newClients(t, srv.URL).CompileTrialBalance(t.Context(), "tenant-1", "le-1", "2026-01")
	if !errors.Is(err, domain.ErrLedgerPageTruncated) {
		t.Fatalf("expected ErrLedgerPageTruncated, got %v", err)
	}
}

// The page size has to be asked for explicitly: the ledger's own default is 200,
// so a request that named no limit silently compiled the trial balance from the
// most recent 200 journals of a period and reported nothing unusual.
func TestCompileTrialBalance_AsksForAnExplicitLimit(t *testing.T) {
	var gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/journals" {
			gotLimit = r.URL.Query().Get("limit")
		}
		_ = json.NewEncoder(w).Encode([]journal{})
	}))
	defer srv.Close()

	if _, err := newClients(t, srv.URL).CompileTrialBalance(t.Context(), "tenant-1", "le-1", "2026-01"); err != nil {
		t.Fatalf("CompileTrialBalance: %v", err)
	}
	if gotLimit == "" {
		t.Fatal("no limit was sent, so the ledger's 200-row default silently bounded the trial balance")
	}
}

// The unposted count is what decides whether a period is blocked, so
// under-counting it reads as "ready to close".
func TestGetUnpostedJournalsCount_CountsBothDraftStates(t *testing.T) {
	f := &fakeLedger{
		journals: map[string]journal{
			"j-p1": {JournalID: "j-p1", Status: "PENDING", FiscalPeriod: "2026-01"},
			"j-p2": {JournalID: "j-p2", Status: "PENDING", FiscalPeriod: "2026-01"},
			"j-v1": {JournalID: "j-v1", Status: "VALIDATED", FiscalPeriod: "2026-01"},
			"j-f1": {JournalID: "j-f1", Status: "FINALIZED", FiscalPeriod: "2026-01"},
			"j-r1": {JournalID: "j-r1", Status: "REVERSED", FiscalPeriod: "2026-01"},
		},
		lines: map[string][]line{},
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	count, err := newClients(t, srv.URL).GetUnpostedJournalsCount(t.Context(), "tenant-1", "le-1", "2026-01")
	if err != nil {
		t.Fatalf("GetUnpostedJournalsCount: %v", err)
	}
	// Two PENDING and one VALIDATED. Posted and reversed journals have reached
	// the books and do not block.
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
}

// The period filter has to reach the ledger, or readiness is answered about the
// wrong window entirely.
func TestGetUnpostedJournalsCount_SendsThePeriod(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_ = json.NewEncoder(w).Encode([]journal{})
	}))
	defer srv.Close()

	if _, err := newClients(t, srv.URL).GetUnpostedJournalsCount(t.Context(), "tenant-1", "le-1", "2026-07"); err != nil {
		t.Fatalf("GetUnpostedJournalsCount: %v", err)
	}
	if got.Get("fiscal_period") != "2026-07" {
		t.Fatalf("fiscal_period = %q, want 2026-07", got.Get("fiscal_period"))
	}
	if got.Get("legal_entity_id") != "le-1" {
		t.Fatalf("legal_entity_id = %q, want le-1", got.Get("legal_entity_id"))
	}
}
