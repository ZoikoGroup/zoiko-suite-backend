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
	"zoiko.io/payment-initiation-adapter-svc/internal/clients"
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

// ── stub treasury client ─────────────────────────────────────────────────────
//
// Defaults to "eligible" so every pre-existing test (none of which is
// about BNK-06's treasury-svc verification) keeps working unchanged.
// Tests that specifically exercise the real verification set err.

type stubTreasury struct {
	err            error
	fingerprintErr error
}

func (t *stubTreasury) VerifyPayerAccount(_ context.Context, _, _, _, _ string) error {
	return t.err
}

// VerifyTransferFingerprint (Wave 11b) uses its own independent
// fingerprintErr field rather than reusing err — existing tests only set
// err (for VerifyPayerAccount) and expect the treasury fingerprint check
// to keep passing unchanged.
func (t *stubTreasury) VerifyTransferFingerprint(_ context.Context, _, _, _ string) error {
	return t.fingerprintErr
}

// ── stub authorization client ────────────────────────────────────────────────
//
// Defaults to "matches" so every pre-existing test (none of which sets
// AuthorizationSource to AuthorizationSourcePaymentAuthorization) keeps
// working unchanged — the verification call is skipped entirely for
// those. Tests that specifically exercise Wave 11a's real verification
// set err.

type stubAuthorization struct{ err error }

func (a *stubAuthorization) VerifyFingerprint(_ context.Context, _, _, _ string) error {
	return a.err
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
	return newTestRouterWithTreasury(st, pub, az, provider, &stubTreasury{})
}

func newTestRouterWithTreasury(st *stubStore, pub *stubPublisher, az *stubAuthz, provider *provideradapter.StubProviderAdapter, treasury *stubTreasury) chi.Router {
	return newTestRouterFull(st, pub, az, provider, treasury, &stubAuthorization{})
}

func newTestRouterFull(st *stubStore, pub *stubPublisher, az *stubAuthz, provider *provideradapter.StubProviderAdapter, treasury *stubTreasury, authorization *stubAuthorization) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, provider, treasury, authorization, logger)
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

// TestPrepareAttempt_TreasuryRejectsAccount_Returns422 proves
// PayerAccountVerified alone is no longer sufficient — a caller can set
// it to true, but if treasury-svc's real BNK-01 record says the account
// isn't ACTIVE/ownership-verified, PrepareAttempt must still refuse.
func TestPrepareAttempt_TreasuryRejectsAccount_Returns422(t *testing.T) {
	r := newTestRouterWithTreasury(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter(),
		&stubTreasury{err: clients.ErrPayerAccountNotEligible})
	req := newPrepareReq("idem-treasury-1", "invoice-payment")
	// PayerAccountVerified is still (falsely) asserted true by the caller.
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", req, testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when treasury-svc says the account isn't eligible, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPrepareAttempt_TreasuryUnavailable_FailsClosed proves an
// unreachable treasury-svc blocks the attempt rather than silently
// trusting the caller's flag.
func TestPrepareAttempt_TreasuryUnavailable_FailsClosed(t *testing.T) {
	r := newTestRouterWithTreasury(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter(),
		&stubTreasury{err: clients.ErrPayerAccountUnavailable})
	req := newPrepareReq("idem-treasury-2", "invoice-payment")
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", req, testTenant)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when treasury-svc is unreachable, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPrepareAttempt_TreasuryVerifies_Succeeds is the positive control:
// a real, ACTIVE, ownership-verified account still succeeds.
func TestPrepareAttempt_TreasuryVerifies_Succeeds(t *testing.T) {
	r := newTestRouterWithTreasury(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter(),
		&stubTreasury{})
	a := prepareAttempt(t, r, newPrepareReq("idem-treasury-3", "invoice-payment"))
	if a.Status != domain.StatusPrepared {
		t.Fatalf("expected PREPARED, got %s", a.Status)
	}
}

// ── Wave 11a: authorization fingerprint verification ─────────────────────────

func newAuthorizationSourceReq(idempotencyKey string) domain.PrepareAttemptRequest {
	req := newPrepareReq(idempotencyKey, "invoice-payment")
	req.AuthorizationSource = domain.AuthorizationSourcePaymentAuthorization
	req.AuthorizationID = "auth-1"
	req.AuthorizationFingerprint = "sha256:real-fingerprint"
	return req
}

// TestPrepareAttempt_AuthorizationSourceRequiresFingerprintAndID proves a
// caller declaring AuthorizationSourcePaymentAuthorization cannot omit
// either the fingerprint or the ID it's supposed to verify against — both
// are required together, not silently accepted as an unverified attempt.
func TestPrepareAttempt_AuthorizationSourceRequiresFingerprintAndID(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())

	missingFingerprint := newAuthorizationSourceReq("idem-auth-1")
	missingFingerprint.AuthorizationFingerprint = ""
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", missingFingerprint, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with a missing fingerprint, got %d: %s", w.Code, w.Body.String())
	}

	missingID := newAuthorizationSourceReq("idem-auth-2")
	missingID.AuthorizationID = ""
	w = doRequest(r, http.MethodPost, "/bnk06/attempts/", missingID, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with a missing authorization_id, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPrepareAttempt_AuthorizationFingerprintMismatch_Returns422 is the
// real proof of Wave 11a: a caller-supplied fingerprint that doesn't
// match payment-authorization-svc's live record is rejected, not stored
// and trusted.
func TestPrepareAttempt_AuthorizationFingerprintMismatch_Returns422(t *testing.T) {
	r := newTestRouterFull(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter(),
		&stubTreasury{}, &stubAuthorization{err: domain.ErrAuthorizationFingerprintMismatch})
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", newAuthorizationSourceReq("idem-auth-3"), testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on a fingerprint mismatch, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPrepareAttempt_AuthorizationServiceUnavailable_FailsClosed proves an
// unreachable payment-authorization-svc blocks the attempt rather than
// silently trusting the caller-supplied fingerprint.
func TestPrepareAttempt_AuthorizationServiceUnavailable_FailsClosed(t *testing.T) {
	r := newTestRouterFull(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter(),
		&stubTreasury{}, &stubAuthorization{err: domain.ErrAuthorizationServiceUnavailable})
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", newAuthorizationSourceReq("idem-auth-4"), testTenant)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when payment-authorization-svc is unreachable, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPrepareAttempt_AuthorizationFingerprintVerified_Succeeds is the
// positive control: a real, matching fingerprint still succeeds.
func TestPrepareAttempt_AuthorizationFingerprintVerified_Succeeds(t *testing.T) {
	r := newTestRouterFull(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter(),
		&stubTreasury{}, &stubAuthorization{})
	a := prepareAttempt(t, r, newAuthorizationSourceReq("idem-auth-5"))
	if a.Status != domain.StatusPrepared {
		t.Fatalf("expected PREPARED, got %s", a.Status)
	}
	if a.AuthorizationID != "auth-1" {
		t.Fatalf("expected authorization_id to be persisted, got %q", a.AuthorizationID)
	}
}

// TestPrepareAttempt_EmptySource_SkipsFingerprintCheck proves the one
// remaining carve-out: an attempt with no AuthorizationSource at all is
// exempted from fingerprint verification (backward compatibility with any
// caller that predates this field), not rejected. As of Wave 11b, both of
// this service's actual integrated callers (payment-run-svc for AP-10,
// treasury-svc for BNK-09) now declare a real source, so this path is no
// longer treasury-svc's shape — see the AuthorizationSourceTreasury tests
// below for that.
func TestPrepareAttempt_EmptySource_SkipsFingerprintCheck(t *testing.T) {
	r := newTestRouterFull(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter(),
		&stubTreasury{}, &stubAuthorization{err: domain.ErrAuthorizationFingerprintMismatch})
	req := newPrepareReq("idem-auth-6", "invoice-payment")
	req.AuthorizationFingerprint = ""
	a := prepareAttempt(t, r, req)
	if a.Status != domain.StatusPrepared {
		t.Fatalf("expected PREPARED (fingerprint check skipped for an empty source), got %s", a.Status)
	}
}

// TestPrepareAttempt_UnrecognizedSource_Rejected proves a non-empty,
// non-recognized AuthorizationSource is refused (400) rather than
// silently trusted like the empty case above.
func TestPrepareAttempt_UnrecognizedSource_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())
	req := newPrepareReq("idem-auth-7", "invoice-payment")
	req.AuthorizationSource = "SOME_OTHER_SERVICE"
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", req, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unrecognized authorization_source, got %d: %s", w.Code, w.Body.String())
	}
}

// ── Wave 11b: BNK-09 treasury transfer fingerprint verification ─────────────

func newTreasurySourceReq(idempotencyKey string) domain.PrepareAttemptRequest {
	req := newPrepareReq(idempotencyKey, "treasury-transfer-payment")
	req.AuthorizationSource = domain.AuthorizationSourceTreasury
	req.AuthorizationID = "transfer-1"
	req.AuthorizationFingerprint = "sha256:real-transfer-fingerprint"
	return req
}

// TestPrepareAttempt_TreasurySourceRequiresFingerprintAndID mirrors
// TestPrepareAttempt_AuthorizationSourceRequiresFingerprintAndID for the
// treasury path.
func TestPrepareAttempt_TreasurySourceRequiresFingerprintAndID(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter())

	missingFingerprint := newTreasurySourceReq("idem-treasury-fp-1")
	missingFingerprint.AuthorizationFingerprint = ""
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", missingFingerprint, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with a missing fingerprint, got %d: %s", w.Code, w.Body.String())
	}

	missingID := newTreasurySourceReq("idem-treasury-fp-2")
	missingID.AuthorizationID = ""
	w = doRequest(r, http.MethodPost, "/bnk06/attempts/", missingID, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with a missing authorization_id, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPrepareAttempt_TreasuryFingerprintMismatch_Returns422 is the real
// proof of Wave 11b: a caller-supplied fingerprint that doesn't match
// treasury-svc's live GetTreasuryTransferFingerprint record is rejected,
// not stored and trusted — closing the exact carve-out Wave 11a left open.
func TestPrepareAttempt_TreasuryFingerprintMismatch_Returns422(t *testing.T) {
	r := newTestRouterFull(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter(),
		&stubTreasury{fingerprintErr: domain.ErrTreasuryFingerprintMismatch}, &stubAuthorization{})
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", newTreasurySourceReq("idem-treasury-fp-3"), testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on a treasury fingerprint mismatch, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPrepareAttempt_TreasuryFingerprintServiceUnavailable_FailsClosed
// proves an unreachable treasury-svc blocks the attempt rather than
// silently trusting the caller-supplied fingerprint.
func TestPrepareAttempt_TreasuryFingerprintServiceUnavailable_FailsClosed(t *testing.T) {
	r := newTestRouterFull(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter(),
		&stubTreasury{fingerprintErr: clients.ErrTransferFingerprintUnavailable}, &stubAuthorization{})
	w := doRequest(r, http.MethodPost, "/bnk06/attempts/", newTreasurySourceReq("idem-treasury-fp-4"), testTenant)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when treasury-svc is unreachable, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPrepareAttempt_TreasuryFingerprintVerified_Succeeds is the positive
// control: a real, matching treasury transfer fingerprint still succeeds.
func TestPrepareAttempt_TreasuryFingerprintVerified_Succeeds(t *testing.T) {
	r := newTestRouterFull(newStubStore(), &stubPublisher{}, &stubAuthz{}, provideradapter.NewStubProviderAdapter(),
		&stubTreasury{}, &stubAuthorization{})
	a := prepareAttempt(t, r, newTreasurySourceReq("idem-treasury-fp-5"))
	if a.Status != domain.StatusPrepared {
		t.Fatalf("expected PREPARED, got %s", a.Status)
	}
	if a.AuthorizationSource != domain.AuthorizationSourceTreasury {
		t.Fatalf("expected authorization_source to be persisted as %q, got %q", domain.AuthorizationSourceTreasury, a.AuthorizationSource)
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
