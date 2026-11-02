package health_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	svcenvelope "zoiko.io/connectivity-api-bridge-svc/internal/envelope"
	"zoiko.io/connectivity-api-bridge-svc/internal/health"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

// TestProbesArePublicThroughProductionChain: /healthz and /readyz must answer
// 200 with no tenant/principal headers through the same tenant + envelope
// middleware cmd/server/main.go installs (they used to return 401), while a
// business route without a tenant is still refused.
func TestProbesArePublicThroughProductionChain(t *testing.T) {
	r := chi.NewRouter()
	r.Use(middleware.TenantContext)
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))
	r.Get("/healthz", health.Handler())
	r.Get("/readyz", health.ReadyHandler(okPinger{}))
	r.Get("/v1/bridges", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	for _, p := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s = %d %s, want 200", p, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/bridges", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("business route without tenant = %d, want 401", w.Code)
	}
}
