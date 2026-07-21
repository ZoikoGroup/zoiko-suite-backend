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

	"zoiko.io/corporate-tax-svc/internal/domain"
	"zoiko.io/corporate-tax-svc/internal/events"
)

// ─── Stub Store ───────────────────────────────────────────────────────────────

type stubStore struct {
	returns map[string]*domain.TaxReturn
}

func newStubStore() *stubStore {
	return &stubStore{returns: make(map[string]*domain.TaxReturn)}
}

func (s *stubStore) Create(_ context.Context, r *domain.TaxReturn) error {
	if r.ReturnID == "" {
		r.ReturnID = "ctret-test-001"
	}
	if r.Status == "" {
		r.Status = domain.StatusDraft
	}
	if r.Currency == "" {
		r.Currency = "USD"
	}
	r.Compute()
	s.returns[r.ReturnID] = r
	return nil
}

func (s *stubStore) GetByID(_ context.Context, id string) (*domain.TaxReturn, error) {
	if r, ok := s.returns[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.ErrTaxReturnNotFound
}

func (s *stubStore) List(_ context.Context, _, _, _ string, _ int) ([]domain.TaxReturn, error) {
	var out []domain.TaxReturn
	for _, r := range s.returns {
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) Update(_ context.Context, r *domain.TaxReturn) error {
	r.Compute()
	s.returns[r.ReturnID] = r
	return nil
}

func (s *stubStore) Submit(_ context.Context, id, submittedBy string) (*domain.TaxReturn, error) {
	r, ok := s.returns[id]
	if !ok {
		return nil, domain.ErrTaxReturnNotFound
	}
	if r.Status == domain.StatusSubmitted {
		return nil, domain.ErrAlreadySubmitted
	}
	r.Status = domain.StatusSubmitted
	r.SubmittedBy = &submittedBy
	return r, nil
}

func (s *stubStore) Assess(_ context.Context, id string, req *domain.AssessTaxReturnRequest) (*domain.TaxReturn, error) {
	r, ok := s.returns[id]
	if !ok {
		return nil, domain.ErrTaxReturnNotFound
	}
	r.Status = domain.StatusAssessed
	r.AssessedTaxAmount = &req.AssessedTaxAmount
	r.AssessmentReference = &req.AssessmentReference
	return r, nil
}

// ─── Stub Publisher ───────────────────────────────────────────────────────────

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _, _, _ string, _ interface{}) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ─── Helpers ──────────────────────────────────────────────────────────────────

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

// ─── Tests ────────────────────────────────────────────────────────────────────

func TestCreateCorporateTaxReturn(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateTaxReturnRequest{
		LegalEntityID:         "le-uk-001",
		JurisdictionID:        "uk-england",
		TaxRegistrationNumber: "UTR9876543210",
		FiscalYear:            2026,
		AccountingPeriodStart: "2026-01-01",
		AccountingPeriodEnd:   "2026-12-31",
		GrossRevenue:          1_000_000.00,
		AllowableDeductions:   200_000.00,
		TaxRatePercent:        25.0,
		TaxCredits:            10_000.00,
		TaxAlreadyPaid:        50_000.00,
		Currency:              "GBP",
		EffectiveFrom:         "2026-01-01",
		CreatedBy:             "tax-manager-01",
	}
	w := httptest.NewRecorder()
	h.Create(w, buildRequest(http.MethodPost, "/v1/corporate-tax-returns", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.TaxReturn
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	// taxable_income = 1_000_000 - 200_000 = 800_000
	if resp.TaxableIncome != 800_000 {
		t.Errorf("expected taxable income 800000, got %f", resp.TaxableIncome)
	}
	// gross_tax_liability = 800_000 * 25% = 200_000
	if resp.GrossTaxLiability != 200_000 {
		t.Errorf("expected gross liability 200000, got %f", resp.GrossTaxLiability)
	}
	// net_tax_payable = 200_000 - 10_000 = 190_000
	if resp.NetTaxPayable != 190_000 {
		t.Errorf("expected net payable 190000, got %f", resp.NetTaxPayable)
	}
	// balance_due = 190_000 - 50_000 = 140_000
	if resp.BalanceDue != 140_000 {
		t.Errorf("expected balance due 140000, got %f", resp.BalanceDue)
	}
}

func TestSubmitCorporateTaxReturn(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Create first
	createBody := domain.CreateTaxReturnRequest{
		LegalEntityID:  "le-uk-001",
		JurisdictionID: "uk-england",
		FiscalYear:     2026,
		GrossRevenue:   500_000,
		TaxRatePercent: 19.0,
		Currency:       "GBP",
		EffectiveFrom:  "2026-01-01",
		CreatedBy:      "tax-manager-01",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/corporate-tax-returns", createBody))
	var created domain.TaxReturn
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Submit
	submitBody := domain.SubmitTaxReturnRequest{SubmittedBy: "tax-director"}
	wSubmit := httptest.NewRecorder()
	r.ServeHTTP(wSubmit, buildRequest(http.MethodPost, "/v1/corporate-tax-returns/"+created.ReturnID+"/submit", submitBody))
	if wSubmit.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wSubmit.Code, wSubmit.Body.String())
	}
	var submitted domain.TaxReturn
	_ = json.NewDecoder(wSubmit.Body).Decode(&submitted)
	if submitted.Status != domain.StatusSubmitted {
		t.Errorf("expected SUBMITTED, got %s", submitted.Status)
	}
}

func TestAssessCorporateTaxReturn(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Create
	createBody := domain.CreateTaxReturnRequest{
		LegalEntityID:  "le-sg-001",
		JurisdictionID: "sg",
		FiscalYear:     2025,
		GrossRevenue:   750_000,
		TaxRatePercent: 17.0,
		Currency:       "SGD",
		EffectiveFrom:  "2025-01-01",
		CreatedBy:      "finance-01",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/corporate-tax-returns", createBody))
	var created domain.TaxReturn
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Assess
	assessBody := domain.AssessTaxReturnRequest{
		AssessedTaxAmount:   127_500,
		AssessmentReference: "IRAS-2025-REF-001",
	}
	wAssess := httptest.NewRecorder()
	r.ServeHTTP(wAssess, buildRequest(http.MethodPost, "/v1/corporate-tax-returns/"+created.ReturnID+"/assess", assessBody))
	if wAssess.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wAssess.Code, wAssess.Body.String())
	}
	var assessed domain.TaxReturn
	_ = json.NewDecoder(wAssess.Body).Decode(&assessed)
	if assessed.Status != domain.StatusAssessed {
		t.Errorf("expected ASSESSED, got %s", assessed.Status)
	}
}
