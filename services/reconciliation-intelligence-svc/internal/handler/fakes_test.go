package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/reconciliation-intelligence-svc/internal/authz"
	"zoiko.io/reconciliation-intelligence-svc/internal/bankrec"
	"zoiko.io/reconciliation-intelligence-svc/internal/financialcontrol"
	"zoiko.io/reconciliation-intelligence-svc/internal/handler"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

// fakeRun is one bank-reconciliation-svc run and its UNMATCHED lines.
type fakeRun struct {
	LegalEntityID string
	Status        string // default RUNNING
	SupersededBy  string
	// Lines maps statement_line_id to the amount's raw JSON (a bare number,
	// as bank-reconciliation-svc sends it, or a quoted string).
	Lines [][2]string
	// Refs overrides a line's bank_reference (default "REF-" + its id).
	Refs map[string]string
}

// Standard fixtures. Each run's bank account is "ba-" + its id.
const (
	runLE3003 = "11111111-1111-4111-8111-111111111111" // LE-3003: 75.00 and 5.00
	runSmall  = "22222222-2222-4222-8222-222222222222" // le-1: one 5.00 line
	runLarge  = "33333333-3333-4333-8333-333333333333" // LE-1: one 75 line
)

func standardRuns() map[string]fakeRun {
	return map[string]fakeRun{
		runLE3003: {LegalEntityID: "LE-3003", Lines: [][2]string{{"sl-3003-a", "75.00"}, {"sl-3003-b", "5.00"}}},
		runSmall:  {LegalEntityID: "le-1", Lines: [][2]string{{"sl-small", "5.00"}}},
		runLarge:  {LegalEntityID: "LE-1", Lines: [][2]string{{"sl-large", "75"}}},
	}
}

// newFakeBankRecServer stands in for bank-reconciliation-svc's two read
// routes. Any non-GET fails the test: this service must never write to
// bank-reconciliation-svc (no propose/confirm/reject-match, nothing).
func newFakeBankRecServer(t *testing.T, runs map[string]fakeRun) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("reconciliation-intelligence-svc sent %s %s to bank-reconciliation-svc; it is read-only", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/reconciliation-runs/"); ok {
			run, found := runs[id]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"run_not_found"}`))
				return
			}
			status := run.Status
			if status == "" {
				status = "RUNNING"
			}
			body := map[string]any{
				"run_id": id, "tenant_id": r.Header.Get("X-Tenant-Id"), "legal_entity_id": run.LegalEntityID,
				"bank_account_id": "ba-" + id, "statement_date": "2026-07-01", "status": status,
			}
			if run.SupersededBy != "" {
				body["superseded_by_run_id"] = run.SupersededBy
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		if r.URL.Path == "/v1/statement-lines" {
			id := strings.TrimPrefix(r.URL.Query().Get("bank_account_id"), "ba-")
			run := runs[id]
			parts := make([]string, 0, len(run.Lines))
			for _, l := range run.Lines {
				ref, ok := run.Refs[l[0]]
				if !ok {
					ref = "REF-" + l[0]
				}
				parts = append(parts, fmt.Sprintf(
					`{"statement_line_id":%q,"legal_entity_id":%q,"bank_account_id":%q,"statement_date":"2026-07-01T00:00:00Z","amount":%s,"currency_code":"USD","bank_reference":%q,"status":"UNMATCHED"}`,
					l[0], run.LegalEntityID, "ba-"+id, l[1], ref))
			}
			_, _ = w.Write([]byte("[" + strings.Join(parts, ",") + "]"))
			return
		}
		t.Errorf("unexpected bank-reconciliation-svc request %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// authzDecision is one call authorization-svc received.
type authzDecision struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
}

// recordingAuthz stands in for authorization-svc. It records every decision
// asked for, and denies any (action, legal entity) pair in deny. Like the
// real service it always answers 200 with the decision in the body.
type recordingAuthz struct {
	mu    sync.Mutex
	calls []authzDecision
	deny  map[[2]string]bool
}

func (a *recordingAuthz) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var d authzDecision
		_ = json.NewDecoder(r.Body).Decode(&d)
		a.mu.Lock()
		a.calls = append(a.calls, d)
		denied := a.deny[[2]string{d.ActionType, d.LegalEntityID}]
		a.mu.Unlock()
		outcome := "GRANTED"
		if denied {
			outcome = "DENIED"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": outcome})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (a *recordingAuthz) asked(action string) []authzDecision {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []authzDecision
	for _, c := range a.calls {
		if c.ActionType == action {
			out = append(out, c)
		}
	}
	return out
}

// newFixedToleranceServer stands in for financial-control-svc: every
// TolerancePolicy lookup answers an absolute_tolerance of "10", the
// small-amount write-off tolerance the tests' 5.00 / 75 lines sit either
// side of.
func newFixedToleranceServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{"tolerance_version": 1, "absolute_tolerance": "10"}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deps selects what newTestRouter wires in; zero values mean the standard
// fakes.
type deps struct {
	runs                map[string]fakeRun
	authz               *recordingAuthz
	authzURL            string // overrides authz, e.g. an unreachable address
	store               *store.MemoryStore
	financialControlURL string
	bankRecURL          string
}

func newTestRouter(t *testing.T, d deps) http.Handler {
	t.Helper()
	logger := zap.NewNop()
	if d.runs == nil {
		d.runs = standardRuns()
	}
	if d.authz == nil {
		d.authz = &recordingAuthz{}
	}
	if d.store == nil {
		d.store = store.NewMemoryStore()
	}
	if d.financialControlURL == "" {
		d.financialControlURL = newFixedToleranceServer(t).URL
	}
	if d.bankRecURL == "" {
		d.bankRecURL = newFakeBankRecServer(t, d.runs).URL
	}
	if d.authzURL == "" {
		d.authzURL = d.authz.server(t).URL
	}
	h := handler.NewHandler(d.store,
		authz.NewClient(d.authzURL, logger),
		financialcontrol.NewClient(d.financialControlURL, logger),
		bankrec.NewClient(d.bankRecURL, logger),
		logger)
	return handler.NewRouter(h)
}

func setupTestRouter(t *testing.T) http.Handler {
	return newTestRouter(t, deps{})
}

func analyzeBody(legalEntityID, runID string) []byte {
	b, _ := json.Marshal(map[string]string{
		"legal_entity_id": legalEntityID, "job_name": "job", "bank_reconciliation_run_id": runID,
	})
	return b
}
