package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/handler"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

// grantingAuthz grants exactly the "principal|legal_entity|action" triples
// in grants and records every question it was asked.
type grantingAuthz struct {
	mu     sync.Mutex
	grants map[string]bool
	asked  []string
}

func (a *grantingAuthz) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			PrincipalID   string `json:"principal_id"`
			LegalEntityID string `json:"legal_entity_id"`
			ActionType    string `json:"action_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		key := q.PrincipalID + "|" + q.LegalEntityID + "|" + q.ActionType
		a.mu.Lock()
		a.asked = append(a.asked, key)
		ok := a.grants[key]
		a.mu.Unlock()
		out := "DENIED"
		if ok {
			out = "GRANTED"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": out})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestApproveMappingException_AuthorizedForTheOwningLegalEntity: the approval
// is asked for the legal entity of the connection whose statement line the
// exception was raised against (le-x), never for an empty entity; a denial
// leaves the exception OPEN; an unknown exception is 404 before any
// authorization is asked. The canonical-transaction read is authorized the
// same way.
func TestApproveMappingException_AuthorizedForTheOwningLegalEntity(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool, store.WithEventRegion("uk"))

	const tenant = "tenant-bnk04-authz"
	ctx := middleware.WithTenant(context.Background(), tenant)
	connID := seedConnection(t, ctx, s, tenant)
	ingested, err := s.IngestStatement(ctx, tenant, domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-authz",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 9, Currency: "USD", Description: "unknown"}},
	}, "auditor-1", "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	exc, err := s.QuarantineTransaction(ctx, domain.QuarantineTransactionParams{
		TenantID: tenant, StatementLineID: ingested.Lines[0].LineID, Reason: "unknown code", ActorPrincipalID: "ops-1",
	})
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	az := &grantingAuthz{grants: map[string]bool{
		"ops-2|le-x|" + handler.BANKING_MAPPING_EXCEPTION_APPROVE: true,
		"ops-2|le-x|" + handler.BANKING_TRANSACTION_READ:          true,
	}}
	srv := az.server(t)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	h := handler.New(s, authz.NewClient(srv.URL), zap.NewNop())
	handler.RegisterRoutes(r, h)
	handler.RegisterBNK0304Routes(r, h, s)
	call := func(method, path, tenantID, principal, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-Id", tenantID)
		if principal != "" {
			req.Header.Set("X-Principal-Id", principal)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	approve := "/v1/banking/mapping-exceptions/" + exc.ExceptionID + "/approve"

	// Unknown and foreign exceptions: 404, authorization never asked.
	if w := call("POST", "/v1/banking/mapping-exceptions/does-not-exist/approve", tenant, "ops-2", `{"category":"MISC"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown exception: %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", approve, "tenant-other", "ops-2", `{"category":"MISC"}`); w.Code != http.StatusNotFound {
		t.Fatalf("foreign exception: %d %s", w.Code, w.Body.String())
	}
	if len(az.asked) != 0 {
		t.Fatalf("authorization asked for an exception the caller cannot see: %v", az.asked)
	}

	// Denied for le-x: 403 and the exception stays OPEN.
	if w := call("POST", approve, tenant, "ops-3", `{"category":"MISC"}`); w.Code != http.StatusForbidden {
		t.Fatalf("denied approver: %d %s", w.Code, w.Body.String())
	}
	if got := az.asked[len(az.asked)-1]; got != "ops-3|le-x|"+handler.BANKING_MAPPING_EXCEPTION_APPROVE {
		t.Fatalf("authorization asked %q, want the exception's legal entity le-x", got)
	}

	// Granted for le-x: the canonical transaction is created.
	w := call("POST", approve, tenant, "ops-2", `{"category":"MISC"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("granted approver: %d %s", w.Code, w.Body.String())
	}
	var txn domain.CanonicalTransaction
	_ = json.Unmarshal(w.Body.Bytes(), &txn)

	// Canonical read: 401 / 403 / 200, asked for le-x.
	read := "/v1/banking/canonical-transactions/" + txn.TransactionID
	if w := call("GET", read, tenant, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("read without principal: %d", w.Code)
	}
	if w := call("GET", read, tenant, "ops-3", ""); w.Code != http.StatusForbidden {
		t.Fatalf("denied read: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", read, tenant, "ops-2", ""); w.Code != http.StatusOK {
		t.Fatalf("granted read: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", read, "tenant-other", "ops-2", ""); w.Code != http.StatusNotFound {
		t.Fatalf("foreign read: %d %s", w.Code, w.Body.String())
	}
}
