package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-status-svc/internal/authz"
	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/events"
	"zoiko.io/payment-status-svc/internal/handler"
	"zoiko.io/payment-status-svc/internal/middleware"
	"zoiko.io/payment-status-svc/internal/webhook"
)

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct {
	calls     int
	published []events.PublishParams
}

func (p *stubPublisher) Publish(_ context.Context, params events.PublishParams) error {
	p.calls++
	p.published = append(p.published, params)
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

const testTenant = "tenant-bnk07-1"
const testLegalEntity = "le-bnk07-1"
const testSecret = "test-webhook-shared-secret"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, testSecret, logger)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h, middleware.TenantContext())
	return r
}

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, tenantID, "principal-operator")
}

// doRequestAs lets a test act as a principal OTHER than the default
// "principal-operator" — needed for the SoD checks on ResolveStatusConflict/
// RecordReturn, where the resolving principal must differ from whoever
// created the payment.
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

// postWebhook posts a REAL, correctly HMAC-signed callback — the same
// mechanism a real provider would use.
func postWebhook(r http.Handler, payload domain.ProviderCallbackPayload, secret string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/bnk07/webhooks/provider-callback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Signature", webhook.Sign([]byte(secret), body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func recordPayment(t *testing.T, r http.Handler) *domain.PaymentExecutionState {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/bnk07/payments/", domain.RecordPaymentStatusRequest{
		LegalEntityID: testLegalEntity, ProviderRequestID: "bnk06-attempt-1", SourceReference: "run-instruction-1",
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("recordPayment: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var p domain.PaymentExecutionState
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return &p
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestRecordPaymentStatus_Prepared(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)
	if p.Status != domain.StatusPrepared {
		t.Fatalf("expected PREPARED, got %s", p.Status)
	}
}

func TestProcessProviderCallback_ValidSignature_Applied(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)

	w := postWebhook(r, domain.ProviderCallbackPayload{
		PaymentID: p.PaymentID, ProviderEventRef: "evt-1", ReportedStatus: domain.StatusSettled, MappingVersion: "v1",
	}, testSecret)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Payment domain.PaymentExecutionState `json:"payment"`
		Applied bool                         `json:"applied"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Applied || resp.Payment.Status != domain.StatusSettled {
		t.Fatalf("expected applied SETTLED, got %+v", resp)
	}
}

// TestProcessProviderCallback_InvalidSignature_Rejected is negative-path #1.
func TestProcessProviderCallback_InvalidSignature_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)

	w := postWebhook(r, domain.ProviderCallbackPayload{
		PaymentID: p.PaymentID, ProviderEventRef: "evt-2", ReportedStatus: domain.StatusSettled,
	}, "wrong-secret-an-attacker-guessed")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 forged callback rejected, got %d: %s", w.Code, w.Body.String())
	}
}

// TestProcessProviderCallback_Duplicate_NotDoubleApplied is negative-path
// #3.
func TestProcessProviderCallback_Duplicate_NotDoubleApplied(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)

	callback := domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-dup", ReportedStatus: domain.StatusAccepted}
	postWebhook(r, callback, testSecret)

	w := postWebhook(r, callback, testSecret)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Applied bool `json:"applied"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Applied {
		t.Fatalf("expected applied=false for a duplicate provider event")
	}
}

// TestProcessProviderCallback_RegressionBlocked is negative-path #2.
func TestProcessProviderCallback_RegressionBlocked(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)
	postWebhook(r, domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-settle", ReportedStatus: domain.StatusSettled}, testSecret)

	w := postWebhook(r, domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-late-pending", ReportedStatus: domain.StatusPending}, testSecret)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Payment domain.PaymentExecutionState `json:"payment"`
		Applied bool                         `json:"applied"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Applied || resp.Payment.Status != domain.StatusSettled {
		t.Fatalf("expected out-of-order callback blocked, payment still SETTLED, got %+v", resp)
	}
}

// TestLinkStatementConfirmation_Conflict is negative-path #4.
func TestLinkStatementConfirmation_Conflict(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)
	postWebhook(r, domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-settle-2", ReportedStatus: domain.StatusSettled}, testSecret)

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/link-statement",
		domain.LinkStatementRequest{StatementReference: "stmt-ref-1", ReportedStatus: domain.StatusRejected}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Payment        domain.PaymentExecutionState `json:"payment"`
		ConflictRaised bool                         `json:"conflict_raised"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.ConflictRaised || !resp.Payment.HasOpenConflict {
		t.Fatalf("expected a conflict to be raised, got %+v", resp)
	}
}

func TestResolveStatusConflict(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)
	postWebhook(r, domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-settle-3", ReportedStatus: domain.StatusSettled}, testSecret)
	doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/link-statement",
		domain.LinkStatementRequest{StatementReference: "stmt-ref-2", ReportedStatus: domain.StatusRejected}, testTenant)

	// A different principal from the one who created the payment
	// ("principal-operator", via recordPayment/doRequest's default) —
	// the SoD rule this endpoint enforces.
	w := doRequestAs(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/resolve-conflict",
		domain.ResolveConflictRequest{FinalStatus: domain.StatusSettled, Reason: "provider record confirmed correct"}, testTenant, "principal-reviewer")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 resolving, got %d: %s", w.Code, w.Body.String())
	}
	var resolved domain.PaymentExecutionState
	_ = json.Unmarshal(w.Body.Bytes(), &resolved)
	if resolved.HasOpenConflict {
		t.Fatalf("expected conflict cleared")
	}
}

// TestResolveStatusConflict_SamePrincipalAsCreator_Forbidden is the real
// proof of the SoD rule: the principal who created the payment cannot
// also resolve its own conflict, even holding PAYMENT_FINALITY_CONFIRM.
func TestResolveStatusConflict_SamePrincipalAsCreator_Forbidden(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r) // created as "principal-operator"
	postWebhook(r, domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-settle-3b", ReportedStatus: domain.StatusSettled}, testSecret)
	doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/link-statement",
		domain.LinkStatementRequest{StatementReference: "stmt-ref-2b", ReportedStatus: domain.StatusRejected}, testTenant)

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/resolve-conflict",
		domain.ResolveConflictRequest{FinalStatus: domain.StatusSettled, Reason: "trying to resolve my own conflict"}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the creator tries to resolve their own conflict, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPollPaymentStatus_SamePrincipalAsCreator_AssertingSettled_Forbidden
// is the real proof of the doc's own SoD line ("same actor should not
// create payment and manually assert final settlement"): PollPaymentStatus
// is the de facto manual-settlement path (no separate ConfirmSettlement
// command exists), so it must be gated the same way
// ResolveStatusConflict/RecordReturn already are.
func TestPollPaymentStatus_SamePrincipalAsCreator_AssertingSettled_Forbidden(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r) // created as "principal-operator"

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/poll",
		domain.ProviderCallbackPayload{ReportedStatus: domain.StatusSettled}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the creator manually asserts their own payment SETTLED, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPollPaymentStatus_SamePrincipalAsCreator_AssertingRejected_Forbidden
// proves the same guard covers REJECTED, the other governed-final status
// PollPaymentStatus can assert — not just SETTLED specifically.
func TestPollPaymentStatus_SamePrincipalAsCreator_AssertingRejected_Forbidden(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/poll",
		domain.ProviderCallbackPayload{ReportedStatus: domain.StatusRejected}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the creator manually asserts their own payment REJECTED, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPollPaymentStatus_SamePrincipalAsCreator_IntermediateStatus_Allowed
// proves the guard is scoped to finality claims only — reporting
// intermediate progress (ACCEPTED/PENDING) is not "asserting final
// settlement" and remains unrestricted, same scoping as the doc's own
// SoD language.
func TestPollPaymentStatus_SamePrincipalAsCreator_IntermediateStatus_Allowed(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/poll",
		domain.ProviderCallbackPayload{ReportedStatus: domain.StatusAccepted}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 — an intermediate status report is not a finality assertion, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPollPaymentStatus_DifferentPrincipal_AssertingSettled_Succeeds is
// the positive control.
func TestPollPaymentStatus_DifferentPrincipal_AssertingSettled_Succeeds(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)

	w := doRequestAs(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/poll",
		domain.ProviderCallbackPayload{ReportedStatus: domain.StatusSettled}, testTenant, "principal-reviewer")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for a different principal asserting SETTLED, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRecordReturn_FromSettled(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)
	postWebhook(r, domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-settle-4", ReportedStatus: domain.StatusSettled}, testSecret)

	w := doRequestAs(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/return",
		domain.RecordReturnRequest{ProviderEventRef: "evt-return-1", Reason: "customer disputed"}, testTenant, "principal-reviewer")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 returning, got %d: %s", w.Code, w.Body.String())
	}
	var returned domain.PaymentExecutionState
	_ = json.Unmarshal(w.Body.Bytes(), &returned)
	if returned.Status != domain.StatusReturned {
		t.Fatalf("expected RETURNED, got %s", returned.Status)
	}
}

// TestRecordReturn_SamePrincipalAsCreator_Forbidden mirrors the conflict
// SoD check for the return path.
func TestRecordReturn_SamePrincipalAsCreator_Forbidden(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r) // created as "principal-operator"
	postWebhook(r, domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-settle-4b", ReportedStatus: domain.StatusSettled}, testSecret)

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/return",
		domain.RecordReturnRequest{ProviderEventRef: "evt-return-1b", Reason: "trying to return my own payment"}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the creator tries to return their own payment, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRecordReturn_NotSettled_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/return",
		domain.RecordReturnRequest{ProviderEventRef: "evt-return-2", Reason: "customer disputed"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCancelPaymentWhereSupported(t *testing.T) {
	pub := &stubPublisher{}
	r := newTestRouter(newStubStore(), pub, &stubAuthz{})
	p := recordPayment(t, r)

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/cancel", domain.CancelRequest{Reason: "duplicate record"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 cancelling, got %d: %s", w.Code, w.Body.String())
	}

	// Wave 8b: PaymentCancelled was recorded to status_events but never
	// published to the event bus — this is the real proof it now is.
	found := false
	for _, ev := range pub.published {
		if ev.EventType == domain.EventPaymentCancelled {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a PAYMENT_CANCELLED event to be published, got %+v", pub.published)
	}
}

func TestGetPaymentStatus_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	w := doRequest(r, http.MethodGet, "/bnk07/payments/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
