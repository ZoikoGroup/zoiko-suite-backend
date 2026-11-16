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
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

// TestAssembledRouter_ConnectionRoutesAreReachable registers the connection
// routes the way cmd/server/main.go does — RegisterRoutes AND
// RegisterBNK02Routes on one router — and checks every /v1/banking/
// connections route reaches its handler.
//
// Before this test existed, RegisterBNK02Routes mounted its own subrouter at
// /v1/banking/connections, which captured POST /v1/banking/connections and
// GET /v1/banking/connections/{id} away from RegisterRoutes' /v1/banking
// mount: both answered 404 in the running server while every test (which
// mounted one function's routes at a time) passed. A 404 or 405 here means a
// route is shadowed again.
func TestAssembledRouter_ConnectionRoutesAreReachable(t *testing.T) {
	mem := store.NewMemoryStore()
	h := New(mem, authz.NewClient(newGrantingAuthzServer(t).URL), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	RegisterRoutes(r, h)
	RegisterBNK02Routes(r, h, &fakeBNK02Store{}, &fakeBankAccounts{}, &fakeVault{})

	do := func(method, path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-Id", "tenant-assembled")
		req.Header.Set("X-Principal-Id", "principal-01")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// RegisterRoutes' routes, through both spellings.
	w := do(http.MethodPost, "/v1/banking/connections", domain.CreateConnectionRequest{
		LegalEntityID: "le-assembled", BankName: "Assembled Bank", AccountNumber: "GB00TEST1234567890", Currency: "GBP"})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v1/banking/connections: %d %s", w.Code, w.Body.String())
	}
	var created domain.BankConnection
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	for _, path := range []string{
		"/v1/banking/connections", "/v1/banking/connections/" + created.ConnectionID,
		"/v1/banking/connections/" + created.ConnectionID + "/account-number",
		"/v1/banking/accounts/" + created.ConnectionID, "/v1/banking/accounts/" + created.ConnectionID + "/account-number",
	} {
		if w := do(http.MethodGet, path, nil); w.Code != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
	}

	// RegisterBNK02Routes' routes still reach their handlers: anything but a
	// routing 404/405 (the handlers answer their own 4xx for these bodies).
	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/v1/banking/connections/request"},
		{http.MethodGet, "/v1/banking/connections/" + created.ConnectionID + "/events"},
		{http.MethodPost, "/v1/banking/connections/" + created.ConnectionID + "/activate"},
		{http.MethodPost, "/v1/banking/connections/" + created.ConnectionID + "/revoke"},
		{http.MethodPost, "/v1/banking/region-policies"},
	} {
		w := do(rt.method, rt.path, map[string]string{})
		if w.Code == http.StatusNotFound && w.Body.String() == "404 page not found\n" || w.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s is not routed: %d %q", rt.method, rt.path, w.Code, w.Body.String())
		}
	}
}
