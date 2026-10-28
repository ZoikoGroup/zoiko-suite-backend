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

	"zoiko.io/tax-rules-svc/internal/domain"
	"zoiko.io/tax-rules-svc/internal/events"
)

type stubStore struct {
	rules map[string]*domain.TaxRule
}

func newStubStore() *stubStore {
	return &stubStore{
		rules: make(map[string]*domain.TaxRule),
	}
}

func (s *stubStore) CreateTaxRule(_ context.Context, r *domain.TaxRule) error {
	if r.RuleID == "" {
		r.RuleID = "trule-test-001"
	}
	if r.Status == "" {
		r.Status = domain.StatusDraft
	}
	s.rules[r.RuleID] = r
	return nil
}

func (s *stubStore) GetTaxRule(_ context.Context, id string) (*domain.TaxRule, error) {
	if r, ok := s.rules[id]; ok {
		return r, nil
	}
	return nil, domain.ErrTaxRuleNotFound
}

func (s *stubStore) ListTaxRules(_ context.Context, _, _, _ string) ([]domain.TaxRule, error) {
	var out []domain.TaxRule
	for _, r := range s.rules {
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) UpdateTaxRule(_ context.Context, r *domain.TaxRule) error {
	s.rules[r.RuleID] = r
	return nil
}

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthz struct{}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	return nil
}

var _ AuthZClient = (*stubAuthz)(nil)

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, &stubAuthz{}, logger)
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

func TestCreateTaxRule(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateTaxRuleRequest{
		JurisdictionID:    "uk-england",
		RuleCode:          "UK-VAT-STD-20",
		Name:              "UK Standard VAT Rate",
		Category:          domain.CategoryVAT,
		TaxRatePercentage: 20.0,
		EffectiveFrom:     "2026-01-01",
		CreatedBy:         "tax-admin-01",
	}
	w := httptest.NewRecorder()
	h.CreateTaxRule(w, buildRequest(http.MethodPost, "/v1/tax-rules", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.TaxRule
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.RuleCode != "UK-VAT-STD-20" {
		t.Errorf("unexpected rule code: %s", resp.RuleCode)
	}
	if resp.TaxRatePercentage != 20.0 {
		t.Errorf("expected rate 20.0, got %f", resp.TaxRatePercentage)
	}
}

func TestUpdateTaxRule(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Create rule first
	body := domain.CreateTaxRuleRequest{
		JurisdictionID:    "us-california",
		RuleCode:          "US-CA-SALES-7.25",
		Name:              "California Sales Tax",
		Category:          domain.CategorySalesTax,
		TaxRatePercentage: 7.25,
		EffectiveFrom:     "2026-01-01",
		CreatedBy:         "tax-admin-01",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/tax-rules", body))
	var created domain.TaxRule
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Update tax rule rate
	newRate := 7.50
	updateBody := domain.UpdateTaxRuleRequest{
		TaxRatePercentage: &newRate,
		Status:            domain.StatusActive,
		UpdatedBy:         "tax-admin-01",
	}
	wUpd := httptest.NewRecorder()
	r.ServeHTTP(wUpd, buildRequest(http.MethodPut, "/v1/tax-rules/"+created.RuleID, updateBody))
	if wUpd.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wUpd.Code, wUpd.Body.String())
	}
	var updated domain.TaxRule
	_ = json.NewDecoder(wUpd.Body).Decode(&updated)
	if updated.TaxRatePercentage != 7.50 {
		t.Errorf("expected rate 7.50, got %f", updated.TaxRatePercentage)
	}
	if updated.Status != domain.StatusActive {
		t.Errorf("expected ACTIVE, got %s", updated.Status)
	}

	// Attempting to update an ACTIVE rule in place must be rejected with 409 Conflict (D2)
	wConflict := httptest.NewRecorder()
	anotherRate := 8.00
	conflictBody := domain.UpdateTaxRuleRequest{
		TaxRatePercentage: &anotherRate,
		UpdatedBy:         "tax-admin-01",
	}
	r.ServeHTTP(wConflict, buildRequest(http.MethodPut, "/v1/tax-rules/"+created.RuleID, conflictBody))
	if wConflict.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when updating ACTIVE rule, got %d — %s", wConflict.Code, wConflict.Body.String())
	}
}

func TestGetAndListTaxRules_Authz(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Create a rule
	body := domain.CreateTaxRuleRequest{
		JurisdictionID:    "uk-england",
		RuleCode:          "UK-VAT-REDUCED-5",
		Name:              "UK Reduced VAT",
		Category:          domain.CategoryVAT,
		TaxRatePercentage: 5.0,
		EffectiveFrom:     "2026-01-01",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/tax-rules", body))
	var created domain.TaxRule
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// GET with principal -> 200 OK
	wGet := httptest.NewRecorder()
	r.ServeHTTP(wGet, buildRequest(http.MethodGet, "/v1/tax-rules/"+created.RuleID, nil))
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wGet.Code)
	}

	// GET without principal -> 401 Unauthorized
	reqNoPrincipal := buildRequest(http.MethodGet, "/v1/tax-rules/"+created.RuleID, nil)
	reqNoPrincipal.Header.Del("X-Principal-Id")
	wGetNoAuth := httptest.NewRecorder()
	r.ServeHTTP(wGetNoAuth, reqNoPrincipal)
	if wGetNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", wGetNoAuth.Code)
	}

	// LIST with principal -> 200 OK
	wList := httptest.NewRecorder()
	r.ServeHTTP(wList, buildRequest(http.MethodGet, "/v1/tax-rules", nil))
	if wList.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wList.Code)
	}

	// Attribution test: created_by should be principal-test-01
	if created.CreatedBy != "principal-test-01" {
		t.Errorf("expected CreatedBy to be pinned to authenticated principal, got %s", created.CreatedBy)
	}
}

