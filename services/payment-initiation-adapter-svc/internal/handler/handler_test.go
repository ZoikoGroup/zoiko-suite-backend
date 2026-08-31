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

	authzpkg "zoiko.io/payment-initiation-adapter-svc/internal/authz"
	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/events"
	"zoiko.io/payment-initiation-adapter-svc/internal/handler"
	"zoiko.io/payment-initiation-adapter-svc/internal/middleware"
	"zoiko.io/payment-initiation-adapter-svc/internal/provideradapter"
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
//
// These tests use the REAL provideradapter.StubProviderAdapter (not a
// second, separate test double) — its own documented, deterministic
// behavior IS the thing under test here, same as any other real dependency
// in this service.

const testTenant = "tenant-bnk06-1"
const testLegalEntity = "le-bnk06-1"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz, provider *provideradapter.StubProviderAdapter) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, provider, logger)
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
	return doRequestAs(r, method, path, body, tenantID, "principal-initiator")
}

func newPrepareReq(idempotencyKey, paymentRef string) domain.PrepareAttemptRequest {
	return domain.PrepareAttemptRequest{
		LegalEntityID: testLegalEntity, SourceReference: "run-instruction-1", AuthorizationFingerprint: "fp-abc",
		PayerAccountRef: "payer-acct-1", PayeeRef: "payee-1", Amount: 500, Currency: "USD",
		ExecutionDate: time.Now().UTC().Add(24 * time.Hour), PaymentReference: paymentRef,
		PayerAccountVerified: true, IdempotencyKey: idempotencyKey,
	}
}

func prepareAttempt(t *testing.T, r http.Handler, req domain.PrepareAttemptRequest) *domain.PaymentInitiationAttempt {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", req, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("prepareAttempt: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var a domain.PaymentInitiationAttempt
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	return &a
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestPrepareAttempt_Prepared(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	a := prepareAttempt(t, r, newPrepareReq("idem-1", "invoice-payment"))
	if a.Status != domain.StatusPrepared {
		t.Fatalf("expected PREPARED, got %s", a.Status)
	}
}

func TestPrepareAttempt_NotVerified_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	req := newPrepareReq("idem-2", "invoice-payment")
	req.PayerAccountVerified = false
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", req, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 unverified account, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPrepareAttempt_DuplicateIdempotencyKey_ReturnsExisting is the
// structural fix for "provider timeout triggers new payment ID": a repeat
// prepare with the same key never creates a second row.
func TestPrepareAttempt_DuplicateIdempotencyKey_ReturnsExisting(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	first := prepareAttempt(t, r, newPrepareReq("idem-3", "invoice-payment"))

	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", newPrepareReq("idem-3", "invoice-payment"), testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 idempotent replay, got %d: %s", w.Code, w.Body.String())
	}
	var second domain.PaymentInitiationAttempt
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	if second.AttemptID != first.AttemptID {
		t.Fatalf("expected the SAME attempt ID on a duplicate prepare, got a different one")
	}
}

func TestSubmitAttempt_Submitted(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	a := prepareAttempt(t, r, newPrepareReq("idem-4", "invoice-payment"))

	w := doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/submit", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 submitting, got %d: %s", w.Code, w.Body.String())
	}
	var submitted domain.PaymentInitiationAttempt
	_ = json.Unmarshal(w.Body.Bytes(), &submitted)
	if submitted.Status != domain.StatusSubmitted || submitted.ProviderRequestID == "" {
		t.Fatalf("expected SUBMITTED with a provider_request_id, got %+v", submitted)
	}
}

// TestSubmitAttempt_Timeout_PendingUnknown is the literal enforcement of
// "UNKNOWN is a first-class financial state": a timeout must never become
// Rejected.
func TestSubmitAttempt_Timeout_PendingUnknown(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	a := prepareAttempt(t, r, newPrepareReq("idem-5", "SIMULATE_TIMEOUT-invoice-payment"))

	w := doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/submit", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var updated domain.PaymentInitiationAttempt
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Status != domain.StatusPendingUnknown {
		t.Fatalf("expected PENDING_UNKNOWN on timeout, got %s", updated.Status)
	}
}

func TestSubmitAttempt_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	a := prepareAttempt(t, r, newPrepareReq("idem-6", "SIMULATE_REJECT-invoice-payment"))

	w := doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/submit", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var updated domain.PaymentInitiationAttempt
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Status != domain.StatusRejectedBeforeSubmission || updated.RejectionReason == "" {
		t.Fatalf("expected REJECTED_BEFORE_SUBMISSION with a reason, got %+v", updated)
	}
}

// TestRetrySameAttempt_ReusesSameAttemptID is the structural fix for
// "provider timeout triggers new payment ID."
func TestRetrySameAttempt_ReusesSameAttemptID(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	a := prepareAttempt(t, r, newPrepareReq("idem-7", "SIMULATE_TIMEOUT-invoice-payment"))
	doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/submit", nil, testTenant)

	w := doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/retry", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 retrying, got %d: %s", w.Code, w.Body.String())
	}
	var retried domain.PaymentInitiationAttempt
	_ = json.Unmarshal(w.Body.Bytes(), &retried)
	if retried.AttemptID != a.AttemptID {
		t.Fatalf("expected retry to reuse the SAME attempt ID, got a different one")
	}
}

func TestCancelBeforeSubmission(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	a := prepareAttempt(t, r, newPrepareReq("idem-8", "invoice-payment"))

	w := doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/cancel", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 cancelling, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCancelBeforeSubmission_AlreadySubmitted_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	a := prepareAttempt(t, r, newPrepareReq("idem-9", "invoice-payment"))
	doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/submit", nil, testTenant)

	w := doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/cancel", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 already submitted, got %d: %s", w.Code, w.Body.String())
	}
}

func TestResolveAmbiguousSubmission(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	a := prepareAttempt(t, r, newPrepareReq("idem-10", "SIMULATE_TIMEOUT-invoice-payment"))
	doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/submit", nil, testTenant)

	w := doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/resolve-ambiguous",
		domain.ResolveAmbiguousRequest{ResolvedStatus: domain.StatusSubmitted, Note: "confirmed via bank statement"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 resolving, got %d: %s", w.Code, w.Body.String())
	}
	var resolved domain.PaymentInitiationAttempt
	_ = json.Unmarshal(w.Body.Bytes(), &resolved)
	if resolved.Status != domain.StatusSubmitted {
		t.Fatalf("expected resolved to SUBMITTED, got %s", resolved.Status)
	}
}

func TestQuarantineAttempt(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	a := prepareAttempt(t, r, newPrepareReq("idem-11", "invoice-payment"))

	w := doRequest(r, http.MethodPost, "/bnk06/attempts/"+a.AttemptID+"/quarantine",
		domain.QuarantineRequest{Reason: "payer account flagged for review"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 quarantining, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetPaymentAttempt_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	w := doRequest(r, http.MethodGet, "/bnk06/attempts/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPrepareAttempt_AuthorizationDenied(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{deny: true}, provideradapter.NewStubProviderAdapter())
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", newPrepareReq("idem-12", "invoice-payment"), testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}
