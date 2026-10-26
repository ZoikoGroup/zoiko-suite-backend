package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/evidencereq"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

type stubEvidence struct {
	err error

	calls       int
	tenantID    string
	legalEntity string
	domainCode  string
	actionType  string
	correlation string
	principalID string
	refs        []string
}

func (s *stubEvidence) EvaluateSufficient(_ context.Context, tenantID, legalEntityID, domainCode, actionType, correlationID, principalID string, refs []string) error {
	s.calls++
	s.tenantID = tenantID
	s.legalEntity = legalEntityID
	s.domainCode = domainCode
	s.actionType = actionType
	s.correlation = correlationID
	s.principalID = principalID
	s.refs = refs
	return s.err
}

func evidenceRouter(s *stubStore, e handler.EvidenceChecker) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	h := handler.New(s, &stubAuthZ{}, &stubClients{}, testSigningKey, zap.NewNop()).WithEvidenceGate(e)
	handler.RegisterRoutes(r, h)
	return r
}

func lockRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/close/periods/fp-open/lock", nil)
	req.Header.Set("X-Principal-Id", "principal-1")
	req.Header.Set("X-Tenant-Id", testTenantID)
	req.Header.Set("X-Correlation-ID", "corr-1")
	req.Header.Set("X-Evidence-Refs", "doc-1, doc-2")
	return req
}

func TestLockPeriod_EvidenceMissingFailsClosedBeforeTransition(t *testing.T) {
	e := &stubEvidence{err: evidencereq.ErrEvidenceMissing}
	s := gateStore()

	rr := httptest.NewRecorder()
	evidenceRouter(s, e).ServeHTTP(rr, lockRequest())

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
	}
	if s.periods["fp-open"].CloseStatus != "CLOSE_REVIEW" {
		t.Fatalf("period must not transition when evidence is missing, got %s", s.periods["fp-open"].CloseStatus)
	}
	if e.calls != 1 {
		t.Fatalf("evidence calls = %d, want 1", e.calls)
	}
	if e.tenantID != testTenantID || e.legalEntity != "le-1" || e.domainCode != "FINANCE" || e.actionType != "PERIOD_LOCK" {
		t.Fatalf("unexpected scope: tenant=%q entity=%q domain=%q action=%q",
			e.tenantID, e.legalEntity, e.domainCode, e.actionType)
	}
	if e.principalID != "principal-1" || e.correlation != "corr-1" {
		t.Fatalf("principal/correlation = %q/%q", e.principalID, e.correlation)
	}
	if len(e.refs) != 2 || e.refs[0] != "doc-1" || e.refs[1] != "doc-2" {
		t.Fatalf("refs = %v, want [doc-1 doc-2]", e.refs)
	}
}

func TestLockPeriod_EvidenceServiceUnavailableFailsClosed(t *testing.T) {
	e := &stubEvidence{err: evidencereq.ErrServiceUnavailable}
	s := gateStore()

	rr := httptest.NewRecorder()
	evidenceRouter(s, e).ServeHTTP(rr, lockRequest())

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
	if s.periods["fp-open"].CloseStatus != "CLOSE_REVIEW" {
		t.Fatalf("period must not transition when the gate is unavailable, got %s", s.periods["fp-open"].CloseStatus)
	}
}
