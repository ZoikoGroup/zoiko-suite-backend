package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/envelope"
	"zoiko.io/banking-connector-svc/internal/handler"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

// TestCanonicalTransactionRead_CrossServiceContract pins the contract
// bank-reconciliation-svc's banking client depends on (SPEC_DEVIATIONS C2),
// against the real handler, the real middleware stack from cmd/server and a
// real Postgres row under RLS:
//
//   - no X-Principal-Id → 401, which the reconciliation client used to hit
//     on every canonical match because it never sent one;
//   - exactly the header set that client now sends → 200 with the row,
//     under the default write-strict envelope mode AND under strict, the
//     mode a deployment moves to once every caller sends a full envelope.
//
// The strict-mode refusal of a bare tenant+principal read is asserted too:
// it is why the client propagates correlation id and source channel rather
// than only the principal.
func TestCanonicalTransactionRead_CrossServiceContract(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool, store.WithEventRegion("uk"))

	const tenant = "tenant-c2-contract"
	ctx := middleware.WithTenant(context.Background(), tenant)
	connID := seedConnection(t, ctx, s, tenant)
	if _, err := s.CreateTransactionMapping(ctx, domain.CreateTransactionMappingParams{
		TenantID: tenant, BankCode: "ACH-CREDIT", Category: "ACH_CREDIT", ActorPrincipalID: "ops-admin",
	}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	ingested, err := s.IngestStatement(ctx, tenant, domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-c2-contract",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 125.5, Currency: "USD", Description: "receipt", BankCode: "ACH-CREDIT"}},
	}, "auditor-1", "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	norm, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: tenant, StatementLineID: ingested.Lines[0].LineID, Counterparty: "Customer Ltd", ActorPrincipalID: "ops-1",
	})
	if err != nil || norm.Transaction == nil {
		t.Fatalf("normalize: %v (%+v)", err, norm)
	}
	txnID := norm.Transaction.TransactionID

	// The cmd/server stack, minus logging/recovery that change nothing here.
	// authorization-svc is never consulted by this read; the client points
	// nowhere so an unexpected call would fail rather than pass.
	router := func(mode envelope.Mode) chi.Router {
		r := chi.NewRouter()
		r.Use(chimiddleware.RequestID)
		r.Use(middleware.TenantContext)
		r.Use(envelope.MiddlewareWithMode(envelope.ServicePolicy(), mode, nil))
		h := handler.New(s, authz.NewClient("http://127.0.0.1:1"), zap.NewNop())
		handler.RegisterRoutes(r, h)
		handler.RegisterBNK0304Routes(r, h, s)
		return r
	}
	get := func(r chi.Router, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/banking/canonical-transactions/"+txnID, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	// What bank-reconciliation-svc's banking.HTTPClient sends.
	reconClient := map[string]string{
		envelope.HeaderTenantID:       tenant,
		envelope.HeaderActorSubjectID: "recon-matcher",
		envelope.HeaderCorrelationID:  "corr-c2",
		envelope.HeaderRequestID:      "req-c2",
		envelope.HeaderSourceChannel:  string(envelope.ChannelSystem),
		"Accept":                      "application/json",
	}
	// What it sent before the fix.
	tenantOnly := map[string]string{envelope.HeaderTenantID: tenant}

	for _, mode := range []envelope.Mode{envelope.ModeWriteStrict, envelope.ModeStrict} {
		t.Run(string(mode), func(t *testing.T) {
			r := router(mode)

			if rec := get(r, tenantOnly); rec.Code == http.StatusOK {
				t.Fatalf("a read with no principal must be refused, got 200: %s", rec.Body.String())
			}

			rec := get(r, reconClient)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 for the reconciliation client's headers, got %d: %s", rec.Code, rec.Body.String())
			}
			var got struct {
				TransactionID string  `json:"transaction_id"`
				TenantID      string  `json:"tenant_id"`
				Amount        float64 `json:"amount"`
				Currency      string  `json:"currency"`
				Status        string  `json:"status"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// The fields verifyCanonicalMatch compares, under the JSON names
			// banking.CanonicalTransaction decodes.
			if got.TransactionID != txnID || got.TenantID != tenant || got.Amount != 125.5 ||
				got.Currency != "USD" || got.Status != string(domain.TxnNormalized) {
				t.Fatalf("unexpected canonical transaction: %+v", got)
			}
		})
	}

	t.Run("write-strict refuses no principal with 401", func(t *testing.T) {
		if rec := get(router(envelope.ModeWriteStrict), tenantOnly); rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 from requirePrincipal, got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("strict refuses tenant+principal without the rest of the envelope", func(t *testing.T) {
		bare := map[string]string{envelope.HeaderTenantID: tenant, envelope.HeaderActorSubjectID: "recon-matcher"}
		if rec := get(router(envelope.ModeStrict), bare); rec.Code != http.StatusBadRequest {
			t.Fatalf("expected strict mode to refuse a partial envelope with 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}
