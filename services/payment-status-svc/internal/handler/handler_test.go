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
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-operator")
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

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/resolve-conflict",
		domain.ResolveConflictRequest{FinalStatus: domain.StatusSettled, Reason: "provider record confirmed correct"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 resolving, got %d: %s", w.Code, w.Body.String())
	}
	var resolved domain.PaymentExecutionState
	_ = json.Unmarshal(w.Body.Bytes(), &resolved)
	if resolved.HasOpenConflict {
		t.Fatalf("expected conflict cleared")
	}
}

func TestRecordReturn_FromSettled(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)
	postWebhook(r, domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-settle-4", ReportedStatus: domain.StatusSettled}, testSecret)

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/return",
		domain.RecordReturnRequest{ProviderEventRef: "evt-return-1", Reason: "customer disputed"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 returning, got %d: %s", w.Code, w.Body.String())
	}
	var returned domain.PaymentExecutionState
	_ = json.Unmarshal(w.Body.Bytes(), &returned)
	if returned.Status != domain.StatusReturned {
		t.Fatalf("expected RETURNED, got %s", returned.Status)
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
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := recordPayment(t, r)

	w := doRequest(r, http.MethodPost, "/bnk07/payments/"+p.PaymentID+"/cancel", domain.CancelRequest{Reason: "duplicate record"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 cancelling, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetPaymentStatus_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	w := doRequest(r, http.MethodGet, "/bnk07/payments/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
