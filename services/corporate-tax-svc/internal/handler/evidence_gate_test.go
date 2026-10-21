package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/corporate-tax-svc/internal/domain"
	"zoiko.io/corporate-tax-svc/internal/evidencereq"
	svcmiddleware "zoiko.io/corporate-tax-svc/internal/middleware"
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

func evidenceStore() *stubStore {
	st := newStubStore()
	st.returns["v1"] = &domain.TaxReturn{
		ReturnID: "v1", TenantID: "tenant-test-01", LegalEntityID: "le-uk-001", Status: domain.StatusDraft,
	}
	return st
}

func evidenceRouter(t *testing.T, st *stubStore, e EvidenceChecker) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContextMiddleware)
	RegisterRoutes(r, New(st, &stubPublisher{}, newStubAuthzServer(t), zap.NewNop()).WithEvidenceGate(e))
	return r
}

func submitRequest() *http.Request {
	req := buildRequest(http.MethodPost, "/v1/corporate-tax-returns/v1/submit", domain.SubmitTaxReturnRequest{SubmittedBy: "tax-director"})
	req.Header.Set("X-Correlation-ID", "corr-1")
	req.Header.Set("X-Evidence-Refs", "doc-1, doc-2")
	return req
}

func TestSubmitCorporateTaxReturn_EvidenceMissingFailsClosed(t *testing.T) {
	e := &stubEvidence{err: evidencereq.ErrEvidenceMissing}
	st := evidenceStore()

	rr := httptest.NewRecorder()
	evidenceRouter(t, st, e).ServeHTTP(rr, submitRequest())

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
	}
	if st.returns["v1"].Status != domain.StatusDraft {
		t.Fatalf("return must remain DRAFT when evidence is missing, got %s", st.returns["v1"].Status)
	}
	if e.calls != 1 || e.tenantID != "tenant-test-01" || e.legalEntity != "le-uk-001" ||
		e.domainCode != "TAX" || e.actionType != ActionTaxReturnSubmit ||
		e.principalID != "principal-test-01" || e.correlation != "corr-1" {
		t.Fatalf("unexpected gate call: %+v", e)
	}
	if len(e.refs) != 2 || e.refs[0] != "doc-1" || e.refs[1] != "doc-2" {
		t.Fatalf("refs = %v, want [doc-1 doc-2]", e.refs)
	}
}

func TestSubmitCorporateTaxReturn_EvidenceServiceUnavailableFailsClosed(t *testing.T) {
	e := &stubEvidence{err: evidencereq.ErrServiceUnavailable}
	st := evidenceStore()

	rr := httptest.NewRecorder()
	evidenceRouter(t, st, e).ServeHTTP(rr, submitRequest())

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
	if st.returns["v1"].Status != domain.StatusDraft {
		t.Fatalf("return must remain DRAFT when the gate is unavailable, got %s", st.returns["v1"].Status)
	}
}

func TestSubmitCorporateTaxReturn_EvidenceSatisfiedSubmits(t *testing.T) {
	e := &stubEvidence{}
	st := evidenceStore()

	rr := httptest.NewRecorder()
	evidenceRouter(t, st, e).ServeHTTP(rr, submitRequest())

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if st.returns["v1"].Status != domain.StatusSubmitted {
		t.Fatalf("return status = %s, want SUBMITTED", st.returns["v1"].Status)
	}
}
