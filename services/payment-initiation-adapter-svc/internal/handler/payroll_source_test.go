package handler_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/payment-initiation-adapter-svc/internal/clients"
	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/handler"
	"zoiko.io/payment-initiation-adapter-svc/internal/middleware"
	"zoiko.io/payment-initiation-adapter-svc/internal/provideradapter"
)

// ── WFP-07: payroll payment instruction fingerprint verification ────────────

type stubPayroll struct{ err error }

func (p *stubPayroll) VerifyInstructionFingerprint(_ context.Context, _, _, _ string) error {
	return p.err
}

func newPayrollRouter(payroll clients.PayrollClient) chi.Router {
	h := handler.New(newStubStore(), &stubAuthz{}, provideradapter.NewStubProviderAdapter(), &stubTreasury{}, &stubAuthorization{}, zap.NewNop())
	if payroll != nil {
		h.WithPayroll(payroll)
	}
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func newPayrollSourceReq(key string) domain.PrepareAttemptRequest {
	req := newPrepareReq(key, "payroll-net-pay")
	req.AuthorizationSource = domain.AuthorizationSourcePayroll
	req.AuthorizationID = "instruction-1"
	req.AuthorizationFingerprint = "sha256:approved-instruction-fingerprint"
	return req
}

func TestPrepareAttempt_PayrollSource(t *testing.T) {
	// verified: prepared, and the source is persisted
	a := prepareAttempt(t, newPayrollRouter(&stubPayroll{}), newPayrollSourceReq("idem-payroll-1"))
	if a.Status != domain.StatusPrepared || a.AuthorizationSource != domain.AuthorizationSourcePayroll {
		t.Fatalf("expected a PREPARED payroll attempt, got %s / %q", a.Status, a.AuthorizationSource)
	}

	// mismatch: 422, nothing prepared
	w := doRequest(newPayrollRouter(&stubPayroll{err: domain.ErrPayrollFingerprintMismatch}), http.MethodPost, "/bnk06/attempts/", newPayrollSourceReq("idem-payroll-2"), testTenant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on a fingerprint mismatch, got %d: %s", w.Code, w.Body.String())
	}

	// payroll-run-svc unreachable: fail closed
	w = doRequest(newPayrollRouter(&stubPayroll{err: clients.ErrPayrollFingerprintUnavailable}), http.MethodPost, "/bnk06/attempts/", newPayrollSourceReq("idem-payroll-3"), testTenant)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when payroll-run-svc is unreachable, got %d: %s", w.Code, w.Body.String())
	}

	// missing fingerprint: 400
	bad := newPayrollSourceReq("idem-payroll-4")
	bad.AuthorizationFingerprint = ""
	if w := doRequest(newPayrollRouter(&stubPayroll{}), http.MethodPost, "/bnk06/attempts/", bad, testTenant); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with a missing fingerprint, got %d", w.Code)
	}

	// a handler that was not given a payroll client does not trust the source
	w = doRequest(newPayrollRouter(nil), http.MethodPost, "/bnk06/attempts/", newPayrollSourceReq("idem-payroll-5"), testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when the payroll source is not enabled, got %d: %s", w.Code, w.Body.String())
	}
}
