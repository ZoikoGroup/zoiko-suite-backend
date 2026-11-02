package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	svcenvelope "zoiko.io/hris-connector-svc/internal/envelope"
	"zoiko.io/hris-connector-svc/internal/health"
	"zoiko.io/hris-connector-svc/internal/middleware"
)

type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

// fullChainRouter mirrors cmd/server/main.go: tenant middleware, envelope,
// probes, then the business routes.
func fullChainRouter(t *testing.T) *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))
	r.Get("/healthz", health.Handler())
	r.Get("/readyz", health.ReadyHandler(okPinger{}))
	t.Helper()
	h := New(nil, nil, nil, nil)
	RegisterRoutes(r, h)
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

func TestProbesArePublic(t *testing.T) {
	r := fullChainRouter(t)
	for _, p := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s without identity headers = %d %s, want 200", p, w.Code, w.Body.String())
		}
	}
}

// TestRouteTable_EveryRouteIsReachable: every registered route resolves to a
// handler through the production middleware chain (not a router 404/405).
func TestRouteTable_EveryRouteIsReachable(t *testing.T) {
	r := fullChainRouter(t)
	routes := []struct{ method, path string }{
		{http.MethodPost, "/v1/hris/integrations"},
		{http.MethodGet, "/v1/hris/integrations"},
		{http.MethodGet, "/v1/hris/integrations/i1"},
		{http.MethodPost, "/v1/hris/sync"},
		{http.MethodGet, "/v1/hris/sync/jobs"},
		{http.MethodPost, "/v1/hris/syncs"},
		{http.MethodGet, "/v1/hris/syncs"},
		{http.MethodGet, "/v1/hris/syncs/i1"},
	}
	for _, rt := range routes {
		req := httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}"))
		for k, v := range map[string]string{
			"X-Tenant-Id": "tenant-a", "X-Principal-Id": "p1", "X-Request-Id": "r1",
			"X-Correlation-ID": "c1", "X-Source-Channel": "web", "Idempotency-Key": "k1",
			"X-Legal-Entity-Id": "le-1", "X-Purpose-Context": "route-contract-test",
		} {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }() // nil store: reaching the handler is enough
			r.ServeHTTP(w, req)
		}()
		if miss := w.Header().Get("X-Route-Miss"); miss != "" {
			t.Errorf("%s %s: router answered %s", rt.method, rt.path, miss)
		}
		if strings.Contains(w.Body.String(), "envelope_incomplete") {
			t.Errorf("%s %s refused before routing: %s", rt.method, rt.path, w.Body.String())
		}
	}
}
