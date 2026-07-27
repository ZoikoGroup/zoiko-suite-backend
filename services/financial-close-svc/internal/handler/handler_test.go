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

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	periods   map[string]*domain.FiscalPeriod
	createErr error
	getErr    error
	lockErr   error
}

func newStubStore() *stubStore {
	return &stubStore{periods: make(map[string]*domain.FiscalPeriod)}
}

func (s *stubStore) CreateFiscalPeriod(_ context.Context, fp *domain.FiscalPeriod) (bool, error) {
	if s.createErr != nil {
		return false, s.createErr
	}
	for _, existing := range s.periods {
		if existing.LegalEntityID == fp.LegalEntityID && existing.PeriodName == fp.PeriodName {
			*fp = *existing
			return false, nil
		}
	}
	s.periods[fp.FiscalPeriodID] = fp
	return true, nil
}

func (s *stubStore) GetFiscalPeriod(_ context.Context, id string) (*domain.FiscalPeriod, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	fp, ok := s.periods[id]
	if !ok {
		return nil, domain.ErrFiscalPeriodNotFound
	}
	return fp, nil
}

func (s *stubStore) GetFiscalPeriodByName(_ context.Context, legalEntityID, name string) (*domain.FiscalPeriod, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	for _, fp := range s.periods {
		if fp.LegalEntityID == legalEntityID && fp.PeriodName == name {
			return fp, nil
		}
	}
	return nil, domain.ErrFiscalPeriodNotFound
}

func (s *stubStore) ListFiscalPeriods(_ context.Context, legalEntityID string) ([]domain.FiscalPeriod, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	var out []domain.FiscalPeriod
	for _, fp := range s.periods {
		if fp.LegalEntityID == legalEntityID {
			out = append(out, *fp)
		}
	}
	return out, nil
}

func (s *stubStore) LockFiscalPeriod(_ context.Context, id string, lockedAt time.Time, evidenceDocID string) error {
	if s.lockErr != nil {
		return s.lockErr
	}
	fp, ok := s.periods[id]
	if !ok {
		return domain.ErrFiscalPeriodNotFound
	}
	if fp.CloseStatus != "OPEN" {
		return domain.ErrPeriodAlreadyLocked
	}
	fp.CloseStatus = "LOCKED"
	t := lockedAt
	fp.CloseLockedAt = &t
	doc := evidenceDocID
	fp.EvidenceDocumentID = &doc
	return nil
}

func (s *stubStore) CreateCloseEvidence(_ context.Context, _ *domain.CloseEvidence) error {
	return nil
}

type stubPublisher struct{ started, blocked, closed int }

func (p *stubPublisher) PublishCloseStarted(_ context.Context, _ string, _ domain.FiscalPeriod) {
	p.started++
}
func (p *stubPublisher) PublishCloseBlocked(_ context.Context, _ string, _ domain.FiscalPeriod, _ []string) {
	p.blocked++
}
func (p *stubPublisher) PublishClosed(_ context.Context, _ string, _ domain.FiscalPeriod, _ string) {
	p.closed++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubClients struct {
	unpostedCount int
	unpostedErr   error
	unsettledAP   int
	apErr         error
	unsettledAR   int
	arErr         error
	uploadErr     error
	trialBalances map[string]float64
	trialBalErr   error
}

func (c *stubClients) GetUnpostedJournalsCount(_ context.Context, _, _, _ string) (int, error) {
	return c.unpostedCount, c.unpostedErr
}
func (c *stubClients) CompileTrialBalance(_ context.Context, _, _, _ string) (map[string]float64, error) {
	if c.trialBalErr != nil {
		return nil, c.trialBalErr
	}
	if c.trialBalances != nil {
		return c.trialBalances, nil
	}
	return map[string]float64{"1000-Cash": 10000.00}, nil
}
func (c *stubClients) GetUnsettledAPInvoicesCount(_ context.Context, _, _ string) (int, error) {
	return c.unsettledAP, c.apErr
}
func (c *stubClients) GetUnsettledARInvoicesCount(_ context.Context, _, _ string) (int, error) {
	return c.unsettledAR, c.arErr
}
func (c *stubClients) UploadCloseEvidence(_ context.Context, _, _, _ string, _ map[string]float64, _ string) (string, error) {
	if c.uploadErr != nil {
		return "", c.uploadErr
	}
	return "doc-evidence-uuid-001", nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, cl *stubClients) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, cl, zap.NewNop())
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

// ── CreateFiscalPeriod tests ──────────────────────────────────────────────────

func TestCreateFiscalPeriod_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/", map[string]any{
		"legal_entity_id": "le-1",
		"period_name":     "2024-Q1",
		"period_start":    "2024-01-01T00:00:00Z",
		"period_end":      "2024-03-31T23:59:59Z",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateFiscalPeriod_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/", map[string]any{
		"legal_entity_id": "le-1",
		"period_name":     "2024-Q1",
		"period_start":    "2024-01-01T00:00:00Z",
		"period_end":      "2024-03-31T23:59:59Z",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestCreateFiscalPeriod_MissingFields(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/", map[string]any{
		"legal_entity_id": "le-1",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestCreateFiscalPeriod_HappyPath(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/", map[string]any{
		"legal_entity_id": "le-1",
		"period_name":     "2024-Q1",
		"period_start":    "2024-01-01T00:00:00Z",
		"period_end":      "2024-03-31T23:59:59Z",
	}, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var fp domain.FiscalPeriod
	if err := json.NewDecoder(rr.Body).Decode(&fp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fp.CloseStatus != "OPEN" {
		t.Errorf("expected OPEN got %q", fp.CloseStatus)
	}
	if fp.TenantID != "tenant-abc" {
		t.Errorf("tenant isolation: expected tenant-abc got %q", fp.TenantID)
	}
}

func TestCreateFiscalPeriod_Retried_ReturnsOriginalNotDuplicate(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	body := map[string]any{
		"legal_entity_id": "le-1",
		"period_name":     "2024-Q1",
		"period_start":    "2024-01-01T00:00:00Z",
		"period_end":      "2024-03-31T23:59:59Z",
	}

	first := doReq(r, http.MethodPost, "/v1/close/periods/", body, "principal-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}
	var firstFP domain.FiscalPeriod
	_ = json.NewDecoder(first.Body).Decode(&firstFP)

	retry := doReq(r, http.MethodPost, "/v1/close/periods/", body, "principal-1")
	if retry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retried call for the same (legal_entity_id, period_name), got %d: %s", retry.Code, retry.Body.String())
	}
	var retryFP domain.FiscalPeriod
	_ = json.NewDecoder(retry.Body).Decode(&retryFP)
	if retryFP.FiscalPeriodID != firstFP.FiscalPeriodID {
		t.Fatalf("retried call resolved to a different fiscal_period_id (%s) than the original (%s)", retryFP.FiscalPeriodID, firstFP.FiscalPeriodID)
	}
	if len(s.periods) != 1 {
		t.Fatalf("expected exactly 1 fiscal period to exist, got %d — a retry must not create a duplicate", len(s.periods))
	}
}

// ── GetPeriodStatus tests ─────────────────────────────────────────────────────

func TestGetPeriodStatus_MissingParams(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/close/periods/status", nil, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestGetPeriodStatus_NotFound_DefaultsOpen(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/close/periods/status?legal_entity_id=le-1&period_name=2024-Q1", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	var resp map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["close_status"] != "OPEN" {
		t.Errorf("expected OPEN default got %q", resp["close_status"])
	}
}

func TestGetPeriodStatus_LockedPeriod(t *testing.T) {
	s := newStubStore()
	docID := "doc-001"
	s.periods["fp-1"] = &domain.FiscalPeriod{
		FiscalPeriodID:     "fp-1",
		TenantID:           "tenant-abc",
		LegalEntityID:      "le-1",
		PeriodName:         "2024-Q1",
		CloseStatus:        "LOCKED",
		EvidenceDocumentID: &docID,
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodGet, "/v1/close/periods/status?legal_entity_id=le-1&period_name=2024-Q1", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	var resp map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["close_status"] != "LOCKED" {
		t.Errorf("expected LOCKED got %q", resp["close_status"])
	}
}

// ── LockPeriod tests ──────────────────────────────────────────────────────────

func TestLockPeriod_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/nonexistent-id/lock", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", rr.Code)
	}
}

func TestLockPeriod_AlreadyLocked(t *testing.T) {
	s := newStubStore()
	s.periods["fp-locked"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-locked",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "LOCKED",
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-locked/lock", nil, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 got %d", rr.Code)
	}
}

func TestLockPeriod_ReadinessBlocked_UnpostedJournals(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		CloseStatus:    "OPEN",
	}
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubClients{unpostedCount: 3})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (blocked) got %d: %s", rr.Code, rr.Body.String())
	}
	if pub.blocked != 1 {
		t.Errorf("expected 1 CloseBlocked event got %d", pub.blocked)
	}
	var resp domain.ReadinessCheckResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.IsReady {
		t.Error("expected is_ready=false")
	}
	if len(resp.BlockingIssues) == 0 {
		t.Error("expected blocking issues")
	}
}

func TestLockPeriod_HappyPath(t *testing.T) {
	s := newStubStore()
	s.periods["fp-open"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-open",
		TenantID:       "tenant-abc",
		LegalEntityID:  "le-1",
		PeriodName:     "2024-Q1",
		PeriodStart:    time.Now().Add(-30 * 24 * time.Hour),
		PeriodEnd:      time.Now().Add(-1 * time.Hour),
		CloseStatus:    "OPEN",
	}
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-open/lock", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.PeriodLockResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CloseStatus != "LOCKED" {
		t.Errorf("expected LOCKED got %q", resp.CloseStatus)
	}
	if resp.EvidenceDocumentID == "" {
		t.Error("evidence_document_id must be set")
	}
	if resp.VerificationHash == "" {
		t.Error("verification_hash must be set")
	}
	if pub.started != 1 {
		t.Errorf("expected 1 CloseStarted event got %d", pub.started)
	}
	if pub.closed != 1 {
		t.Errorf("expected 1 Closed event got %d", pub.closed)
	}
}
