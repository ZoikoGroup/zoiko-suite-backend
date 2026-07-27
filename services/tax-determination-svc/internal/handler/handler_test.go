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

	"zoiko.io/tax-determination-svc/internal/domain"
	"zoiko.io/tax-determination-svc/internal/events"
	"zoiko.io/tax-determination-svc/internal/rules"
)

type stubStore struct {
	determinations map[string]*domain.TaxDetermination
}

func newStubStore() *stubStore {
	return &stubStore{
		determinations: make(map[string]*domain.TaxDetermination),
	}
}

func (s *stubStore) CreateDetermination(_ context.Context, d *domain.TaxDetermination) error {
	if d.DeterminationID == "" {
		d.DeterminationID = "tdet-test-001"
	}
	if d.Status == "" {
		d.Status = domain.StatusCalculated
	}
	s.determinations[d.DeterminationID] = d
	return nil
}

func (s *stubStore) GetDetermination(_ context.Context, id string) (*domain.TaxDetermination, error) {
	if d, ok := s.determinations[id]; ok {
		return d, nil
	}
	return nil, domain.ErrTaxDeterminationNotFound
}

func (s *stubStore) ListDeterminations(_ context.Context, _, _, _ string) ([]domain.TaxDetermination, error) {
	var out []domain.TaxDetermination
	for _, d := range s.determinations {
		out = append(out, *d)
	}
	return out, nil
}

func (s *stubStore) UpdateDetermination(_ context.Context, d *domain.TaxDetermination) error {
	s.determinations[d.DeterminationID] = d
	return nil
}

func (s *stubStore) OverrideDetermination(_ context.Context, id string, req *domain.OverrideTaxRequest) (*domain.TaxDetermination, error) {
	d, ok := s.determinations[id]
	if !ok {
		return nil, domain.ErrTaxDeterminationNotFound
	}
	if d.Status == domain.StatusOverridden {
		return nil, domain.ErrAlreadyOverridden
	}
	d.Status = domain.StatusOverridden
	d.CalculatedTaxAmount = req.OverriddenTaxAmount
	return d, nil
}

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _, _, _ string, _ interface{}) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	rulesClient := rules.NewClient("http://localhost:8125")
	return New(newStubStore(), &stubPublisher{}, nil, rulesClient, logger)
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

func TestDetermineTax(t *testing.T) {
	h := newTestHandler()
	body := domain.DetermineTaxRequest{
		TransactionID:  "tx-inv-1001",
		SourceModule:   "INVOICE",
		LegalEntityID:  "le-001",
		JurisdictionID: "uk-england",
		TaxCategory:    "VAT",
		GrossAmount:    1000.0,
		ExemptAmount:   0.0,
		Currency:       "GBP",
		EffectiveFrom:  "2026-01-01",
		EvaluatedBy:    "billing-engine",
	}
	w := httptest.NewRecorder()
	h.DetermineTax(w, buildRequest(http.MethodPost, "/v1/tax-determinations", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.TaxDetermination
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.TransactionID != "tx-inv-1001" {
		t.Errorf("unexpected transaction ID: %s", resp.TransactionID)
	}
	if resp.Status != domain.StatusCalculated {
		t.Errorf("expected CALCULATED, got %s", resp.Status)
	}
}

func TestOverrideDetermination(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Determine tax first
	body := domain.DetermineTaxRequest{
		TransactionID:  "tx-po-500",
		SourceModule:   "PURCHASE_ORDER",
		LegalEntityID:  "le-001",
		JurisdictionID: "us-california",
		TaxCategory:    "SALES_TAX",
		GrossAmount:    500.0,
		Currency:       "USD",
		EffectiveFrom:  "2026-01-01",
		EvaluatedBy:    "po-engine",
	}
	wDet := httptest.NewRecorder()
	r.ServeHTTP(wDet, buildRequest(http.MethodPost, "/v1/tax-determinations", body))
	var created domain.TaxDetermination
	_ = json.NewDecoder(wDet.Body).Decode(&created)

	// Override tax
	ovrBody := domain.OverrideTaxRequest{
		OverriddenTaxAmount: 25.0,
		Reason:              "Tax exemption certificate verified manually",
		UpdatedBy:           "tax-auditor",
	}
	wOvr := httptest.NewRecorder()
	r.ServeHTTP(wOvr, buildRequest(http.MethodPost, "/v1/tax-determinations/"+created.DeterminationID+"/override", ovrBody))
	if wOvr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wOvr.Code, wOvr.Body.String())
	}
	var updated domain.TaxDetermination
	_ = json.NewDecoder(wOvr.Body).Decode(&updated)
	if updated.CalculatedTaxAmount != 25.0 {
		t.Errorf("expected tax 25.0, got %f", updated.CalculatedTaxAmount)
	}
	if updated.Status != domain.StatusOverridden {
		t.Errorf("expected OVERRIDDEN, got %s", updated.Status)
	}
}
