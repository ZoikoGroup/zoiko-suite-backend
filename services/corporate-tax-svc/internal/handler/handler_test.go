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

	"zoiko.io/corporate-tax-svc/internal/authz"
	"zoiko.io/corporate-tax-svc/internal/domain"
	"zoiko.io/corporate-tax-svc/internal/events"
)

// ─── Stub Store ───────────────────────────────────────────────────────────────

type stubStore struct {
	returns map[string]*domain.TaxReturn
	nexus   map[string]*domain.NexusRecord
}

func newStubStore() *stubStore {
	return &stubStore{
		returns: make(map[string]*domain.TaxReturn),
		nexus:   make(map[string]*domain.NexusRecord),
	}
}

func (s *stubStore) Create(_ context.Context, r *domain.TaxReturn) error {
	if r.ReturnID == "" {
		r.ReturnID = "ctret-test-001"
	}
	if r.Status == "" {
		r.Status = domain.StatusDraft
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

func (s *stubStore) CreateNexusRecord(_ context.Context, r *domain.NexusRecord) error {
	if r.NexusRecordID == "" {
		r.NexusRecordID = "nexus-test-001"
	}
	s.nexus[r.NexusRecordID] = r
	return nil
}

func (s *stubStore) GetNexusRecordByID(_ context.Context, id string) (*domain.NexusRecord, error) {
	if r, ok := s.nexus[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, domain.ErrNexusRecordNotFound
}

func (s *stubStore) ListNexusRecords(_ context.Context, legalEntityID, jurisdictionID string) ([]domain.NexusRecord, error) {
	var out []domain.NexusRecord
	for _, r := range s.nexus {
		if legalEntityID != "" && r.LegalEntityID != legalEntityID {
			continue
		}
		if jurisdictionID != "" && r.JurisdictionID != jurisdictionID {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

// ─── Stub Publisher ───────────────────────────────────────────────────────────

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ─── Helpers ──────────────────────────────────────────────────────────────────

func newStubAuthzServer(t *testing.T) *authz.Client {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	t.Cleanup(ts.Close)
	return authz.NewClient(ts.URL)
}

func newTestHandler(t *testing.T) *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, newStubAuthzServer(t), logger)
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

// ─── Tests ────────────────────────────────────────────────────────────────────

func TestCreateCorporateTaxReturn(t *testing.T) {
	h := newTestHandler(t)
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
	var res domain.TaxReturn
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.TaxableIncome != 800_000.00 {
		t.Errorf("expected taxable income 800000, got %f", res.TaxableIncome)
	}
	if res.GrossTaxLiability != 200_000.00 {
		t.Errorf("expected gross tax liability 200000, got %f", res.GrossTaxLiability)
	}
	if res.NetTaxPayable != 190_000.00 {
		t.Errorf("expected net tax payable 190000, got %f", res.NetTaxPayable)
	}
	if res.BalanceDue != 140_000.00 {
		t.Errorf("expected balance due 140000, got %f", res.BalanceDue)
	}
}

func TestSubmitCorporateTaxReturn(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Create
	createBody := domain.CreateTaxReturnRequest{
		LegalEntityID:  "le-de-001",
		JurisdictionID: "de",
		FiscalYear:     2026,
		GrossRevenue:   500_000,
		TaxRatePercent: 15.0,
		Currency:       "EUR",
		EffectiveFrom:  "2026-01-01",
		CreatedBy:      "cfo-01",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/corporate-tax-returns", createBody))
	var created domain.TaxReturn
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Submit
	submitBody := domain.SubmitTaxReturnRequest{SubmittedBy: "principal-test-01"}
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
	h := newTestHandler(t)
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

func TestNexusRecords_CreateAndList(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	nexusBody := domain.CreateNexusRecordRequest{
		LegalEntityID:   "le-us-001",
		JurisdictionID:  "us-california",
		NexusType:       "ECONOMIC",
		ThresholdAmount: 500_000.00,
		Currency:        "USD",
		EffectiveFrom:   "2026-01-01",
	}

	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/nexus-records", nexusBody))
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d - %s", wCreate.Code, wCreate.Body.String())
	}

	var created domain.NexusRecord
	if err := json.NewDecoder(wCreate.Body).Decode(&created); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if created.NexusType != "ECONOMIC" {
		t.Errorf("expected nexus type ECONOMIC, got %s", created.NexusType)
	}

	// Get by ID
	wGet := httptest.NewRecorder()
	r.ServeHTTP(wGet, buildRequest(http.MethodGet, "/v1/nexus-records/"+created.NexusRecordID, nil))
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d - %s", wGet.Code, wGet.Body.String())
	}

	// List
	wList := httptest.NewRecorder()
	r.ServeHTTP(wList, buildRequest(http.MethodGet, "/v1/nexus-records?legal_entity_id=le-us-001", nil))
	if wList.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d - %s", wList.Code, wList.Body.String())
	}

	var list []domain.NexusRecord
	if err := json.NewDecoder(wList.Body).Decode(&list); err != nil {
		t.Fatalf("failed to decode list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 nexus record, got %d", len(list))
	}
}
