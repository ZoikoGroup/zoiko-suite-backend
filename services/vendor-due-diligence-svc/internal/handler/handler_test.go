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

	"zoiko.io/vendor-due-diligence-svc/internal/domain"
	"zoiko.io/vendor-due-diligence-svc/internal/handler"
	"zoiko.io/vendor-due-diligence-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	checks   map[string]*domain.VendorDDCheck // keyed by check_id
	byCorr   map[string]string                // correlation_id -> check_id
	evidence map[string][]domain.VendorDDEvidence
}

func newStubStore() *stubStore {
	return &stubStore{
		checks:   make(map[string]*domain.VendorDDCheck),
		byCorr:   make(map[string]string),
		evidence: make(map[string][]domain.VendorDDEvidence),
	}
}

func (s *stubStore) CreateCheck(_ context.Context, c *domain.VendorDDCheck) (bool, error) {
	if id, ok := s.byCorr[c.CorrelationID]; ok {
		*c = *s.checks[id]
		return false, nil
	}
	s.checks[c.CheckID] = c
	s.byCorr[c.CorrelationID] = c.CheckID
	return true, nil
}

func (s *stubStore) GetCheck(_ context.Context, id string) (*domain.VendorDDCheck, error) {
	c, ok := s.checks[id]
	if !ok {
		return nil, domain.ErrCheckNotFound
	}
	return c, nil
}

func (s *stubStore) ListChecks(_ context.Context, legalEntityID, counterpartyID string) ([]domain.VendorDDCheck, error) {
	var out []domain.VendorDDCheck
	for _, c := range s.checks {
		if legalEntityID != "" && c.LegalEntityID != legalEntityID {
			continue
		}
		if counterpartyID != "" && c.CounterpartyID != counterpartyID {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *stubStore) CompleteCheck(_ context.Context, id, newStatus, riskOutcome, screeningBasis string) error {
	c, ok := s.checks[id]
	if !ok {
		return domain.ErrCheckNotFound
	}
	c.Status = newStatus
	c.RiskOutcome = riskOutcome
	c.ScreeningBasis = screeningBasis
	return nil
}

func (s *stubStore) AddEvidence(_ context.Context, e *domain.VendorDDEvidence) error {
	s.evidence[e.CheckID] = append(s.evidence[e.CheckID], *e)
	return nil
}

func (s *stubStore) ListEvidence(_ context.Context, checkID string) ([]domain.VendorDDEvidence, error) {
	return s.evidence[checkID], nil
}

type stubPublisher struct {
	started, completed, failed int
}

func (p *stubPublisher) PublishStarted(_ context.Context, _ string, _ domain.VendorDDCheck) { p.started++ }
func (p *stubPublisher) PublishCompleted(_ context.Context, _ string, _ domain.VendorDDCheck) {
	p.completed++
}
func (p *stubPublisher) PublishFailed(_ context.Context, _ string, _ domain.VendorDDCheck, _ string) {
	p.failed++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubCounterparty struct {
	complianceCalls, riskCalls int
	failCompliance             bool
}

func (c *stubCounterparty) UpdateComplianceStatus(_ context.Context, _, _, _ string) error {
	c.complianceCalls++
	if c.failCompliance {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *stubCounterparty) UpdateRiskCategory(_ context.Context, _, _, _ string) error {
	c.riskCalls++
	return nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, cp *stubCounterparty) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, cp, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── CreateCheck tests ──────────────────────────────────────────────────────────

func TestCreateCheck_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", map[string]any{
		"counterparty_id": "cp-1",
		"legal_entity_id": "le-us",
		"vendor_name":     "Acme Corp",
		"correlation_id":  "corr-1",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateCheck_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubCounterparty{})
	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", map[string]any{
		"counterparty_id": "cp-1",
		"legal_entity_id": "le-us",
		"vendor_name":     "Acme Corp",
		"correlation_id":  "corr-1",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestCreateCheck_CleanVendor_Allowed(t *testing.T) {
	pub := &stubPublisher{}
	cp := &stubCounterparty{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, cp)

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", map[string]any{
		"counterparty_id": "cp-1",
		"legal_entity_id": "le-us",
		"vendor_name":     "Totally Legitimate Vendor Inc",
		"correlation_id":  "corr-clean",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.CheckDetailResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Check.Status != "COMPLETED" {
		t.Errorf("expected COMPLETED got %q", resp.Check.Status)
	}
	if resp.Check.RiskOutcome != "CLEAR" {
		t.Errorf("expected CLEAR got %q", resp.Check.RiskOutcome)
	}
	if len(resp.Evidence) != 1 {
		t.Errorf("expected 1 evidence entry got %d", len(resp.Evidence))
	}
	if pub.started != 1 || pub.completed != 1 {
		t.Errorf("expected 1 started + 1 completed event, got started=%d completed=%d", pub.started, pub.completed)
	}
	if cp.complianceCalls != 1 {
		t.Errorf("expected 1 compliance-status push got %d", cp.complianceCalls)
	}
	if cp.riskCalls != 0 {
		t.Errorf("expected no risk-category push for a clean vendor, got %d", cp.riskCalls)
	}
}

func TestCreateCheck_DenylistedVendor_Flagged(t *testing.T) {
	pub := &stubPublisher{}
	cp := &stubCounterparty{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, cp)

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", map[string]any{
		"counterparty_id": "cp-2",
		"legal_entity_id": "le-us",
		"vendor_name":     "Acme Sanctioned Holdings",
		"correlation_id":  "corr-flagged",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.CheckDetailResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)

	if resp.Check.RiskOutcome != "FLAGGED" {
		t.Errorf("expected FLAGGED got %q", resp.Check.RiskOutcome)
	}
	if cp.riskCalls != 1 {
		t.Errorf("expected a risk-category push for a flagged vendor, got %d", cp.riskCalls)
	}
}

func TestCreateCheck_CounterpartyPushFails_StillReturnsResult(t *testing.T) {
	pub := &stubPublisher{}
	cp := &stubCounterparty{failCompliance: true}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, cp)

	rr := doReq(r, http.MethodPost, "/v1/vendor-checks/", map[string]any{
		"counterparty_id": "cp-3",
		"legal_entity_id": "le-us",
		"vendor_name":     "Another Clean Vendor",
		"correlation_id":  "corr-cp-down",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("counterparty-management-svc being unreachable must not fail the due diligence result, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.CheckDetailResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Check.Status != "COMPLETED" {
		t.Errorf("expected COMPLETED got %q", resp.Check.Status)
	}
}

func TestCreateCheck_IdempotentReplay(t *testing.T) {
	pub := &stubPublisher{}
	cp := &stubCounterparty{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, cp)

	body := map[string]any{
		"counterparty_id": "cp-4",
		"legal_entity_id": "le-us",
		"vendor_name":     "Repeat Vendor",
		"correlation_id":  "corr-retry",
	}

	rr1 := doReq(r, http.MethodPost, "/v1/vendor-checks/", body, "principal-1")
	var resp1 domain.CheckDetailResponse
	_ = json.NewDecoder(rr1.Body).Decode(&resp1)

	rr2 := doReq(r, http.MethodPost, "/v1/vendor-checks/", body, "principal-1")
	if rr2.Code != http.StatusOK {
		t.Fatalf("expected 200 (replay) got %d: %s", rr2.Code, rr2.Body.String())
	}
	var resp2 domain.CheckDetailResponse
	_ = json.NewDecoder(rr2.Body).Decode(&resp2)

	if resp2.Check.CheckID != resp1.Check.CheckID {
		t.Fatalf("retried check resolved to a different check_id (%s) than the original (%s)", resp2.Check.CheckID, resp1.Check.CheckID)
	}
	// Retry must not re-run screening or re-notify counterparty-management-svc.
	if pub.started != 1 || pub.completed != 1 {
		t.Errorf("expected exactly 1 started + 1 completed event across both requests, got started=%d completed=%d", pub.started, pub.completed)
	}
	if cp.complianceCalls != 1 {
		t.Errorf("expected exactly 1 compliance push across both requests, got %d", cp.complianceCalls)
	}
}

// ── GetCheck / ListChecks tests ────────────────────────────────────────────────

func TestGetCheck_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/does-not-exist", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", rr.Code)
	}
}

func TestListChecks_EmptyIsEmptyArrayNotNull(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubCounterparty{})
	rr := doReq(r, http.MethodGet, "/v1/vendor-checks/", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	if rr.Body.String() != "[]\n" {
		t.Errorf("expected empty JSON array, got %q", rr.Body.String())
	}
}
