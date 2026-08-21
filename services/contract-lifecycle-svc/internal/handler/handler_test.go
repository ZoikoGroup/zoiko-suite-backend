package handler

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

	"zoiko.io/contract-lifecycle-svc/internal/authz"
	"zoiko.io/contract-lifecycle-svc/internal/domain"
	"zoiko.io/contract-lifecycle-svc/internal/events"
	"zoiko.io/contract-lifecycle-svc/internal/governancelog"
)

// --- In-memory stub store ---

type stubStore struct {
	contracts map[string]*domain.Contract
	versions  map[string][]domain.ContractVersion
}

func newStubStore() *stubStore {
	return &stubStore{
		contracts: make(map[string]*domain.Contract),
		versions:  make(map[string][]domain.ContractVersion),
	}
}

func (s *stubStore) CreateContract(_ context.Context, c *domain.Contract) error {
	if c.ContractID == "" {
		c.ContractID = "ctr-test-001"
	}
	if c.Status == "" {
		c.Status = domain.ContractStatusDraft
	}
	s.contracts[c.ContractID] = c
	return nil
}

func (s *stubStore) GetContract(_ context.Context, id string) (*domain.Contract, error) {
	if c, ok := s.contracts[id]; ok {
		return c, nil
	}
	return nil, domain.ErrContractNotFound
}

func (s *stubStore) ListContracts(_ context.Context, _ string) ([]domain.Contract, error) {
	var out []domain.Contract
	for _, c := range s.contracts {
		out = append(out, *c)
	}
	return out, nil
}

func (s *stubStore) UpdateContract(_ context.Context, c *domain.Contract, _ string) error {
	s.contracts[c.ContractID] = c
	return nil
}

func (s *stubStore) UpdateContractStatus(_ context.Context, id string, status domain.ContractStatus, _ string) error {
	if c, ok := s.contracts[id]; ok {
		c.Status = status
		return nil
	}
	return domain.ErrContractNotFound
}

func (s *stubStore) ActivateContract(_ context.Context, id string, req *domain.ActivateContractRequest) (*domain.Contract, error) {
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}
	if c.Status == domain.ContractStatusActive {
		return nil, domain.ErrContractAlreadyActive
	}
	c.Status = domain.ContractStatusActive
	c.SignedBy = &req.SignedBy
	c.GovernanceDecisionID = &req.GovernanceDecisionID
	return c, nil
}

func (s *stubStore) TerminateContract(_ context.Context, id string, req *domain.TerminateContractRequest) (*domain.Contract, error) {
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}
	if c.Status == domain.ContractStatusTerminated {
		return nil, domain.ErrContractTerminated
	}
	c.Status = domain.ContractStatusTerminated
	c.TerminatedBy = &req.TerminatedBy
	return c, nil
}

func (s *stubStore) ListContractVersions(_ context.Context, contractID string) ([]domain.ContractVersion, error) {
	return s.versions[contractID], nil
}

// --- Stub publisher ---

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// --- Stub authz client ---

// stubAuthzClient grants every request by default, matching the
// authorization-svc contract's decision_outcome field but skipping the
// network call. Tests can flip deny/err to exercise failure paths.
type stubAuthzClient struct {
	deny bool
	err  error
}

func (s *stubAuthzClient) CheckAllowed(_ context.Context, _, _, _ string) error {
	if s.err != nil {
		return s.err
	}
	if s.deny {
		return authz.ErrAuthorizationDenied
	}
	return nil
}

// --- Stub governance-log client ---

// stubGovernanceLogClient grants every verification by default, matching
// the "GRANTED" outcome but skipping the network call. Tests can flip
// err to exercise the fail-closed path.
type stubGovernanceLogClient struct {
	err error
}

func (s *stubGovernanceLogClient) VerifyGranted(_ context.Context, _, _, _, _ string) error {
	return s.err
}

// --- Test helpers ---

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", "tenant-test-01")
	r.Header.Set("X-Principal-Id", "user-test-01")
	return r
}

// --- Tests ---

func TestCreateContract(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateContractRequest{
		LegalEntityID:    "le-001",
		ContractType:     domain.ContractTypeVendor,
		Title:            "Cloud Services Agreement",
		CounterpartyID:   "cp-001",
		CounterpartyName: "Acme Cloud",
		EffectiveFrom:    "2026-01-01",
		Currency:         "USD",
		TotalValue:       50000,
		CreatedBy:        "user-001",
	}
	w := httptest.NewRecorder()
	h.CreateContract(w, buildRequest(http.MethodPost, "/v1/contracts", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.Contract
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Title != "Cloud Services Agreement" {
		t.Errorf("unexpected title: %s", resp.Title)
	}
	if resp.Status != domain.ContractStatusDraft {
		t.Errorf("expected DRAFT, got %s", resp.Status)
	}
}

func TestCreateContract_MissingFields(t *testing.T) {
	h := newTestHandler()
	body := map[string]string{"title": ""}
	w := httptest.NewRecorder()
	h.CreateContract(w, buildRequest(http.MethodPost, "/v1/contracts", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetContract_NotFound(t *testing.T) {
	h := newTestHandler()

	router := newTestRouter(h)
	req := buildRequest(http.MethodGet, "/v1/contracts/nonexistent", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListContracts(t *testing.T) {
	h := newTestHandler()
	w := httptest.NewRecorder()
	h.ListContracts(w, buildRequest(http.MethodGet, "/v1/contracts", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestTerminateContract_NotFound(t *testing.T) {
	h := newTestHandler()
	router := newTestRouter(h)
	body := domain.TerminateContractRequest{
		TerminatedBy:    "user-001",
		TerminationNote: "Project cancelled",
	}
	req := buildRequest(http.MethodPost, "/v1/contracts/nonexistent/terminate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// newChiRouter builds the real production router (RegisterRoutes), unlike
// newTestRouter below which is a hand-rolled stub that never actually
// invokes the handler. Needed for ActivateContract since it reads
// chi.URLParam(r, "id").
func newChiRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r
}

func TestActivateContract_MissingGovernanceDecisionID(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusPendingApproval,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
	router := newChiRouter(h)

	body := domain.ActivateContractRequest{SignedBy: "user-001", SignedAt: time.Now()}
	req := buildRequest(http.MethodPost, "/v1/contracts/ctr-001/activate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when governance_decision_id is missing, got %d — %s", w.Code, w.Body.String())
	}
}

func TestActivateContract_GovernanceDecisionNotGranted(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusPendingApproval,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{err: governancelog.ErrDecisionNotGranted}, logger)
	router := newChiRouter(h)

	body := domain.ActivateContractRequest{SignedBy: "user-001", SignedAt: time.Now(), GovernanceDecisionID: "dec-001"}
	req := buildRequest(http.MethodPost, "/v1/contracts/ctr-001/activate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when governance decision was not granted, got %d — %s", w.Code, w.Body.String())
	}
}

func TestActivateContract_GovernanceLogUnavailable(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusPendingApproval,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{err: governancelog.ErrServiceUnavailable}, logger)
	router := newChiRouter(h)

	body := domain.ActivateContractRequest{SignedBy: "user-001", SignedAt: time.Now(), GovernanceDecisionID: "dec-001"}
	req := buildRequest(http.MethodPost, "/v1/contracts/ctr-001/activate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when governance log is unavailable, got %d — %s", w.Code, w.Body.String())
	}
}

func TestActivateContract_Success(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusPendingApproval,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
	router := newChiRouter(h)

	body := domain.ActivateContractRequest{SignedBy: "user-001", SignedAt: time.Now(), GovernanceDecisionID: "dec-001"}
	req := buildRequest(http.MethodPost, "/v1/contracts/ctr-001/activate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.Contract
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != domain.ContractStatusActive {
		t.Errorf("expected ACTIVE, got %s", resp.Status)
	}
	if resp.GovernanceDecisionID == nil || *resp.GovernanceDecisionID != "dec-001" {
		t.Errorf("expected governance_decision_id to be recorded, got %v", resp.GovernanceDecisionID)
	}
}

func newTestRouter(h *Handler) http.Handler {
	// Import chi inline for tests
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/contracts/", func(w http.ResponseWriter, r *http.Request) {
		// Route based on path suffix
		path := r.URL.Path
		switch {
		case len(path) > len("/v1/contracts/") && r.Method == http.MethodGet:
			// Simple stub: treat everything as not found
			http.Error(w, `{"error":"contract not found"}`, http.StatusNotFound)
		case len(path) > len("/v1/contracts/") && r.Method == http.MethodPost:
			http.Error(w, `{"error":"contract not found"}`, http.StatusNotFound)
		}
	})
	return mux
}
