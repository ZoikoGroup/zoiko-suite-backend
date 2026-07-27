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

func (p *stubPublisher) Publish(_ context.Context, _, _, _ string, _ interface{}) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

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
}
