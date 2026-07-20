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
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	periods map[string]*domain.FiscalPeriod
	byName  map[string]*domain.FiscalPeriod // key: legalEntityID+"|"+periodName

	createErr error
	lockErr   error
}

func newStubStore() *stubStore {
	return &stubStore{
		periods: map[string]*domain.FiscalPeriod{},
		byName:  map[string]*domain.FiscalPeriod{},
	}
}

func (s *stubStore) CreateFiscalPeriod(_ context.Context, fp *domain.FiscalPeriod) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.periods[fp.FiscalPeriodID] = fp
	s.byName[fp.LegalEntityID+"|"+fp.PeriodName] = fp
	return nil
}

func (s *stubStore) GetFiscalPeriod(_ context.Context, id string) (*domain.FiscalPeriod, error) {
	fp, ok := s.periods[id]
	if !ok {
		return nil, domain.ErrFiscalPeriodNotFound
	}
	return fp, nil
}

func (s *stubStore) GetFiscalPeriodByName(_ context.Context, legalEntityID, name string) (*domain.FiscalPeriod, error) {
	fp, ok := s.byName[legalEntityID+"|"+name]
	if !ok {
		return nil, domain.ErrFiscalPeriodNotFound
	}
	return fp, nil
}

func (s *stubStore) ListFiscalPeriods(_ context.Context, legalEntityID string) ([]domain.FiscalPeriod, error) {
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
	if !ok || fp.CloseStatus != "OPEN" {
		return domain.ErrPeriodAlreadyLocked
	}
	fp.CloseStatus = "LOCKED"
	fp.CloseLockedAt = &lockedAt
	fp.EvidenceDocumentID = &evidenceDocID
	return nil
}

func (s *stubStore) CreateCloseEvidence(_ context.Context, _ *domain.CloseEvidence) error {
	return nil
}

type stubPublisher struct {
	started, blocked, closed int
}

func (p *stubPublisher) PublishCloseStarted(_ context.Context, _ string, _ domain.FiscalPeriod) {
	p.started++
}
func (p *stubPublisher) PublishCloseBlocked(_ context.Context, _ string, _ domain.FiscalPeriod, _ []string) {
	p.blocked++
}
func (p *stubPublisher) PublishClosed(_ context.Context, _ string, _ domain.FiscalPeriod, _ string) {
	p.closed++
}

type stubAuthZ struct {
	err error
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

// stubClients lets each test control every downstream dependency
// independently, including which one fails, without booting real services.
type stubClients struct {
	unpostedJournals int
	unpostedErr      error
	unsettledAP      int
	unsettledAPErr   error
	unsettledAR      int
	unsettledARErr   error
	trialBalance     map[string]float64
	trialBalanceErr  error
	evidenceDocID    string
	uploadErr        error
}

func newStubClients() *stubClients {
	return &stubClients{trialBalance: map[string]float64{"1000-CASH": 5000}, evidenceDocID: "doc-1"}
}

func (c *stubClients) GetUnpostedJournalsCount(_ context.Context, _, _, _ string) (int, error) {
	return c.unpostedJournals, c.unpostedErr
}
func (c *stubClients) CompileTrialBalance(_ context.Context, _, _, _ string) (map[string]float64, error) {
	return c.trialBalance, c.trialBalanceErr
}
func (c *stubClients) GetUnsettledAPInvoicesCount(_ context.Context, _, _ string) (int, error) {
	return c.unsettledAP, c.unsettledAPErr
}
func (c *stubClients) GetUnsettledARInvoicesCount(_ context.Context, _, _ string) (int, error) {
	return c.unsettledAR, c.unsettledARErr
}
func (c *stubClients) UploadCloseEvidence(_ context.Context, _, _, _ string, _ map[string]float64, _ string) (string, error) {
	return c.evidenceDocID, c.uploadErr
}

func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ, c *stubClients) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, p, a, c, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequest(r chi.Router, method, path string, body any, principalID, tenantID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// ── CreateFiscalPeriod ───────────────────────────────────────────────────────

func validCreateReq() domain.PeriodCreateRequest {
	return domain.PeriodCreateRequest{
		LegalEntityID: "e1",
		PeriodName:    "2026-07",
		PeriodStart:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:     time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
	}
}

func TestCreateFiscalPeriod_Success(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubClients())
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/", validCreateReq(), "principal-1", "t1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateFiscalPeriod_MissingPrincipalHeader_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubClients())
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/", validCreateReq(), "", "t1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestCreateFiscalPeriod_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, newStubClients())
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/", validCreateReq(), "principal-1", "t1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestCreateFiscalPeriod_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubClients())
	req := validCreateReq()
	req.PeriodName = ""
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/", req, "principal-1", "t1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// ── ListFiscalPeriods ────────────────────────────────────────────────────────

func TestListFiscalPeriods_RequiresLegalEntityID(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubClients())
	rec := doRequest(r, http.MethodGet, "/v1/close/periods/", nil, "principal-1", "t1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without legal_entity_id, got %d", rec.Code)
	}
}

// ── GetPeriodStatus ──────────────────────────────────────────────────────────

func TestGetPeriodStatus_UnregisteredPeriod_DefaultsToOpen(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubClients())
	rec := doRequest(r, http.MethodGet, "/v1/close/periods/status?legal_entity_id=e1&period_name=2026-08", nil, "", "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["close_status"] != "OPEN" {
		t.Fatalf("expected an unregistered period to default to OPEN, got %q", body["close_status"])
	}
}

// ── LockPeriod ───────────────────────────────────────────────────────────────

func createdPeriod(t *testing.T, s *stubStore, tenantID string) *domain.FiscalPeriod {
	t.Helper()
	fp := &domain.FiscalPeriod{
		FiscalPeriodID: "fp1",
		TenantID:       tenantID,
		LegalEntityID:  "e1",
		PeriodName:     "2026-07",
		CloseStatus:    "OPEN",
	}
	if err := s.CreateFiscalPeriod(context.Background(), fp); err != nil {
		t.Fatalf("seed period: %v", err)
	}
	return fp
}

func TestLockPeriod_AllChecksPass_Succeeds(t *testing.T) {
	s := newStubStore()
	createdPeriod(t, s, "t1")

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, newStubClients())
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/fp1/lock", nil, "principal-1", "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.periods["fp1"].CloseStatus != "LOCKED" {
		t.Fatalf("expected status LOCKED, got %s", s.periods["fp1"].CloseStatus)
	}
	if pub.started != 1 || pub.closed != 1 {
		t.Fatalf("expected close.started and close.closed to each publish once, got started=%d closed=%d", pub.started, pub.closed)
	}
}

func TestLockPeriod_UnpostedJournalsBlockClose(t *testing.T) {
	s := newStubStore()
	createdPeriod(t, s, "t1")

	clients := newStubClients()
	clients.unpostedJournals = 2

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, clients)
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/fp1/lock", nil, "principal-1", "t1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 with unposted journals outstanding, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.periods["fp1"].CloseStatus != "OPEN" {
		t.Fatalf("period must remain OPEN when close is blocked, got %s", s.periods["fp1"].CloseStatus)
	}
	if pub.blocked != 1 {
		t.Fatalf("expected close.blocked to publish once, got %d", pub.blocked)
	}
}

func TestLockPeriod_UnsettledAPBlocksClose(t *testing.T) {
	s := newStubStore()
	createdPeriod(t, s, "t1")

	clients := newStubClients()
	clients.unsettledAP = 1

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, clients)
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/fp1/lock", nil, "principal-1", "t1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 with unsettled AP invoices outstanding, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLockPeriod_UnsettledARBlocksClose(t *testing.T) {
	s := newStubStore()
	createdPeriod(t, s, "t1")

	clients := newStubClients()
	clients.unsettledAR = 1

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, clients)
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/fp1/lock", nil, "principal-1", "t1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 with unsettled AR invoices outstanding, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLockPeriod_GLQueryFails_FailsClosed(t *testing.T) {
	s := newStubStore()
	createdPeriod(t, s, "t1")

	clients := newStubClients()
	clients.unpostedErr = domain.ErrGLServiceUnavailable

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, clients)
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/fp1/lock", nil, "principal-1", "t1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when general-ledger-svc is unreachable (fail closed), got %d: %s", rec.Code, rec.Body.String())
	}
	if s.periods["fp1"].CloseStatus != "OPEN" {
		t.Fatalf("period must remain OPEN when a readiness check couldn't be performed, got %s", s.periods["fp1"].CloseStatus)
	}
}

func TestLockPeriod_TrialBalanceCompileFails_FailsClosed(t *testing.T) {
	s := newStubStore()
	createdPeriod(t, s, "t1")

	clients := newStubClients()
	clients.trialBalanceErr = domain.ErrGLServiceUnavailable

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, clients)
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/fp1/lock", nil, "principal-1", "t1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when trial balance compilation fails, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.periods["fp1"].CloseStatus != "OPEN" {
		t.Fatalf("period must remain OPEN when evidence generation failed, got %s", s.periods["fp1"].CloseStatus)
	}
}

func TestLockPeriod_EvidenceUploadFails_FailsClosed(t *testing.T) {
	s := newStubStore()
	createdPeriod(t, s, "t1")

	clients := newStubClients()
	clients.uploadErr = domain.ErrVaultServiceUnavailable

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, clients)
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/fp1/lock", nil, "principal-1", "t1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when document-vault-svc upload fails, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.periods["fp1"].CloseStatus != "OPEN" {
		t.Fatalf("period must remain OPEN when close evidence couldn't be recorded, got %s", s.periods["fp1"].CloseStatus)
	}
}

func TestLockPeriod_AlreadyLocked_Rejected(t *testing.T) {
	s := newStubStore()
	fp := createdPeriod(t, s, "t1")
	fp.CloseStatus = "LOCKED"

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, newStubClients())
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/fp1/lock", nil, "principal-1", "t1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 re-locking an already-LOCKED period, got %d", rec.Code)
	}
}

func TestLockPeriod_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubClients())
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/does-not-exist/lock", nil, "principal-1", "t1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestLockPeriod_AuthorizationDenied_Returns403(t *testing.T) {
	s := newStubStore()
	createdPeriod(t, s, "t1")

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, newStubClients())
	rec := doRequest(r, http.MethodPost, "/v1/close/periods/fp1/lock", nil, "principal-1", "t1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}
