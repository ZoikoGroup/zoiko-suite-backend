package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

func newSubscriptionTestRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterRoutes(r, h)
	RegisterSubscriptionRoutes(r, h)
	return r
}

// The doc7 catalog write routes published prices with no maker-checker and
// stored them as floats. They now refuse with 410 and point at COM-01. The
// rest of the Chunk 6 subscription/entitlement/billing-transfer surface this
// file used to test (CreateSubscription, ResolveEntitlement,
// RecordUsageEvent, SetSubscriptionStatus, CreateEvaluationProgram,
// subscription-change-requests, contract-entitlement-overlays,
// billing-source-transfers) is retired per SPEC_DEVIATIONS.md A3 — see
// commit history for the removal and handler.go/subscription_handler.go for
// what replaced it.
func TestLegacyCatalogWrites_AreRetired(t *testing.T) {
	r := newSubscriptionTestRouter(newTestHandler())
	for _, rq := range []struct{ method, path string }{
		{http.MethodPost, "/v1/price-catalogs"},
		{http.MethodPost, "/v1/plans"},
		{http.MethodPut, "/v1/plans/" + uuid.NewString() + "/entitlement-limits"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(rq.method, rq.path, map[string]any{"catalog_code": "x"}))
		if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), CodeEndpointRetired) {
			t.Errorf("%s %s: HTTP %d %s, want 410 %s", rq.method, rq.path, w.Code, w.Body.String(), CodeEndpointRetired)
		}
	}
}

// TestLegacySubscriptionRoutes_NoLongerMounted proves the retired routes
// are gone, not merely unauthorized — a 404 here, not a 401/403.
func TestLegacySubscriptionRoutes_NoLongerMounted(t *testing.T) {
	r := newSubscriptionTestRouter(newTestHandler())
	for _, rq := range []struct{ method, path string }{
		{http.MethodPost, "/v1/subscriptions"},
		{http.MethodGet, "/v1/subscriptions/" + uuid.NewString()},
		{http.MethodPost, "/v1/subscription-change-requests"},
		{http.MethodPost, "/v1/contract-entitlement-overlays"},
		{http.MethodPost, "/v1/billing-source-transfers"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(rq.method, rq.path, map[string]any{}))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: expected 404 (route retired), got %d — %s", rq.method, rq.path, w.Code, w.Body.String())
		}
	}
}
