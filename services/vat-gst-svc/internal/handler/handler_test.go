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

	"zoiko.io/vat-gst-svc/internal/domain"
	"zoiko.io/vat-gst-svc/internal/events"
)

type stubStore struct {
	returns map[string]*domain.VATReturn
}

func newStubStore() *stubStore {
	return &stubStore{
		returns: make(map[string]*domain.VATReturn),
	}
}

func (s *stubStore) CreateVATReturn(_ context.Context, r *domain.VATReturn) error {
	if r.ReturnID == "" {
		r.ReturnID = "vret-test-001"
	}
	if r.Status == "" {
		r.Status = domain.StatusDraft
	}
	r.NetTaxPayable = r.OutputTaxAmount - r.InputTaxAmount
	s.returns[r.ReturnID] = r
	return nil
}

func (s *stubStore) GetVATReturn(_ context.Context, id string) (*domain.VATReturn, error) {
	if r, ok := s.returns[id]; ok {
		return r, nil
	}
	return nil, domain.ErrVATReturnNotFound
}

func (s *stubStore) ListVATReturns(_ context.Context, _, _, _ string) ([]domain.VATReturn, error) {
	var out []domain.VATReturn
	for _, r := range s.returns {
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) UpdateVATReturn(_ context.Context, r *domain.VATReturn) error {
	r.NetTaxPayable = r.OutputTaxAmount - r.InputTaxAmount
	s.returns[r.ReturnID] = r
	return nil
}

func (s *stubStore) FileVATReturn(_ context.Context, id, filedBy string) (*domain.VATReturn, error) {
	r, ok := s.returns[id]
	if !ok {
		return nil, domain.ErrVATReturnNotFound
	}
	if r.Status == domain.StatusFiled {
		return nil, domain.ErrAlreadyFiled
	}
	r.Status = domain.StatusFiled
	r.FiledBy = &filedBy
	return r, nil
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

func TestCreateVATReturn(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateVATReturnRequest{
		LegalEntityID:         "le-001",
		JurisdictionID:        "uk-england",
		TaxRegistrationNumber: "GB999888777",
		TaxPeriod:             "2026-Q1",
		TotalSalesAmount:      100000.0,
		TotalPurchaseAmount:   40000.0,
		OutputTaxAmount:       20000.0,
		InputTaxAmount:        8000.0,
		Currency:              "GBP",
		EffectiveFrom:         "2026-01-01",
		CreatedBy:             "tax-officer-01",
	}
	w := httptest.NewRecorder()
	h.CreateVATReturn(w, buildRequest(http.MethodPost, "/v1/vat-returns", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.VATReturn
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.TaxPeriod != "2026-Q1" {
		t.Errorf("unexpected tax period: %s", resp.TaxPeriod)
	}
	if resp.NetTaxPayable != 12000.0 {
		t.Errorf("expected net tax 12000.0, got %f", resp.NetTaxPayable)
	}
}

func TestFileVATReturn(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Create return first
	body := domain.CreateVATReturnRequest{
		LegalEntityID:         "le-001",
		JurisdictionID:        "uk-england",
		TaxRegistrationNumber: "GB999888777",
		TaxPeriod:             "2026-Q1",
		TotalSalesAmount:      50000.0,
		OutputTaxAmount:       10000.0,
		InputTaxAmount:        3000.0,
		CreatedBy:             "tax-officer-01",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/vat-returns", body))
	var created domain.VATReturn
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// File return
	fileBody := domain.FileVATReturnRequest{
		FiledBy: "tax-director",
	}
	wFile := httptest.NewRecorder()
	r.ServeHTTP(wFile, buildRequest(http.MethodPost, "/v1/vat-returns/"+created.ReturnID+"/file", fileBody))
	if wFile.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wFile.Code, wFile.Body.String())
	}
	var filed domain.VATReturn
	_ = json.NewDecoder(wFile.Body).Decode(&filed)
	if filed.Status != domain.StatusFiled {
		t.Errorf("expected FILED, got %s", filed.Status)
	}
}
