package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/go-chi/chi/v5"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

// connReadFixture creates one connection as "maker" in tenant-a / le-1 and
// returns its id. reader holds BANKING_CONNECTION_READ and
// BANKING_STATEMENT_READ; viewer holds nothing.
func connReadFixture(t *testing.T) (chi.Router, string) {
	t.Helper()
	srv := newPolicyAuthzServer(t, map[string]bool{
		"maker|" + BANKING_CONNECTION_CREATE: true,
		"reader|" + BANKING_CONNECTION_READ:  true,
		"reader|" + BANKING_STATEMENT_READ:   true,
	})
	r := policyRouter(t, srv.URL)
	w := send(r, http.MethodPost, "/v1/banking/connections", "tenant-a", "maker",
		map[string]string{"legal_entity_id": "le-1", "bank_name": "Bank", "account_number": "GB00TEST12345678"})
	if w.Code != http.StatusCreated {
		t.Fatalf("setup create: %d %s", w.Code, w.Body.String())
	}
	var c domain.BankConnection
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	return r, c.ConnectionID
}

func TestReads_RequirePrincipalAndAuthorization(t *testing.T) {
	r, id := connReadFixture(t)
	paths := []string{
		"/v1/banking/connections",
		"/v1/banking/connections?legal_entity_id=le-1",
		"/v1/banking/connections/" + id,
		"/v1/banking/accounts",
		"/v1/banking/accounts/" + id,
		"/v1/banking/connections/" + id + "/statements",
	}
	for _, p := range paths {
		if w := send(r, http.MethodGet, p, "tenant-a", "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without principal: %d, want 401 (%s)", p, w.Code, w.Body.String())
		}
		if w := send(r, http.MethodGet, p, "tenant-a", "viewer", nil); w.Code != http.StatusForbidden {
			t.Errorf("GET %s denied: %d, want 403 (%s)", p, w.Code, w.Body.String())
		}
		if w := send(r, http.MethodGet, p, "tenant-a", "reader", nil); w.Code != http.StatusOK {
			t.Errorf("GET %s granted: %d, want 200 (%s)", p, w.Code, w.Body.String())
		}
	}
}

func TestReads_AuthzUnavailableFailsClosed(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	mem := store.NewMemoryStore()
	ctx := middleware.WithTenant(t.Context(), "tenant-a")
	c := &domain.BankConnection{TenantID: "tenant-a", LegalEntityID: "le-1", BankName: "B", Status: domain.ConnStatusRequested}
	if err := mem.CreateConnection(ctx, c, "maker", ""); err != nil {
		t.Fatal(err)
	}
	h := New(mem, authz.NewClient(down.URL), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	for _, p := range []string{"/v1/banking/connections", "/v1/banking/connections/" + c.ConnectionID, "/v1/banking/connections/" + c.ConnectionID + "/statements"} {
		if w := send(r, http.MethodGet, p, "tenant-a", "reader", nil); w.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s with authz down: %d, want 503 (%s)", p, w.Code, w.Body.String())
		}
	}
}

// A principal-less list is served only to a caller that declares itself an
// internal service call (purchase-request-svc authorizeRead pattern).
func TestListConnections_SystemChannelWithoutPrincipal(t *testing.T) {
	r, _ := connReadFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/banking/connections?legal_entity_id=le-1", nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Source-Channel", "system")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("system-channel list: %d %s", w.Code, w.Body.String())
	}
	// The exemption is for lists only; a single read still needs a principal.
	req = httptest.NewRequest(http.MethodGet, "/v1/banking/connections/anything", nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Source-Channel", "system")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("system-channel get without principal: %d, want 401", w.Code)
	}
}

func TestRead_ForeignConnectionIs404(t *testing.T) {
	r, id := connReadFixture(t)
	if w := send(r, http.MethodGet, "/v1/banking/connections/"+id, "tenant-b", "reader", nil); w.Code != http.StatusNotFound {
		t.Fatalf("foreign connection: %d, want 404", w.Code)
	}
}

// CreateConnection contacts no bank, so it must not claim a live link.
func TestCreateConnection_StartsRequestedNotConnected(t *testing.T) {
	r, id := connReadFixture(t)
	w := send(r, http.MethodGet, "/v1/banking/connections/"+id, "tenant-a", "reader", nil)
	var c domain.BankConnection
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.Status != domain.ConnStatusRequested {
		t.Fatalf("status %q, want %q", c.Status, domain.ConnStatusRequested)
	}
}
