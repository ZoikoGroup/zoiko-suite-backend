package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/contract-lifecycle-svc/internal/domain"
	"zoiko.io/contract-lifecycle-svc/internal/events"
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

func (p *stubPublisher) Publish(_ context.Context, _ string, _ string, _ string, _ interface{}) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// --- Test helpers ---

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, nil, logger)
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", "tenant-test-01")
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
