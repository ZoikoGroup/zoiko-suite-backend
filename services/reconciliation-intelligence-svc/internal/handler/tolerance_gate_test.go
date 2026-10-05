package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/reconciliation-intelligence-svc/internal/authz"
	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	"zoiko.io/reconciliation-intelligence-svc/internal/events"
	"zoiko.io/reconciliation-intelligence-svc/internal/financialcontrol"
	"zoiko.io/reconciliation-intelligence-svc/internal/handler"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

// TestAnalyzeReconciliation_NoGovernedTolerance_RefusesRatherThanGuesses
// is the end-to-end proof of ZS-SVC-Z-001 INV-09's fail-closed
// behavior: when financial-control-svc has no TolerancePolicy
// configured for this legal entity, AnalyzeReconciliation must refuse
// the whole request — never silently fall back to a guessed default,
// and never create a job.
func TestAnalyzeReconciliation_NoGovernedTolerance_RefusesRatherThanGuesses(t *testing.T) {
	logger := zap.NewNop()
	memStore := store.NewMemoryStore()
	publisher := events.NewPublisher([]string{"localhost:9092"}, "zoiko.reconciliation-intelligence.events", logger)
	authzSrv := newGrantingAuthzServer(t)
	authzClient := authz.NewClient(authzSrv.URL, logger)

	// No TolerancePolicy configured for anyone — the real shape an
	// operator hasn't gotten to yet.
	emptyToleranceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	}))
	t.Cleanup(emptyToleranceSrv.Close)
	financialControlClient := financialcontrol.NewClient(emptyToleranceSrv.URL, logger)

	h := handler.NewHandler(memStore, publisher, authzClient, financialControlClient, logger)
	router := handler.NewRouter(h)

	body, _ := json.Marshal(domain.AnalyzeReconciliationRequest{
		LegalEntityID: "le-unconfigured", JobName: "job", SourceSystemA: domain.SourceGeneralLedger,
		SourceSystemB: domain.SourceBankStatement,
		TransactionsA: []domain.TransactionItem{{RefID: "r1", Amount: 100.00}},
		TransactionsB: []domain.TransactionItem{{RefID: "r1", Amount: 132.00}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-no-policy")
	req.Header.Set("X-Principal-Id", "principal-01")
	req = withEnvelope(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("expected 424 Failed Dependency when no tolerance policy is configured, got %d: %s", rec.Code, rec.Body.String())
	}

	jobs, err := memStore.ListJobs(req.Context(), "tenant-no-policy", "le-unconfigured", "", "")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused analyze call must not create a job, found %d", len(jobs))
	}
}

// TestAnalyzeReconciliation_FinancialControlUnreachable_FailsClosed
// covers the transport-failure case distinctly from "no policy yet" —
// both must refuse, but for different reasons (503 vs 424).
func TestAnalyzeReconciliation_FinancialControlUnreachable_FailsClosed(t *testing.T) {
	logger := zap.NewNop()
	memStore := store.NewMemoryStore()
	publisher := events.NewPublisher([]string{"localhost:9092"}, "zoiko.reconciliation-intelligence.events", logger)
	authzSrv := newGrantingAuthzServer(t)
	authzClient := authz.NewClient(authzSrv.URL, logger)
	financialControlClient := financialcontrol.NewClient("http://127.0.0.1:1", logger) // nothing listens here

	h := handler.NewHandler(memStore, publisher, authzClient, financialControlClient, logger)
	router := handler.NewRouter(h)

	body, _ := json.Marshal(domain.AnalyzeReconciliationRequest{
		LegalEntityID: "le-1", JobName: "job", SourceSystemA: domain.SourceGeneralLedger,
		SourceSystemB: domain.SourceBankStatement,
		TransactionsA: []domain.TransactionItem{{RefID: "r1", Amount: 100.00}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-1")
	req.Header.Set("X-Principal-Id", "principal-01")
	req = withEnvelope(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when financial-control-svc is unreachable, got %d: %s", rec.Code, rec.Body.String())
	}
}
