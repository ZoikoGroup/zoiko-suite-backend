package handler_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

func gateRouter(s *stubStore, cl *stubClients, enforce bool) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	// These tests isolate the financial-control gate; the subledger gate has
	// its own tests (close_readiness_test.go).
	h := handler.New(s, &stubPublisher{}, &stubAuthZ{}, cl, testSigningKey, zap.NewNop()).
		SetCloseGateEnforced(enforce).SetSubledgerControlGateEnforced(false)
	handler.RegisterRoutes(r, h)
	return r
}

func gateStore() *stubStore {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       testTenantID,
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "CLOSE_REVIEW",
	}
	return s
}

func TestCloseGate_ModeOff_NeverCallsClient(t *testing.T) {
	cl := &stubClients{closeGateErr: domain.ErrFinancialControlUnavailable}
	r := gateRouter(gateStore(), cl, false)
	rr := doReq(r, http.MethodGet, "/v1/close/periods/fp-open/readiness", nil, "principal-1")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"is_ready":true`) {
		t.Fatalf("mode off must ignore the gate, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code == http.StatusServiceUnavailable || rr.Code == http.StatusUnprocessableEntity {
		t.Fatalf("mode off must not block on the gate, got %d: %s", rr.Code, rr.Body.String())
	}
	if cl.closeGateCalls != 0 {
		t.Fatalf("mode off must never call GetCloseGate, got %d calls", cl.closeGateCalls)
	}
}

func TestCloseGate_EnforceOpen_Passes(t *testing.T) {
	cl := &stubClients{closeGate: &domain.CloseGateResponse{Open: true, Configured: true}}
	r := gateRouter(gateStore(), cl, true)
	rr := doReq(r, http.MethodGet, "/v1/close/periods/fp-open/readiness", nil, "principal-1")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"is_ready":true`) {
		t.Fatalf("open gate must be ready, got %d: %s", rr.Code, rr.Body.String())
	}
	if cl.closeGateCalls != 1 {
		t.Fatalf("expected 1 gate call, got %d", cl.closeGateCalls)
	}
}

func TestCloseGate_EnforceBlocked_422(t *testing.T) {
	const issue = "financial_controls: 3 mandatory control(s) not certified"
	cl := &stubClients{closeGate: &domain.CloseGateResponse{Open: false, Configured: true, BlockingCount: 3}}
	r := gateRouter(gateStore(), cl, true)
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), issue) {
		t.Fatalf("expected 422 with control issue, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doReq(r, http.MethodGet, "/v1/close/periods/fp-open/readiness", nil, "principal-1")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"is_ready":false`) || !strings.Contains(rr.Body.String(), issue) {
		t.Fatalf("readiness must reflect the gate, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCloseGate_EnforceUnavailable_503(t *testing.T) {
	s := gateStore()
	cl := &stubClients{closeGateErr: domain.ErrFinancialControlUnavailable}
	r := gateRouter(s, cl, true)
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "financial-control-svc") {
		t.Fatalf("expected 503 fail closed, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.periods["fp-open"].CloseStatus != "CLOSE_REVIEW" {
		t.Fatalf("period must stay OPEN")
	}
	rr = doReq(r, http.MethodGet, "/v1/close/periods/fp-open/readiness", nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness must be 503, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCloseGate_EnforceUnconfigured_422(t *testing.T) {
	cl := &stubClients{closeGate: &domain.CloseGateResponse{Open: false, Configured: false}}
	r := gateRouter(gateStore(), cl, true)
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "no mandatory controls configured for this entity") {
		t.Fatalf("expected 422 unconfigured, got %d: %s", rr.Code, rr.Body.String())
	}
}
