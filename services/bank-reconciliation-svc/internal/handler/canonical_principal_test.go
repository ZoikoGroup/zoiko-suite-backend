package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/banking"
	"zoiko.io/bank-reconciliation-svc/internal/domain"
	"zoiko.io/bank-reconciliation-svc/internal/envelope"
	"zoiko.io/bank-reconciliation-svc/internal/handler"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
)

// connectorContract stands in for banking-connector-svc's
// GET /v1/banking/canonical-transactions/{id} with the one rule that broke
// the integration: like the real handler (BNK0304Handler.GetCanonicalTransaction
// → requirePrincipal) it answers 401 to a request with no X-Principal-Id.
// It records the headers each call carried so a test can assert WHOSE
// principal was forwarded, not merely that one was. The same contract is
// exercised against the real connector process in connector_e2e_test.go.
type connectorContract struct {
	mu   sync.Mutex
	seen []http.Header
	txn  banking.CanonicalTransaction
}

func (c *connectorContract) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.seen = append(c.seen, r.Header.Clone())
	c.mu.Unlock()
	if r.Header.Get("X-Tenant-Id") == "" || r.Header.Get("X-Principal-Id") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(c.txn)
}

func (c *connectorContract) calls() []http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]http.Header(nil), c.seen...)
}

// newRouterWithBanking is newRouter with a real banking.HTTPClient and the
// §4 envelope middleware in observe mode (so it never refuses, but does put
// the envelope on the context the way production does).
func newRouterWithBanking(s *stubStore, bankingClient banking.Client) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(envelope.MiddlewareWithMode(envelope.ServicePolicy(), envelope.ModeObserve, nil))
	h := handler.New(s, &stubAuthZ{}, &stubLedger{}, bankingClient, &stubClose{}, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func canonicalFixture(t *testing.T) (*stubStore, *connectorContract, chi.Router) {
	t.Helper()
	s := newStubStore()
	s.lines["sl-1"] = &domain.StatementLine{
		StatementLineID: "sl-1", TenantID: testTenant, LegalEntityID: "e1",
		Amount: 125.50, CurrencyCode: "USD", Status: domain.StatementLineStatusUnmatched,
	}
	conn := &connectorContract{txn: banking.CanonicalTransaction{
		TransactionID: "txn-1", TenantID: testTenant, Amount: 125.50, Currency: "USD", Status: "NORMALIZED",
	}}
	srv := httptest.NewServer(conn)
	t.Cleanup(srv.Close)
	return s, conn, newRouterWithBanking(s, banking.NewHTTPClient(srv.URL))
}

// TestCanonicalMatch_ForwardsCallerPrincipalToConnector is the C2 regression:
// every entry point that verifies a canonical transaction must forward the
// principal it already resolved. Before the fix each of these answered 503
// banking_connector_unavailable (the connector's 401, remapped), and the
// connector never saw a principal at all.
func TestCanonicalMatch_ForwardsCallerPrincipalToConnector(t *testing.T) {
	cases := []struct {
		name      string
		principal string
		setup     func(s *stubStore)
		path      string
		body      any
	}{
		{
			name: "MatchStatementLine", principal: "p-matcher",
			path: "/v1/statement-lines/sl-1/match",
			body: domain.MatchStatementLineRequest{TransactionID: "txn-1"},
		},
		{
			name: "ProposeMatch", principal: "p-maker",
			path: "/v1/statement-lines/sl-1/propose-match",
			body: domain.MatchStatementLineRequest{TransactionID: "txn-1"},
		},
		{
			// The checker's own principal, not the proposer's recorded one —
			// the re-verification is a read made on the checker's behalf.
			name: "ConfirmMatch", principal: "p-checker",
			setup: func(s *stubStore) {
				l := s.lines["sl-1"]
				txn, maker := "txn-1", "p-maker"
				l.Status = domain.StatementLineStatusPendingConfirmation
				l.ProposedTransactionID = &txn
				l.ProposedByPrincipalID = &maker
			},
			path: "/v1/statement-lines/sl-1/confirm-match",
		},
		{
			name: "RunAutomaticMatching", principal: "p-auto",
			setup: func(s *stubStore) {
				pop := "pop-1"
				s.runPopulationID = &pop
				s.unmatchedInPopulation = []domain.StatementLine{*s.lines["sl-1"]}
			},
			path: "/v1/reconciliation-runs/run-1/auto-match",
			body: domain.RunAutomaticMatchingRequest{Candidates: []domain.AutoMatchCandidate{
				{StatementLineID: "sl-1", TransactionID: "txn-1"},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, conn, r := canonicalFixture(t)
			if tc.setup != nil {
				tc.setup(s)
			}
			rec := doRequest(r, http.MethodPost, tc.path, tc.body, tc.principal)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			if tc.name == "RunAutomaticMatching" {
				var resp domain.RunAutomaticMatchingResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if len(resp.Results) != 1 || !resp.Results[0].Matched {
					t.Fatalf("expected the candidate to match, got %+v", resp.Results)
				}
			}

			calls := conn.calls()
			if len(calls) != 1 {
				t.Fatalf("expected exactly one connector call, got %d", len(calls))
			}
			got := calls[0]
			if p := got.Get("X-Principal-Id"); p != tc.principal {
				t.Errorf("X-Principal-Id = %q, want the caller's %q", p, tc.principal)
			}
			if tn := got.Get("X-Tenant-Id"); tn != testTenant {
				t.Errorf("X-Tenant-Id = %q, want %q", tn, testTenant)
			}
			if ch := got.Get("X-Source-Channel"); ch != "system" {
				t.Errorf("X-Source-Channel = %q, want system", ch)
			}
		})
	}
}

// TestCanonicalMatch_PropagatesCorrelationToConnector: the connector's read
// carries the inbound X-Correlation-ID, so it is traceable to the match that
// caused it (§4 correlation_id is mandatory under strict enforcement).
func TestCanonicalMatch_PropagatesCorrelationToConnector(t *testing.T) {
	_, conn, r := canonicalFixture(t)

	body, _ := json.Marshal(domain.MatchStatementLineRequest{TransactionID: "txn-1"})
	req := httptest.NewRequest(http.MethodPost, "/v1/statement-lines/sl-1/match", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Principal-Id", "p-matcher")
	req.Header.Set("X-Correlation-ID", "corr-c2")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	calls := conn.calls()
	if len(calls) != 1 || calls[0].Get("X-Correlation-ID") != "corr-c2" {
		t.Fatalf("expected the connector call to carry X-Correlation-ID corr-c2, got %v", calls)
	}
}
