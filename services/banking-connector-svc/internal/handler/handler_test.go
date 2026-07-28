package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/events"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

func setupTestRouter() (chi.Router, *events.MockPublisher) {
	st := store.NewMemoryStore()
	pub := events.NewMockPublisher()
	az := authz.NewClient("http://localhost:8081")
	logger := zap.NewNop()

	h := New(st, pub, az, logger)

	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	return r, pub
}

func TestBankingFlow(t *testing.T) {
	r, _ := setupTestRouter()

	connReq := domain.CreateConnectionRequest{
		LegalEntityID: "le-101",
		BankName:      "JPMorgan Chase",
		BIC:           "CHASUS33XXX",
		AccountNumber: "GB89CHAS1234567890",
		Currency:      "USD",
	}
	connBytes, _ := json.Marshal(connReq)

	req := httptest.NewRequest("POST", "/v1/banking/connections", bytes.NewReader(connBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "tenant-test")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var conn domain.BankConnection
	if err := json.Unmarshal(w.Body.Bytes(), &conn); err != nil {
		t.Fatalf("failed to unmarshal connection: %v", err)
	}

	stmtReq := domain.IngestStatementRequest{
		ConnectionID:    conn.ConnectionID,
		StatementFormat: domain.FormatISO20022,
		StatementDate:   time.Now().UTC(),
		OpeningBalance:  100000.0,
		ClosingBalance:  125000.0,
		RawContent:      "<camt.053>xml content</camt.053>",
	}
	stmtBytes, _ := json.Marshal(stmtReq)

	stmtHTTP := httptest.NewRequest("POST", "/v1/banking/statements", bytes.NewReader(stmtBytes))
	stmtHTTP.Header.Set("Content-Type", "application/json")
	stmtHTTP.Header.Set("X-Tenant-ID", "tenant-test")

	stmtW := httptest.NewRecorder()
	r.ServeHTTP(stmtW, stmtHTTP)

	if stmtW.Code != http.StatusCreated {
		t.Fatalf("expected status 201 on statement ingest, got %d: %s", stmtW.Code, stmtW.Body.String())
	}
}
