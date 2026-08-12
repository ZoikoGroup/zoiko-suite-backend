package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/commercial-account-svc/internal/domain"
)

func newSubscriptionTestRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	RegisterSubscriptionRoutes(r, h)
	return r
}

func createTestCatalogAndPlan(t *testing.T, r *chi.Mux, orgID string) (catalogID, planID string) {
	t.Helper()
	wCat := httptest.NewRecorder()
	r.ServeHTTP(wCat, buildRequest(http.MethodPost, "/v1/price-catalogs", domain.CreatePriceCatalogRequest{
		CatalogCode:   "2026-Q1-" + orgID,
		EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if wCat.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating catalog, got %d — %s", wCat.Code, wCat.Body.String())
	}
	var catalog domain.PriceCatalog
	_ = json.NewDecoder(wCat.Body).Decode(&catalog)

	wPlan := httptest.NewRecorder()
	r.ServeHTTP(wPlan, buildRequest(http.MethodPost, "/v1/plans", domain.CreatePlanRequest{
		CatalogVersionID:      catalog.CatalogVersionID,
		PlanCode:              "GROWTH",
		DisplayName:           "Growth",
		BillingInterval:       "MONTHLY",
		BasePriceAmount:       499,
		BasePriceCurrencyCode: "USD",
	}))
	if wPlan.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating plan, got %d — %s", wPlan.Code, wPlan.Body.String())
	}
	var plan domain.Plan
	_ = json.NewDecoder(wPlan.Body).Decode(&plan)

	wLimit := httptest.NewRecorder()
	r.ServeHTTP(wLimit, buildRequest(http.MethodPut, "/v1/plans/"+plan.PlanID+"/entitlement-limits", domain.SetEntitlementLimitRequest{
		MetricType: "USERS",
		LimitValue: int64Ptr(10),
	}))
	if wLimit.Code != http.StatusOK {
		t.Fatalf("expected 200 setting entitlement limit, got %d — %s", wLimit.Code, wLimit.Body.String())
	}

	return catalog.CatalogVersionID, plan.PlanID
}

func int64Ptr(v int64) *int64 { return &v }

func TestResolveEntitlement_PlanLimitWithNoOverlay(t *testing.T) {
	h := newTestHandler()
	r := newSubscriptionTestRouter(h)

	_, planID := createTestCatalogAndPlan(t, r, "org-ent-1")

	wSub := httptest.NewRecorder()
	r.ServeHTTP(wSub, buildRequest(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: "ca-ent-1",
		PlanID:              planID,
	}))
	if wSub.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating subscription, got %d — %s", wSub.Code, wSub.Body.String())
	}
	var sub domain.CommercialSubscription
	_ = json.NewDecoder(wSub.Body).Decode(&sub)

	wResolve := httptest.NewRecorder()
	r.ServeHTTP(wResolve, buildRequest(http.MethodGet, "/v1/subscriptions/"+sub.SubscriptionID+"/entitlements/USERS", nil))
	if wResolve.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wResolve.Code, wResolve.Body.String())
	}
	var resolved domain.ResolvedEntitlement
	_ = json.NewDecoder(wResolve.Body).Decode(&resolved)
	if resolved.Source != "PLAN" || resolved.LimitValue == nil || *resolved.LimitValue != 10 {
		t.Fatalf("expected PLAN source with limit 10, got %+v", resolved)
	}
}

func TestResolveEntitlement_OverlayOverridesPlan(t *testing.T) {
	h := newTestHandler()
	r := newSubscriptionTestRouter(h)

	_, planID := createTestCatalogAndPlan(t, r, "org-ent-2")

	wSub := httptest.NewRecorder()
	r.ServeHTTP(wSub, buildRequest(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: "ca-ent-2",
		PlanID:              planID,
	}))
	var sub domain.CommercialSubscription
	_ = json.NewDecoder(wSub.Body).Decode(&sub)

	wOverlay := httptest.NewRecorder()
	r.ServeHTTP(wOverlay, buildRequest(http.MethodPost, "/v1/contract-entitlement-overlays", domain.CreateOverlayRequest{
		CommercialAccountID:   "ca-ent-2",
		MetricType:            "USERS",
		OverrideLimitValue:    int64Ptr(500),
		ApprovedByPrincipalID: "cro-1",
		EffectiveFrom:         "2020-01-01T00:00:00Z",
	}))
	if wOverlay.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating overlay, got %d — %s", wOverlay.Code, wOverlay.Body.String())
	}

	wResolve := httptest.NewRecorder()
	r.ServeHTTP(wResolve, buildRequest(http.MethodGet, "/v1/subscriptions/"+sub.SubscriptionID+"/entitlements/USERS", nil))
	var resolved domain.ResolvedEntitlement
	_ = json.NewDecoder(wResolve.Body).Decode(&resolved)
	if resolved.Source != "OVERLAY" || resolved.LimitValue == nil || *resolved.LimitValue != 500 {
		t.Fatalf("expected OVERLAY source with limit 500, got %+v", resolved)
	}
}

func TestRecordUsageEvent_DedupesRetry(t *testing.T) {
	h := newTestHandler()
	r := newSubscriptionTestRouter(h)

	_, planID := createTestCatalogAndPlan(t, r, "org-usage-1")
	wSub := httptest.NewRecorder()
	r.ServeHTTP(wSub, buildRequest(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: "ca-usage-1",
		PlanID:              planID,
	}))
	var sub domain.CommercialSubscription
	_ = json.NewDecoder(wSub.Body).Decode(&sub)

	body := domain.RecordUsageEventRequest{
		UsageEventID:  "evt-fixed-key-1",
		MetricType:    "AI_TOKENS",
		Quantity:      42,
		SourceService: "ai-svc",
	}

	wFirst := httptest.NewRecorder()
	r.ServeHTTP(wFirst, buildRequest(http.MethodPost, "/v1/subscriptions/"+sub.SubscriptionID+"/usage-events", body))
	if wFirst.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first record, got %d — %s", wFirst.Code, wFirst.Body.String())
	}

	// Simulated retry with the identical idempotency key must not double-count.
	wRetry := httptest.NewRecorder()
	r.ServeHTTP(wRetry, buildRequest(http.MethodPost, "/v1/subscriptions/"+sub.SubscriptionID+"/usage-events", body))
	if wRetry.Code != http.StatusOK {
		t.Fatalf("expected 200 (already_recorded) on retry, got %d — %s", wRetry.Code, wRetry.Body.String())
	}
}

func TestSubscriptionChange_PreviewThenConfirm_SecondConfirmFails(t *testing.T) {
	h := newTestHandler()
	r := newSubscriptionTestRouter(h)

	_, planID := createTestCatalogAndPlan(t, r, "org-change-1")
	wSub := httptest.NewRecorder()
	r.ServeHTTP(wSub, buildRequest(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: "ca-change-1",
		PlanID:              planID,
	}))
	var sub domain.CommercialSubscription
	_ = json.NewDecoder(wSub.Body).Decode(&sub)

	// Second plan to upgrade to.
	wCat2 := httptest.NewRecorder()
	r.ServeHTTP(wCat2, buildRequest(http.MethodPost, "/v1/price-catalogs", domain.CreatePriceCatalogRequest{
		CatalogCode:   "2026-Q2-change-1",
		EffectiveFrom: "2026-04-01T00:00:00Z",
	}))
	var catalog2 domain.PriceCatalog
	_ = json.NewDecoder(wCat2.Body).Decode(&catalog2)
	wPlan2 := httptest.NewRecorder()
	r.ServeHTTP(wPlan2, buildRequest(http.MethodPost, "/v1/plans", domain.CreatePlanRequest{
		CatalogVersionID:      catalog2.CatalogVersionID,
		PlanCode:              "ENTERPRISE",
		DisplayName:           "Enterprise",
		BillingInterval:       "MONTHLY",
		BasePriceAmount:       1999,
		BasePriceCurrencyCode: "USD",
	}))
	var plan2 domain.Plan
	_ = json.NewDecoder(wPlan2.Body).Decode(&plan2)

	wPreview := httptest.NewRecorder()
	r.ServeHTTP(wPreview, buildRequest(http.MethodPost, "/v1/subscription-change-requests", domain.PreviewChangeRequest{
		SubscriptionID: sub.SubscriptionID,
		TargetPlanID:   plan2.PlanID,
	}))
	if wPreview.Code != http.StatusCreated {
		t.Fatalf("expected 201 previewing change, got %d — %s", wPreview.Code, wPreview.Body.String())
	}
	var change domain.SubscriptionChangeRequest
	_ = json.NewDecoder(wPreview.Body).Decode(&change)

	wConfirm := httptest.NewRecorder()
	r.ServeHTTP(wConfirm, buildRequest(http.MethodPost, "/v1/subscription-change-requests/"+change.ChangeRequestID+"/confirm", nil))
	if wConfirm.Code != http.StatusOK {
		t.Fatalf("expected 200 confirming change, got %d — %s", wConfirm.Code, wConfirm.Body.String())
	}
	var updated domain.CommercialSubscription
	_ = json.NewDecoder(wConfirm.Body).Decode(&updated)
	if updated.PlanID != plan2.PlanID {
		t.Fatalf("expected subscription repointed to plan2, got plan_id=%s", updated.PlanID)
	}

	// Confirming an already-applied change request must fail, not silently
	// re-apply it a second time.
	wConfirmAgain := httptest.NewRecorder()
	r.ServeHTTP(wConfirmAgain, buildRequest(http.MethodPost, "/v1/subscription-change-requests/"+change.ChangeRequestID+"/confirm", nil))
	if wConfirmAgain.Code != http.StatusConflict {
		t.Fatalf("expected 409 on second confirm, got %d", wConfirmAgain.Code)
	}
}
