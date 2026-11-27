package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/clients"
	svcenvelope "zoiko.io/banking-connector-svc/internal/envelope"
	"zoiko.io/banking-connector-svc/internal/health"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

// productionRouter mirrors cmd/server/main.go's middleware chain and route
// registration (RegisterAll is the same call main makes).
func productionRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))
	r.Get("/healthz", health.Handler())
	r.Get("/readyz", health.ReadyHandler(okPinger{}))
	h := New(store.NewMemoryStore(), authz.NewClient("http://authz.invalid"), zap.NewNop())
	var st *store.PgStore // never invoked: the test only resolves routes
	RegisterAll(r, h, st, clients.NewBankAccountHTTPClient("http://treasury.invalid"), clients.NewStubVaultClient())
	return r
}

// documentedRoutes is every route named in SPEC_DEVIATIONS.md's command/query
// tables plus the Postman aliases. Each must resolve to a handler: before the
// fix, the r.Route mounts in bnk02/bnk0304 shadowed RegisterRoutes' /v1/banking
// subrouter, and the first five entries answered 404.
var documentedRoutes = []struct{ method, path string }{
	{http.MethodPost, "/v1/banking/connections"},
	{http.MethodGet, "/v1/banking/connections"},
	{http.MethodGet, "/v1/banking/connections/c1"},
	{http.MethodGet, "/v1/banking/connections/c1/statements"},
	{http.MethodPost, "/v1/banking/statements"},
	{http.MethodPost, "/v1/banking/accounts"},
	{http.MethodGet, "/v1/banking/accounts"},
	{http.MethodGet, "/v1/banking/accounts/c1"},
	// BNK-02
	{http.MethodPost, "/v1/banking/connections/request"},
	{http.MethodGet, "/v1/banking/connections/c1/events"},
	{http.MethodPost, "/v1/banking/connections/c1/complete-authorization"},
	{http.MethodPost, "/v1/banking/connections/c1/activate"},
	{http.MethodPost, "/v1/banking/connections/c1/refresh"},
	{http.MethodPost, "/v1/banking/connections/c1/trigger-reconsent"},
	{http.MethodPost, "/v1/banking/connections/c1/reconsent"},
	{http.MethodPost, "/v1/banking/connections/c1/suspend"},
	{http.MethodPost, "/v1/banking/connections/c1/revoke"},
	{http.MethodPost, "/v1/banking/connections/c1/reconnect"},
	{http.MethodPost, "/v1/banking/connections/c1/rotate-credential"},
	{http.MethodPost, "/v1/banking/region-policies"},
	// BNK-03
	{http.MethodPost, "/v1/banking/statements/ingest"},
	{http.MethodPost, "/v1/banking/statements/s1/validate"},
	{http.MethodPost, "/v1/banking/statements/s1/accept"},
	{http.MethodPost, "/v1/banking/statements/s1/quarantine"},
	{http.MethodPost, "/v1/banking/statements/s1/reprocess"},
	// BNK-04
	{http.MethodPost, "/v1/banking/transactions/normalize"},
	{http.MethodPost, "/v1/banking/transactions/t1/re-normalize"},
	{http.MethodPost, "/v1/banking/transactions/quarantine"},
	{http.MethodPost, "/v1/banking/transaction-mappings"},
	{http.MethodGet, "/v1/banking/canonical-transactions/t1"},
	{http.MethodPost, "/v1/banking/mapping-exceptions/m1/approve"},
	// control population + probes
	{http.MethodGet, "/v1/control-populations/bank-transactions"},
	{http.MethodGet, "/healthz"},
	{http.MethodGet, "/readyz"},
}

// serveRouted sends method+path through the production router with a complete
// envelope and reports whether the ROUTER (not a handler) refused it. The
// router's own 404/405 are replaced with handlers that set X-Route-Miss, so a
// handler's legitimate JSON 404 ("bank connection not found") is not confused
// with an unregistered route. Handlers built on the nil PgStore may panic;
// that still proves the route was reached.
func serveRouted(r *chi.Mux, method, path string) (status int, miss string, body string) {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	for k, v := range map[string]string{
		"X-Tenant-Id": "tenant-a", "X-Principal-Id": "p1", "X-Request-Id": "r1",
		"X-Correlation-ID": "c1", "X-Source-Channel": "web", "Idempotency-Key": "k1",
		"X-Legal-Entity-Id": "le-1", "X-Purpose-Context": "route-contract-test",
	} {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	func() {
		defer func() {
			if recover() != nil {
				w.Code = http.StatusInternalServerError // reached a handler
			}
		}()
		r.ServeHTTP(w, req)
	}()
	return w.Code, w.Header().Get("X-Route-Miss"), w.Body.String()
}

func routerWithMissMarkers() *chi.Mux {
	r := productionRouter()
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Route-Miss", "404")
		w.WriteHeader(http.StatusNotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Route-Miss", "405")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	return r
}

func TestRouteTable_EveryDocumentedRouteIsReachable(t *testing.T) {
	r := routerWithMissMarkers()
	for _, rt := range documentedRoutes {
		status, miss, body := serveRouted(r, rt.method, rt.path)
		if miss != "" {
			t.Errorf("%s %s: router answered %s (route not reachable)", rt.method, rt.path, miss)
			continue
		}
		// An envelope refusal happens before routing and would mask the result.
		if strings.Contains(body, "envelope_incomplete") || strings.Contains(body, "missing_tenant_scope") {
			t.Errorf("%s %s: refused before routing (%d %s); fix the probe headers", rt.method, rt.path, status, body)
		}
	}
}

// TestRouteTable_UnknownRouteStill404 keeps the check above honest: the same
// probe must report a miss for routes that are genuinely not registered.
func TestRouteTable_UnknownRouteStill404(t *testing.T) {
	r := routerWithMissMarkers()
	if _, miss, _ := serveRouted(r, http.MethodDelete, "/v1/banking/connections/c1"); miss != "405" {
		t.Fatalf("DELETE /v1/banking/connections/{id}: miss=%q, want 405", miss)
	}
	if _, miss, _ := serveRouted(r, http.MethodGet, "/v1/banking/nope"); miss != "404" {
		t.Fatalf("unknown path: miss=%q, want 404", miss)
	}
}

// TestProbesArePublic: /healthz and /readyz must answer without any tenant
// or principal header (they used to return 401 from the tenant middleware).
func TestProbesArePublic(t *testing.T) {
	r := productionRouter()
	for _, p := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s without identity headers = %d %s, want 200", p, w.Code, w.Body.String())
		}
	}
}
