package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/counterparty-management-svc/internal/domain"
	"zoiko.io/counterparty-management-svc/internal/events"
)

type stubStore struct {
	counterparties map[string]*domain.Counterparty
}

func newStubStore() *stubStore {
	return &stubStore{
		counterparties: make(map[string]*domain.Counterparty),
	}
}

func (s *stubStore) CreateCounterparty(_ context.Context, c *domain.Counterparty) error {
	if c.CounterpartyID == "" {
		c.CounterpartyID = "cpty-test-001"
	}
	if c.Status == "" {
		c.Status = domain.StatusOnboarding
	}
	if c.ComplianceStatus == "" {
		c.ComplianceStatus = "PENDING"
	}
	s.counterparties[c.CounterpartyID] = c
	return nil
}

func (s *stubStore) GetCounterparty(_ context.Context, id string) (*domain.Counterparty, error) {
	if c, ok := s.counterparties[id]; ok {
		return c, nil
	}
	return nil, domain.ErrCounterpartyNotFound
}

func (s *stubStore) ListCounterparties(_ context.Context, _, _, _ string) ([]domain.Counterparty, error) {
	var out []domain.Counterparty
	for _, c := range s.counterparties {
		out = append(out, *c)
	}
	return out, nil
}

func (s *stubStore) UpdateCounterparty(_ context.Context, c *domain.Counterparty) error {
	s.counterparties[c.CounterpartyID] = c
	return nil
}

func (s *stubStore) UpdateComplianceStatus(_ context.Context, id, complianceStatus string) (*domain.Counterparty, error) {
	c, ok := s.counterparties[id]
	if !ok {
		return nil, domain.ErrCounterpartyNotFound
	}
	c.ComplianceStatus = complianceStatus
	return c, nil
}

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthzClient struct{}

func (s *stubAuthzClient) CheckAllowed(_ context.Context, _, _, _ string) error {
	return nil
}

var _ AuthZClient = (*stubAuthzClient)(nil)

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, &stubAuthzClient{}, logger)
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", "tenant-test-01")
	r.Header.Set("X-Principal-Id", "principal-test-01")
	return r
}

func TestCreateCounterparty(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateCounterpartyRequest{
		LegalEntityID:      "le-001",
		Name:               "Acme Corp Inc",
		CounterpartyType:   domain.TypeVendor,
		RegistrationNumber: "REG-99102",
		TaxID:              "TX-99102",
		JurisdictionID:     "us-delaware",
		ContactEmail:       "contact@acme.com",
		EffectiveFrom:      "2026-01-01",
		// CreatedBy intentionally absent — must be pinned to X-Principal-Id
	}
	w := httptest.NewRecorder()
	h.CreateCounterparty(w, buildRequest(http.MethodPost, "/v1/counterparties", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.Counterparty
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Name != "Acme Corp Inc" {
		t.Errorf("unexpected name: %s", resp.Name)
	}
	if resp.Status != domain.StatusOnboarding {
		t.Errorf("expected ONBOARDING, got %s", resp.Status)
	}
	// created_by must be pinned to the authenticated principal, not the body.
	if resp.CreatedBy != "principal-test-01" {
		t.Errorf("expected created_by=principal-test-01, got %q", resp.CreatedBy)
	}
}

// TestCreateCounterparty_SelfAttributionMismatch verifies that a body-supplied
// created_by that differs from X-Principal-Id is rejected 400.
func TestCreateCounterparty_SelfAttributionMismatch(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateCounterpartyRequest{
		LegalEntityID:    "le-001",
		Name:             "Fake Corp",
		CounterpartyType: domain.TypeVendor,
		JurisdictionID:   "us-delaware",
		CreatedBy:        "some-other-user", // differs from X-Principal-Id
	}
	w := httptest.NewRecorder()
	h.CreateCounterparty(w, buildRequest(http.MethodPost, "/v1/counterparties", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for attribution mismatch, got %d — %s", w.Code, w.Body.String())
	}
}

// TestGetCounterparty_RequiresPrincipal verifies the GET route now enforces auth.
func TestGetCounterparty_RequiresPrincipal(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	req := httptest.NewRequest(http.MethodGet, "/v1/counterparties/any-id", nil)
	req.Header.Set("X-Tenant-Id", "tenant-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when X-Principal-Id absent, got %d — %s", w.Code, w.Body.String())
	}
}

// TestListCounterparties_RequiresPrincipal verifies the LIST route now enforces auth.
func TestListCounterparties_RequiresPrincipal(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	req := httptest.NewRequest(http.MethodGet, "/v1/counterparties", nil)
	req.Header.Set("X-Tenant-Id", "tenant-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when X-Principal-Id absent, got %d — %s", w.Code, w.Body.String())
	}
}

func TestUpdateComplianceStatus(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Create with no CreatedBy in body — pinned to X-Principal-Id
	body := domain.CreateCounterpartyRequest{
		LegalEntityID:    "le-001",
		Name:             "Global Logistics Ltd",
		CounterpartyType: domain.TypeVendor,
		JurisdictionID:   "uk-england",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/counterparties", body))
	var created domain.Counterparty
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Update compliance status to VERIFIED
	compBody := domain.UpdateComplianceStatusRequest{
		ComplianceStatus: "VERIFIED",
		UpdatedBy:        "compliance-officer",
	}
	wComp := httptest.NewRecorder()
	r.ServeHTTP(wComp, buildRequest(http.MethodPost, "/v1/counterparties/"+created.CounterpartyID+"/compliance", compBody))
	if wComp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wComp.Code, wComp.Body.String())
	}
	var updated domain.Counterparty
	_ = json.NewDecoder(wComp.Body).Decode(&updated)
	if updated.ComplianceStatus != "VERIFIED" {
		t.Errorf("expected VERIFIED, got %s", updated.ComplianceStatus)
	}
}
