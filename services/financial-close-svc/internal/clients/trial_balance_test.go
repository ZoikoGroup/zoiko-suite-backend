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

// TestCompileTrialBalance_ParsesLinesIntoMap proves the client correctly
// converts general-ledger-svc's own trial-balance response into the
// map[string]float64 the rest of this service expects. general-ledger-svc
// now owns the FINALIZED/REVERSED netting rule itself (see master-
// register-findings-2026-08-27.md §3.32) — this client's only remaining
// job is to ask its real endpoint and parse the answer, not re-derive
// ledger semantics a second time.
func TestCompileTrialBalance_ParsesLinesIntoMap(t *testing.T) {
	var gotBody map[string]string
	var gotTenant, gotPrincipal string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/trial-balance/compile" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotTenant = r.Header.Get("X-Tenant-Id")
		gotPrincipal = r.Header.Get("X-Principal-Id")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"trial_balance_snapshot_id": "tb-1",
			"ledger_watermark":          42,
			"lines": []map[string]any{
				{"account_code": "1000-Cash", "net_balance": 250},
				{"account_code": "4000-Rev", "net_balance": -250},
			},
		})
	}))
	defer srv.Close()

	balances, err := newClients(t, srv.URL).CompileTrialBalance(t.Context(), "tenant-1", "le-1", "2026-01", "principal-1")
	if err != nil {
		t.Fatalf("CompileTrialBalance: %v", err)
	}
	if got := balances["1000-Cash"]; got != 250 {
		t.Fatalf("1000-Cash = %v, want 250", got)
	}
	if got := balances["4000-Rev"]; got != -250 {
		t.Fatalf("4000-Rev = %v, want -250", got)
	}
	if gotTenant != "tenant-1" || gotPrincipal != "principal-1" {
		t.Fatalf("expected tenant/principal forwarded, got tenant=%q principal=%q", gotTenant, gotPrincipal)
	}
	if gotBody["legal_entity_id"] != "le-1" || gotBody["fiscal_period"] != "2026-01" {
		t.Fatalf("expected legal_entity_id/fiscal_period sent in the request body, got %+v", gotBody)
	}
}

// TestCompileTrialBalance_ServerError_ReturnsErrGLServiceUnavailable proves
// a non-201 response from general-ledger-svc is surfaced as a real error,
// never parsed as an empty-but-successful trial balance.
func TestCompileTrialBalance_ServerError_ReturnsErrGLServiceUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := newClients(t, srv.URL).CompileTrialBalance(t.Context(), "tenant-1", "le-1", "2026-01", "principal-1")
	if !errors.Is(err, domain.ErrGLServiceUnavailable) {
		t.Fatalf("expected ErrGLServiceUnavailable, got %v", err)
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
