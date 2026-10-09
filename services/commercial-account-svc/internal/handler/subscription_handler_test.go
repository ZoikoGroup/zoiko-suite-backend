package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

func newSubscriptionTestRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterRoutes(r, h)
	RegisterSubscriptionRoutes(r, h)
	return r
}

func seedTestAccount(h *Handler, accountID, orgID string) {
	if ss, ok := h.store.(*stubStore); ok {
		ss.SeedAccount(accountID, orgID)
	}
}

// createTestCatalogAndPlan seeds a doc7 catalog and plan through the store.
// The HTTP catalog write routes are retired (410); existing doc7 plans are
// still what the doc7 subscription endpoints under test bind to.
func createTestCatalogAndPlan(t *testing.T, h *Handler, orgID string) (catalogID, planID string) {
	t.Helper()
	seedTestAccount(h, "ca-"+orgID, "org-test-01")
	if strings.HasPrefix(orgID, "org-") {
		seedTestAccount(h, "ca-"+strings.TrimPrefix(orgID, "org-"), "org-test-01")
	}
	return seedLegacyPlan(t, h, "2026-Q1-"+orgID, "GROWTH", 499, true)
}

func seedLegacyPlan(t *testing.T, h *Handler, catalogCode, planCode string, amount float64, withLimit bool) (catalogID, planID string) {
	t.Helper()
	ctx := context.Background()
	c := &domain.PriceCatalog{CatalogVersionID: uuid.NewString(), CatalogCode: catalogCode, Status: domain.CatalogStatusPublished,
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "seed"}
	if err := h.store.CreatePriceCatalog(ctx, c); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	p := &domain.Plan{PlanID: uuid.NewString(), CatalogVersionID: c.CatalogVersionID, PlanCode: planCode, DisplayName: planCode,
		BillingInterval: "MONTHLY", BasePriceAmount: amount, BasePriceCurrencyCode: "USD", CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "seed"}
	if err := h.store.CreatePlan(ctx, p); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if withLimit {
		if err := h.store.SetEntitlementLimit(ctx, &domain.EntitlementLimit{EntitlementLimitID: uuid.NewString(), PlanID: p.PlanID,
			MetricType: "USERS", LimitValue: int64Ptr(10)}); err != nil {
			t.Fatalf("seed entitlement limit: %v", err)
		}
	}
	return c.CatalogVersionID, p.PlanID
}

// The doc7 catalog write routes published prices with no maker-checker and
// stored them as floats. They now refuse with 410 and point at COM-01.
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

func int64Ptr(v int64) *int64 { return &v }

func TestResolveEntitlement_PlanLimitWithNoOverlay(t *testing.T) {
	h := newTestHandler()
	r := newSubscriptionTestRouter(h)

	_, planID := createTestCatalogAndPlan(t, h, "org-ent-1")

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

	_, planID := createTestCatalogAndPlan(t, h, "org-ent-2")

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

	_, planID := createTestCatalogAndPlan(t, h, "org-usage-1")
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

	_, planID := createTestCatalogAndPlan(t, h, "org-change-1")
	wSub := httptest.NewRecorder()
	r.ServeHTTP(wSub, buildRequest(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: "ca-change-1",
		PlanID:              planID,
	}))
	var sub domain.CommercialSubscription
	_ = json.NewDecoder(wSub.Body).Decode(&sub)

	// Second plan to upgrade to.
	_, plan2ID := seedLegacyPlan(t, h, "2026-Q2-change-1", "ENTERPRISE", 1999, false)

	wPreview := httptest.NewRecorder()
	r.ServeHTTP(wPreview, buildRequest(http.MethodPost, "/v1/subscription-change-requests", domain.PreviewChangeRequest{
		SubscriptionID: sub.SubscriptionID,
		TargetPlanID:   plan2ID,
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
	if updated.PlanID != plan2ID {
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

// TestDunning_EscalatesThenRecoversIdempotently exercises the doc7 §O1-O3
// state machine end to end: ACTIVE -> PAST_DUE -> RESTRICTED -> SUSPENDED as
// dunning escalates, then a recovery straight back to ACTIVE, and finally a
// repeat of that same recovery call proving it's an idempotent no-op rather
// than an error.
func TestDunning_EscalatesThenRecoversIdempotently(t *testing.T) {
	h := newTestHandler()
	r := newSubscriptionTestRouter(h)

	_, planID := createTestCatalogAndPlan(t, h, "org-dun-1")
	wSub := httptest.NewRecorder()
	r.ServeHTTP(wSub, buildRequest(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: "ca-dun-1",
		PlanID:              planID,
	}))
	var sub domain.CommercialSubscription
	_ = json.NewDecoder(wSub.Body).Decode(&sub)
	if sub.Status != domain.SubscriptionStatusActive {
		t.Fatalf("expected new subscription to start ACTIVE, got %s", sub.Status)
	}

	setStatus := func(newStatus string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/subscriptions/"+sub.SubscriptionID+"/status", domain.SetSubscriptionStatusRequest{
			NewStatus: newStatus,
			Reason:    "test escalation",
		}))
		return w
	}

	for _, step := range []string{"PAST_DUE", "RESTRICTED", "SUSPENDED"} {
		w := setStatus(step)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 transitioning to %s, got %d — %s", step, w.Code, w.Body.String())
		}
	}

	wRecover := setStatus("ACTIVE")
	if wRecover.Code != http.StatusOK {
		t.Fatalf("expected 200 recovering to ACTIVE, got %d — %s", wRecover.Code, wRecover.Body.String())
	}

	// Idempotent repeat: already ACTIVE, must succeed without error.
	wRecoverAgain := setStatus("ACTIVE")
	if wRecoverAgain.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent repeat recovery, got %d — %s", wRecoverAgain.Code, wRecoverAgain.Body.String())
	}

	wEvents := httptest.NewRecorder()
	r.ServeHTTP(wEvents, buildRequest(http.MethodGet, "/v1/subscriptions/"+sub.SubscriptionID+"/status-events", nil))
	if wEvents.Code != http.StatusOK {
		t.Fatalf("expected 200 listing status events, got %d — %s", wEvents.Code, wEvents.Body.String())
	}
	var eventsResp struct {
		StatusEvents []domain.SubscriptionStatusEvent `json:"status_events"`
	}
	_ = json.NewDecoder(wEvents.Body).Decode(&eventsResp)
	// 4 real transitions logged (ACTIVE->PAST_DUE->RESTRICTED->SUSPENDED->ACTIVE);
	// the idempotent repeat must NOT add a 5th event.
	if len(eventsResp.StatusEvents) != 4 {
		t.Fatalf("expected exactly 4 logged status events, got %d", len(eventsResp.StatusEvents))
	}
}

// TestDunning_RejectsInvalidTransition proves the state machine is
// fail-closed: a terminal CANCELED subscription can never be reactivated
// straight to ACTIVE.
func TestDunning_RejectsInvalidTransition(t *testing.T) {
	h := newTestHandler()
	r := newSubscriptionTestRouter(h)

	_, planID := createTestCatalogAndPlan(t, h, "org-dun-2")
	wSub := httptest.NewRecorder()
	r.ServeHTTP(wSub, buildRequest(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: "ca-dun-2",
		PlanID:              planID,
	}))
	var sub domain.CommercialSubscription
	_ = json.NewDecoder(wSub.Body).Decode(&sub)

	wCancel := httptest.NewRecorder()
	r.ServeHTTP(wCancel, buildRequest(http.MethodPost, "/v1/subscriptions/"+sub.SubscriptionID+"/status", domain.SetSubscriptionStatusRequest{
		NewStatus: "CANCELED",
	}))
	if wCancel.Code != http.StatusOK {
		t.Fatalf("expected 200 canceling, got %d — %s", wCancel.Code, wCancel.Body.String())
	}

	wReactivate := httptest.NewRecorder()
	r.ServeHTTP(wReactivate, buildRequest(http.MethodPost, "/v1/subscriptions/"+sub.SubscriptionID+"/status", domain.SetSubscriptionStatusRequest{
		NewStatus: "ACTIVE",
	}))
	if wReactivate.Code != http.StatusConflict {
		t.Fatalf("expected 409 rejecting CANCELED->ACTIVE, got %d — %s", wReactivate.Code, wReactivate.Body.String())
	}
}

// TestBillingSourceTransfer_CancelsOldAndPreventsDoubleBilling verifies doc7
// §P3: transferring an account from DIRECT to ZOIKO_ONE_BUNDLE cancels the
// old subscription and creates a new one in the same atomic operation, and
// the account never ends up with two simultaneously non-terminal
// subscriptions.
func TestBillingSourceTransfer_CancelsOldAndPreventsDoubleBilling(t *testing.T) {
	h := newTestHandler()
	r := newSubscriptionTestRouter(h)

	_, planID := createTestCatalogAndPlan(t, h, "org-transfer-1")
	wSub := httptest.NewRecorder()
	r.ServeHTTP(wSub, buildRequest(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: "ca-transfer-1",
		PlanID:              planID,
		BillingSource:       "DIRECT",
	}))
	var oldSub domain.CommercialSubscription
	_ = json.NewDecoder(wSub.Body).Decode(&oldSub)

	wTransfer := httptest.NewRecorder()
	r.ServeHTTP(wTransfer, buildRequest(http.MethodPost, "/v1/billing-source-transfers", domain.TransferBillingSourceRequest{
		CommercialAccountID: "ca-transfer-1",
		OldSubscriptionID:   oldSub.SubscriptionID,
		NewBillingSource:    "ZOIKO_ONE_BUNDLE",
	}))
	if wTransfer.Code != http.StatusCreated {
		t.Fatalf("expected 201 on transfer, got %d — %s", wTransfer.Code, wTransfer.Body.String())
	}
	var transfer domain.BillingSourceTransfer
	_ = json.NewDecoder(wTransfer.Body).Decode(&transfer)
	if transfer.NewSubscriptionID == nil {
		t.Fatalf("expected transfer to record a new_subscription_id")
	}

	wOld := httptest.NewRecorder()
	r.ServeHTTP(wOld, buildRequest(http.MethodGet, "/v1/subscriptions/"+oldSub.SubscriptionID, nil))
	var oldAfter domain.CommercialSubscription
	_ = json.NewDecoder(wOld.Body).Decode(&oldAfter)
	if oldAfter.Status != domain.SubscriptionStatusCanceled {
		t.Fatalf("expected old subscription CANCELED after transfer, got %s", oldAfter.Status)
	}

	wNew := httptest.NewRecorder()
	r.ServeHTTP(wNew, buildRequest(http.MethodGet, "/v1/subscriptions/"+*transfer.NewSubscriptionID, nil))
	var newSub domain.CommercialSubscription
	_ = json.NewDecoder(wNew.Body).Decode(&newSub)
	if newSub.BillingSource != domain.BillingSourceZoikoOneBundle || newSub.Status != domain.SubscriptionStatusActive {
		t.Fatalf("expected new subscription ACTIVE on ZOIKO_ONE_BUNDLE, got %+v", newSub)
	}

	// A second attempt to create yet another active subscription on the same
	// account (without going through a transfer that cancels one first) must
	// be blocked by the existing double-billing constraint.
	wDup := httptest.NewRecorder()
	r.ServeHTTP(wDup, buildRequest(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: "ca-transfer-1",
		PlanID:              planID,
	}))
	if wDup.Code != http.StatusConflict {
		t.Fatalf("expected 409 preventing a second concurrent subscription, got %d — %s", wDup.Code, wDup.Body.String())
	}
}

// ── Gap 1 (Tracker Row 84a) Authorization Scope Verification Tests ─────────

type recordingAuthz struct {
	mu     sync.Mutex
	calls  []authzCall
	grants map[string]bool // key: principal|legalEntityID|actionType
}

type authzCall struct {
	Principal     string
	LegalEntityID string
	ActionType    string
}

func newRecordingAuthz() *recordingAuthz {
	return &recordingAuthz{grants: make(map[string]bool)}
}

func (a *recordingAuthz) CheckAllowed(_ context.Context, principalID, legalEntityID, actionType string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, authzCall{
		Principal:     principalID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
	})
	if a.grants[principalID+"|"+legalEntityID+"|"+actionType] {
		return nil
	}
	return authz.ErrAuthorizationDenied
}

func (a *recordingAuthz) grant(principalID, legalEntityID, actionType string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grants[principalID+"|"+legalEntityID+"|"+actionType] = true
}

func (a *recordingAuthz) lastCall() authzCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return authzCall{}
	}
	return a.calls[len(a.calls)-1]
}

func newTestHandlerWithAuthz(az AuthzChecker) (*Handler, *stubStore) {
	logger, _ := zap.NewDevelopment()
	st := newStubStore()
	return New(st, &stubPublisher{}, az, logger), st
}

func buildRequestWithHeaders(method, path string, body interface{}, tenantID, principalID string) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", tenantID)
	r.Header.Set("X-Principal-Id", principalID)
	return r
}

// TestSubscriptionAuthorization_AllSevenMutationsUseOrganizationScope proves that all
// seven subscription mutation operations evaluate authorization against the owning commercial
// account's authoritative OrganizationID, NOT its CommercialAccountID (tracker row 84a).
func TestSubscriptionAuthorization_AllSevenMutationsUseOrganizationScope(t *testing.T) {
	az := newRecordingAuthz()
	h, st := newTestHandlerWithAuthz(az)
	r := newSubscriptionTestRouter(h)

	const (
		orgID       = "org-scope-test-01"
		accountID   = "ca-scope-test-01"
		principalID = "principal-test-01"
	)

	// Seed commercial account belonging to orgID.
	st.SeedAccount(accountID, orgID)

	// Seed catalog and plan.
	_, planID := seedLegacyPlan(t, h, "2026-Q1-scope", "GROWTH", 100, true)
	_, targetPlanID := seedLegacyPlan(t, h, "2026-Q2-scope", "PRO", 200, false)

	// Grant all 7 operations to principalID on orgID.
	az.grant(principalID, orgID, SubscriptionCreate)
	az.grant(principalID, orgID, EvaluationProgramCreate)
	az.grant(principalID, orgID, OverlayCreate)
	az.grant(principalID, orgID, SubscriptionChangePreview)
	az.grant(principalID, orgID, SubscriptionChangeConfirm)
	az.grant(principalID, orgID, SubscriptionStatusSet)
	az.grant(principalID, orgID, BillingSourceTransferSet)

	// 1. CreateSubscription (POST /v1/subscriptions)
	wSub := httptest.NewRecorder()
	r.ServeHTTP(wSub, buildRequestWithHeaders(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: accountID,
		PlanID:              planID,
		StartAsEvaluation:   true,
	}, orgID, principalID))
	if wSub.Code != http.StatusCreated {
		t.Fatalf("CreateSubscription failed: %d - %s", wSub.Code, wSub.Body.String())
	}
	last := az.lastCall()
	if last.LegalEntityID != orgID {
		t.Errorf("CreateSubscription authorized against %q, want organization_id %q", last.LegalEntityID, orgID)
	}
	if last.LegalEntityID == accountID {
		t.Errorf("CreateSubscription incorrectly authorized against commercial_account_id %q", accountID)
	}
	if last.ActionType != SubscriptionCreate {
		t.Errorf("expected action %q, got %q", SubscriptionCreate, last.ActionType)
	}

	var sub domain.CommercialSubscription
	_ = json.NewDecoder(wSub.Body).Decode(&sub)

	// 2. CreateEvaluationProgram (POST /v1/subscriptions/{id}/evaluation-program)
	wEval := httptest.NewRecorder()
	r.ServeHTTP(wEval, buildRequestWithHeaders(http.MethodPost, "/v1/subscriptions/"+sub.SubscriptionID+"/evaluation-program", domain.CreateEvaluationProgramRequest{
		DurationDays:     14,
		PaymentRequired:  false,
		ConversionPolicy: "MANUAL",
		ExpiryAction:     "SUSPEND",
	}, orgID, principalID))
	if wEval.Code != http.StatusCreated {
		t.Fatalf("CreateEvaluationProgram failed: %d - %s", wEval.Code, wEval.Body.String())
	}
	last = az.lastCall()
	if last.LegalEntityID != orgID {
		t.Errorf("CreateEvaluationProgram authorized against %q, want organization_id %q", last.LegalEntityID, orgID)
	}
	if last.LegalEntityID == accountID {
		t.Errorf("CreateEvaluationProgram incorrectly authorized against commercial_account_id %q", accountID)
	}
	if last.ActionType != EvaluationProgramCreate {
		t.Errorf("expected action %q, got %q", EvaluationProgramCreate, last.ActionType)
	}

	// 3. CreateOverlay (POST /v1/contract-entitlement-overlays)
	wOverlay := httptest.NewRecorder()
	r.ServeHTTP(wOverlay, buildRequestWithHeaders(http.MethodPost, "/v1/contract-entitlement-overlays", domain.CreateOverlayRequest{
		CommercialAccountID:   accountID,
		MetricType:            "USERS",
		OverrideLimitValue:    int64Ptr(999),
		ApprovedByPrincipalID: principalID,
		EffectiveFrom:         "2020-01-01T00:00:00Z",
	}, orgID, principalID))
	if wOverlay.Code != http.StatusCreated {
		t.Fatalf("CreateOverlay failed: %d - %s", wOverlay.Code, wOverlay.Body.String())
	}
	last = az.lastCall()
	if last.LegalEntityID != orgID {
		t.Errorf("CreateOverlay authorized against %q, want organization_id %q", last.LegalEntityID, orgID)
	}
	if last.LegalEntityID == accountID {
		t.Errorf("CreateOverlay incorrectly authorized against commercial_account_id %q", accountID)
	}
	if last.ActionType != OverlayCreate {
		t.Errorf("expected action %q, got %q", OverlayCreate, last.ActionType)
	}

	// 4. PreviewSubscriptionChange (POST /v1/subscription-change-requests)
	wPreview := httptest.NewRecorder()
	r.ServeHTTP(wPreview, buildRequestWithHeaders(http.MethodPost, "/v1/subscription-change-requests", domain.PreviewChangeRequest{
		SubscriptionID: sub.SubscriptionID,
		TargetPlanID:   targetPlanID,
	}, orgID, principalID))
	if wPreview.Code != http.StatusCreated {
		t.Fatalf("PreviewSubscriptionChange failed: %d - %s", wPreview.Code, wPreview.Body.String())
	}
	last = az.lastCall()
	if last.LegalEntityID != orgID {
		t.Errorf("PreviewSubscriptionChange authorized against %q, want organization_id %q", last.LegalEntityID, orgID)
	}
	if last.LegalEntityID == accountID {
		t.Errorf("PreviewSubscriptionChange incorrectly authorized against commercial_account_id %q", accountID)
	}
	if last.ActionType != SubscriptionChangePreview {
		t.Errorf("expected action %q, got %q", SubscriptionChangePreview, last.ActionType)
	}

	var change domain.SubscriptionChangeRequest
	_ = json.NewDecoder(wPreview.Body).Decode(&change)

	// 5. ConfirmSubscriptionChange (POST /v1/subscription-change-requests/{id}/confirm)
	wConfirm := httptest.NewRecorder()
	r.ServeHTTP(wConfirm, buildRequestWithHeaders(http.MethodPost, "/v1/subscription-change-requests/"+change.ChangeRequestID+"/confirm", nil, orgID, principalID))
	if wConfirm.Code != http.StatusOK {
		t.Fatalf("ConfirmSubscriptionChange failed: %d - %s", wConfirm.Code, wConfirm.Body.String())
	}
	last = az.lastCall()
	if last.LegalEntityID != orgID {
		t.Errorf("ConfirmSubscriptionChange authorized against %q, want organization_id %q", last.LegalEntityID, orgID)
	}
	if last.LegalEntityID == accountID {
		t.Errorf("ConfirmSubscriptionChange incorrectly authorized against commercial_account_id %q", accountID)
	}
	if last.ActionType != SubscriptionChangeConfirm {
		t.Errorf("expected action %q, got %q", SubscriptionChangeConfirm, last.ActionType)
	}

	// 6. SetSubscriptionStatus (POST /v1/subscriptions/{id}/status)
	wStatus := httptest.NewRecorder()
	r.ServeHTTP(wStatus, buildRequestWithHeaders(http.MethodPost, "/v1/subscriptions/"+sub.SubscriptionID+"/status", domain.SetSubscriptionStatusRequest{
		NewStatus: "ACTIVE",
		Reason:    "trial conversion",
	}, orgID, principalID))
	if wStatus.Code != http.StatusOK {
		t.Fatalf("SetSubscriptionStatus failed: %d - %s", wStatus.Code, wStatus.Body.String())
	}
	last = az.lastCall()
	if last.LegalEntityID != orgID {
		t.Errorf("SetSubscriptionStatus authorized against %q, want organization_id %q", last.LegalEntityID, orgID)
	}
	if last.LegalEntityID == accountID {
		t.Errorf("SetSubscriptionStatus incorrectly authorized against commercial_account_id %q", accountID)
	}
	if last.ActionType != SubscriptionStatusSet {
		t.Errorf("expected action %q, got %q", SubscriptionStatusSet, last.ActionType)
	}

	// 7. TransferBillingSource (POST /v1/billing-source-transfers)
	wTransfer := httptest.NewRecorder()
	r.ServeHTTP(wTransfer, buildRequestWithHeaders(http.MethodPost, "/v1/billing-source-transfers", domain.TransferBillingSourceRequest{
		CommercialAccountID: accountID,
		OldSubscriptionID:   sub.SubscriptionID,
		NewBillingSource:    "ZOIKO_ONE_BUNDLE",
	}, orgID, principalID))
	if wTransfer.Code != http.StatusCreated {
		t.Fatalf("TransferBillingSource failed: %d - %s", wTransfer.Code, wTransfer.Body.String())
	}
	last = az.lastCall()
	if last.LegalEntityID != orgID {
		t.Errorf("TransferBillingSource authorized against %q, want organization_id %q", last.LegalEntityID, orgID)
	}
	if last.LegalEntityID == accountID {
		t.Errorf("TransferBillingSource incorrectly authorized against commercial_account_id %q", accountID)
	}
	if last.ActionType != BillingSourceTransferSet {
		t.Errorf("expected action %q, got %q", BillingSourceTransferSet, last.ActionType)
	}
}

// TestSubscriptionAuthorization_SameOrgGrantSucceeds_And_MissingGrantDenied tests that:
// (a) when the principal has the grant for their organization scope, the request succeeds.
// (b) when the principal lacks the grant for their organization scope, the request is denied (403).
func TestSubscriptionAuthorization_SameOrgGrantSucceeds_And_MissingGrantDenied(t *testing.T) {
	az := newRecordingAuthz()
	h, st := newTestHandlerWithAuthz(az)
	r := newSubscriptionTestRouter(h)

	const (
		orgID       = "org-granted-01"
		accountID   = "ca-granted-01"
		principalID = "principal-user-01"
	)

	st.SeedAccount(accountID, orgID)
	_, planID := seedLegacyPlan(t, h, "2026-Q1-grant", "GROWTH", 100, true)

	// Step 1: Request with NO grant -> Expect 403 Forbidden.
	wNoGrant := httptest.NewRecorder()
	r.ServeHTTP(wNoGrant, buildRequestWithHeaders(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: accountID,
		PlanID:              planID,
	}, orgID, principalID))
	if wNoGrant.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when principal lacks grant, got %d - %s", wNoGrant.Code, wNoGrant.Body.String())
	}

	// Step 2: Grant permission to principalID on orgID -> Expect 201 Created.
	az.grant(principalID, orgID, SubscriptionCreate)

	wGranted := httptest.NewRecorder()
	r.ServeHTTP(wGranted, buildRequestWithHeaders(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: accountID,
		PlanID:              planID,
	}, orgID, principalID))
	if wGranted.Code != http.StatusCreated {
		t.Fatalf("expected 201 after grant, got %d - %s", wGranted.Code, wGranted.Body.String())
	}
}

// TestSubscriptionAuthorization_CrossOrgDenied verifies tenant isolation and fail-closed security:
// When an authenticated principal in Organization B attempts to perform an operation on
// an account or subscription belonging to Organization A, the request is refused (404/not found),
// preventing cross-tenant mutation and data leakage.
func TestSubscriptionAuthorization_CrossOrgDenied(t *testing.T) {
	az := newRecordingAuthz()
	h, st := newTestHandlerWithAuthz(az)
	r := newSubscriptionTestRouter(h)

	const (
		orgA       = "11111111-1111-1111-1111-111111111111"
		accountA   = "ca-org-a"
		orgB       = "22222222-2222-2222-2222-222222222222"
		principalB = "principal-org-b"
	)

	// Seed account belonging to Org A.
	st.SeedAccount(accountA, orgA)
	_, planID := seedLegacyPlan(t, h, "2026-Q1-cross", "GROWTH", 100, true)

	// Grant all permissions to principalB for orgB (and even falsely for orgA to prove RLS backstop).
	az.grant(principalB, orgB, SubscriptionCreate)
	az.grant(principalB, orgA, SubscriptionCreate)

	// Principal B (in tenant orgB) attempts to create a subscription on accountA (owned by orgA).
	wCross := httptest.NewRecorder()
	r.ServeHTTP(wCross, buildRequestWithHeaders(http.MethodPost, "/v1/subscriptions", domain.CreateSubscriptionRequest{
		CommercialAccountID: accountA,
		PlanID:              planID,
	}, orgB, principalB))

	// Must be rejected with 404 Not Found (account not found in caller's verified tenant scope).
	if wCross.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-organization account access, got %d - %s", wCross.Code, wCross.Body.String())
	}
	if !strings.Contains(wCross.Body.String(), "commercial account not found") {
		t.Errorf("expected 'commercial account not found', got %s", wCross.Body.String())
	}
}
