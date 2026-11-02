package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

// newPolicyAuthzServer is a policy-driven authorization-svc stand-in: it
// grants exactly the (principal, action) pairs in grants and denies the rest,
// returning HTTP 200 with a decision body like the real service.
func newPolicyAuthzServer(t *testing.T, grants map[string]bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PrincipalID string `json:"principal_id"`
			ActionType  string `json:"action_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		outcome := "DENIED"
		if grants[req.PrincipalID+"|"+req.ActionType] {
			outcome = "GRANTED"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": outcome})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func policyRouter(t *testing.T, authzURL string) chi.Router {
	h := New(store.NewMemoryStore(), authz.NewClient(authzURL), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	return r
}

func send(r chi.Router, method, path, tenant, principal string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAuthz_CreateConnection_AllowedAndDenied(t *testing.T) {
	srv := newPolicyAuthzServer(t, map[string]bool{"maker|" + BANKING_CONNECTION_CREATE: true})
	r := policyRouter(t, srv.URL)
	body := map[string]string{"legal_entity_id": "le-1", "bank_name": "Bank", "account_number": "GB00TEST12345678", "currency": "EUR"}

	if w := send(r, http.MethodPost, "/v1/banking/connections", "tenant-a", "maker", body); w.Code != http.StatusCreated {
		t.Fatalf("granted principal: got %d %s, want 201", w.Code, w.Body.String())
	}
	if w := send(r, http.MethodPost, "/v1/banking/connections", "tenant-a", "viewer", body); w.Code != http.StatusForbidden {
		t.Fatalf("denied principal: got %d %s, want 403", w.Code, w.Body.String())
	}
	if w := send(r, http.MethodPost, "/v1/banking/connections", "tenant-a", "", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal: got %d, want 401", w.Code)
	}
}

func TestAuthz_IngestStatement_DeniedAndCrossTenant(t *testing.T) {
	srv := newPolicyAuthzServer(t, map[string]bool{
		"maker|" + BANKING_CONNECTION_CREATE: true,
		"maker|" + BANKING_STATEMENT_INGEST:  true,
	})
	r := policyRouter(t, srv.URL)
	w := send(r, http.MethodPost, "/v1/banking/connections", "tenant-a", "maker",
		map[string]string{"legal_entity_id": "le-1", "bank_name": "Bank"})
	if w.Code != http.StatusCreated {
		t.Fatalf("setup create: %d %s", w.Code, w.Body.String())
	}
	var conn struct {
		ConnectionID string `json:"connection_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &conn)
	stmt := map[string]any{"connection_id": conn.ConnectionID, "statement_format": "CAMT053"}

	if w := send(r, http.MethodPost, "/v1/banking/statements", "tenant-a", "maker", stmt); w.Code != http.StatusCreated {
		t.Fatalf("granted ingest: got %d %s, want 201", w.Code, w.Body.String())
	}
	// A principal holding create but not ingest is refused.
	if w := send(r, http.MethodPost, "/v1/banking/statements", "tenant-a", "viewer", stmt); w.Code != http.StatusForbidden {
		t.Fatalf("denied ingest: got %d %s, want 403", w.Code, w.Body.String())
	}
	// Another tenant cannot even resolve the connection (no existence leak).
	if w := send(r, http.MethodPost, "/v1/banking/statements", "tenant-b", "maker", stmt); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant ingest: got %d %s, want 404", w.Code, w.Body.String())
	}
	if w := send(r, http.MethodGet, "/v1/banking/connections/"+conn.ConnectionID+"/statements", "tenant-b", "maker", nil); w.Code != http.StatusOK || bytes.Contains(w.Body.Bytes(), []byte(conn.ConnectionID)) {
		t.Fatalf("cross-tenant statement list leaked: %d %s", w.Code, w.Body.String())
	}
}

func TestAuthz_ServiceUnavailable_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	r := policyRouter(t, srv.URL)
	w := send(r, http.MethodPost, "/v1/banking/connections", "tenant-a", "maker",
		map[string]string{"legal_entity_id": "le-1", "bank_name": "Bank"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("authz down: got %d %s, want 503 (never an implicit grant)", w.Code, w.Body.String())
	}
}
