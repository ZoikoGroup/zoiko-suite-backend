package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payable-open-item-svc/internal/authz"
	"zoiko.io/payable-open-item-svc/internal/domain"
	"zoiko.io/payable-open-item-svc/internal/events"
	"zoiko.io/payable-open-item-svc/internal/handler"
	"zoiko.io/payable-open-item-svc/internal/middleware"
)

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ── stub authz ───────────────────────────────────────────────────────────────

type stubAuthz struct{ deny bool }

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-ap08-1"
const testLegalEntity = "le-ap08-1"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, logger)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequestAs(r http.Handler, method, path string, body interface{}, tenantID, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID)
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, tenantID, "principal-operator")
}

func createPayable(t *testing.T, r http.Handler, sourceRef string, amount float64) *domain.PayableOpenItem {
	t.Helper()
	req := domain.CreatePayableRequest{
		LegalEntityID: testLegalEntity, SourceType: domain.SourceExpenseClaim, SourceReference: sourceRef,
		PayeeRef: "payee-" + sourceRef, OriginalAmount: amount, Currency: "USD",
		DueDate: time.Now().UTC().Add(24 * time.Hour),
	}
	w := doRequest(r, http.MethodPost, "/ap08/payables/", req, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createPayable: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var p domain.PayableOpenItem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return &p
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCreatePayableFromApprovedSource(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createPayable(t, r, "claim-1", 500)
	if p.Status != domain.StatusOpen || p.ResidualAmount != 500 {
		t.Fatalf("expected OPEN with residual 500, got %s / %.2f", p.Status, p.ResidualAmount)
	}
}

// TestCreatePayableFromApprovedSource_DuplicateSource_Idempotent is
// negative-path #4 ("AP totals match GL but duplicate/missing open items
// remain") — a repeat call for the same source returns the existing row.
func TestCreatePayableFromApprovedSource_DuplicateSource_Idempotent(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	first := createPayable(t, r, "claim-2", 300)

	req := domain.CreatePayableRequest{
		LegalEntityID: testLegalEntity, SourceType: domain.SourceExpenseClaim, SourceReference: "claim-2",
		PayeeRef: "payee-claim-2", OriginalAmount: 300, Currency: "USD", DueDate: time.Now().UTC().Add(24 * time.Hour),
	}
	w := doRequest(r, http.MethodPost, "/ap08/payables/", req, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 idempotent replay, got %d: %s", w.Code, w.Body.String())
	}
	var second domain.PayableOpenItem
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	if second.PayableID != first.PayableID {
		t.Fatalf("expected the same payable returned, got a different one")
	}
}

func TestApplyConfirmedPayment_PartialThenFull(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createPayable(t, r, "claim-3", 100)

	w := doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/apply-confirmed-payment",
		domain.ApplyConfirmedPaymentRequest{Amount: 60, ProviderPaymentRef: "pay-evt-1"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp1 struct {
		Payable domain.PayableOpenItem `json:"payable"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp1)
	if resp1.Payable.Status != domain.StatusPartiallySettled || resp1.Payable.ResidualAmount != 40 {
		t.Fatalf("expected PARTIALLY_SETTLED residual 40, got %s / %.2f", resp1.Payable.Status, resp1.Payable.ResidualAmount)
	}

	w = doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/apply-confirmed-payment",
		domain.ApplyConfirmedPaymentRequest{Amount: 40, ProviderPaymentRef: "pay-evt-2"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp2 struct {
		Payable domain.PayableOpenItem `json:"payable"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp2)
	if resp2.Payable.Status != domain.StatusSettled || resp2.Payable.ResidualAmount != 0 {
		t.Fatalf("expected SETTLED residual 0, got %s / %.2f", resp2.Payable.Status, resp2.Payable.ResidualAmount)
	}
}

// TestApplyConfirmedPayment_ReplayedRef_Idempotent is negative-path #3
// ("confirmed payment applied twice").
func TestApplyConfirmedPayment_ReplayedRef_Idempotent(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createPayable(t, r, "claim-4", 100)

	req := domain.ApplyConfirmedPaymentRequest{Amount: 100, ProviderPaymentRef: "pay-evt-dup"}
	doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/apply-confirmed-payment", req, testTenant)

	w := doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/apply-confirmed-payment", req, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Applied bool `json:"applied"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Applied {
		t.Fatalf("expected applied=false for a replayed payment reference")
	}
}

// TestApplyConfirmedPayment_ExceedsResidual_Blocked verifies the residual
// cannot go negative through a payment (only a supplier credit may).
func TestApplyConfirmedPayment_ExceedsResidual_Blocked(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createPayable(t, r, "claim-5", 100)

	w := doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/apply-confirmed-payment",
		domain.ApplyConfirmedPaymentRequest{Amount: 150, ProviderPaymentRef: "pay-evt-over"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 residual would go negative, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApplySupplierCredit_CanTakeResidualNegative(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createPayable(t, r, "claim-6", 100)

	w := doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/apply-supplier-credit",
		domain.ApplySupplierCreditRequest{Amount: 150, CreditRef: "credit-1", Reason: "overpayment credit"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var updated domain.PayableOpenItem
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.ResidualAmount != -50 || updated.Status != domain.StatusSettled {
		t.Fatalf("expected residual -50 and SETTLED, got %.2f / %s", updated.ResidualAmount, updated.Status)
	}
}

// TestGetPaymentEligibility_ExcludesDisputedAndHeld is negative-path #2
// ("disputed payable included as eligible payment").
func TestGetPaymentEligibility_ExcludesDisputedAndHeld(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	eligible := createPayable(t, r, "claim-7", 100)
	disputed := createPayable(t, r, "claim-8", 100)
	held := createPayable(t, r, "claim-9", 100)

	doRequest(r, http.MethodPost, "/ap08/payables/"+disputed.PayableID+"/dispute", domain.OpenDisputeRequest{Reason: "quality issue"}, testTenant)
	doRequest(r, http.MethodPost, "/ap08/payables/"+held.PayableID+"/hold", domain.PlaceHoldRequest{Reason: "under review"}, testTenant)

	w := doRequest(r, http.MethodGet, "/ap08/payment-eligibility?legal_entity_id="+testLegalEntity, nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data []domain.PayableOpenItem `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	found := map[string]bool{}
	for _, p := range resp.Data {
		found[p.PayableID] = true
	}
	if !found[eligible.PayableID] {
		t.Fatalf("expected the undisputed, unheld payable to be eligible")
	}
	if found[disputed.PayableID] {
		t.Fatalf("expected the disputed payable to be excluded from eligibility")
	}
	if found[held.PayableID] {
		t.Fatalf("expected the held payable to be excluded from eligibility")
	}
}

func TestClosePayable_RequiresFullySettledAndNotHeldOrDisputed(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createPayable(t, r, "claim-10", 100)

	w := doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/close", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 not fully settled, got %d: %s", w.Code, w.Body.String())
	}

	doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/apply-confirmed-payment",
		domain.ApplyConfirmedPaymentRequest{Amount: 100, ProviderPaymentRef: "pay-close-1"}, testTenant)

	w = doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/close", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 closing settled payable, got %d: %s", w.Code, w.Body.String())
	}
}

func TestClosePayable_HeldBlocksClose(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createPayable(t, r, "claim-11", 100)
	doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/apply-confirmed-payment",
		domain.ApplyConfirmedPaymentRequest{Amount: 100, ProviderPaymentRef: "pay-close-2"}, testTenant)
	doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/hold", domain.PlaceHoldRequest{Reason: "audit hold"}, testTenant)

	w := doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/close", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 held blocks close, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetSupplierBalance(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	req1 := domain.CreatePayableRequest{
		LegalEntityID: testLegalEntity, SourceType: domain.SourceExpenseClaim, SourceReference: "claim-12",
		PayeeRef: "payee-shared", OriginalAmount: 100, Currency: "USD", DueDate: time.Now().UTC().Add(24 * time.Hour),
	}
	req2 := domain.CreatePayableRequest{
		LegalEntityID: testLegalEntity, SourceType: domain.SourceExpenseClaim, SourceReference: "claim-13",
		PayeeRef: "payee-shared", OriginalAmount: 50, Currency: "USD", DueDate: time.Now().UTC().Add(24 * time.Hour),
	}
	doRequest(r, http.MethodPost, "/ap08/payables/", req1, testTenant)
	doRequest(r, http.MethodPost, "/ap08/payables/", req2, testTenant)

	w := doRequest(r, http.MethodGet, "/ap08/supplier-balance?payee_ref=payee-shared", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OpenBalance float64 `json:"open_balance"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.OpenBalance != 150 {
		t.Fatalf("expected combined open balance 150, got %.2f", resp.OpenBalance)
	}
}

func TestGetPayable_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	w := doRequest(r, http.MethodGet, "/ap08/payables/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestResolvePayableDispute(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createPayable(t, r, "claim-14", 100)
	doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/dispute", domain.OpenDisputeRequest{Reason: "quantity mismatch"}, testTenant)

	w := doRequest(r, http.MethodPost, "/ap08/payables/"+p.PayableID+"/resolve-dispute", domain.ResolveDisputeRequest{Resolution: "confirmed correct"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var updated domain.PayableOpenItem
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.IsDisputed {
		t.Fatalf("expected dispute resolved, still disputed")
	}
}
