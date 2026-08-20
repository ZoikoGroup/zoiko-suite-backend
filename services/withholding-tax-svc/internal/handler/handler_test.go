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

	"zoiko.io/withholding-tax-svc/internal/authz"
	"zoiko.io/withholding-tax-svc/internal/domain"
	"zoiko.io/withholding-tax-svc/internal/events"
)

type mockStore struct {
	items map[string]*domain.WithholdingTaxObligation
}

func newMockStore() *mockStore {
	return &mockStore{items: make(map[string]*domain.WithholdingTaxObligation)}
}

func (m *mockStore) Create(ctx context.Context, o *domain.WithholdingTaxObligation) error {
	if o.ObligationID == "" {
		o.ObligationID = "whto-test-123"
	}
	o.CreatedAt = time.Now().UTC()
	o.UpdatedAt = time.Now().UTC()
	if o.Status == "" {
		o.Status = domain.StatusPendingRemittance
	}
	o.Compute()
	m.items[o.ObligationID] = o
	return nil
}

func (m *mockStore) GetByID(ctx context.Context, id string) (*domain.WithholdingTaxObligation, error) {
	o, ok := m.items[id]
	if !ok {
		return nil, domain.ErrObligationNotFound
	}
	return o, nil
}

func (m *mockStore) List(ctx context.Context, legalEntityID, jurisdictionID, counterpartyID, status string) ([]domain.WithholdingTaxObligation, error) {
	var out []domain.WithholdingTaxObligation
	for _, item := range m.items {
		if legalEntityID != "" && item.LegalEntityID != legalEntityID {
			continue
		}
		if jurisdictionID != "" && item.JurisdictionID != jurisdictionID {
			continue
		}
		if counterpartyID != "" && item.CounterpartyID != counterpartyID {
			continue
		}
		if status != "" && string(item.Status) != status {
			continue
		}
		out = append(out, *item)
	}
	return out, nil
}

func (m *mockStore) Update(ctx context.Context, o *domain.WithholdingTaxObligation) error {
	if _, ok := m.items[o.ObligationID]; !ok {
		return domain.ErrObligationNotFound
	}
	o.Compute()
	o.UpdatedAt = time.Now().UTC()
	m.items[o.ObligationID] = o
	return nil
}

func (m *mockStore) Remit(ctx context.Context, id string, req *domain.RemitObligationRequest) (*domain.WithholdingTaxObligation, error) {
	o, ok := m.items[id]
	if !ok {
		return nil, domain.ErrObligationNotFound
	}
	if o.Status == domain.StatusRemitted {
		return nil, domain.ErrAlreadyRemitted
	}
	if o.Status == domain.StatusCancelled {
		return nil, domain.ErrAlreadyCancelled
	}
	now := time.Now().UTC()
	o.Status = domain.StatusRemitted
	o.RemittanceReference = &req.RemittanceReference
	o.RemittedAt = &now
	o.RemittedBy = &req.RemittedBy
	o.UpdatedAt = now
	return o, nil
}

func (m *mockStore) Cancel(ctx context.Context, id string, req *domain.CancelObligationRequest) (*domain.WithholdingTaxObligation, error) {
	o, ok := m.items[id]
	if !ok {
		return nil, domain.ErrObligationNotFound
	}
	if o.Status == domain.StatusRemitted {
		return nil, domain.ErrAlreadyRemitted
	}
	if o.Status == domain.StatusCancelled {
		return nil, domain.ErrAlreadyCancelled
	}
	now := time.Now().UTC()
	o.Status = domain.StatusCancelled
	o.UpdatedAt = now
	return o, nil
}

type mockPublisher struct{}

func (p *mockPublisher) Publish(ctx context.Context, params events.PublishParams) error {
	return nil
}

// newGrantingAuthzServer stands in for authorization-svc and always grants,
// matching the real contract: HTTP 200 with {"decision_outcome": "GRANTED"}.
func newGrantingAuthzServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupTestRouter(t *testing.T) (*chi.Mux, *mockStore) {
	st := newMockStore()
	pub := &mockPublisher{}
	authzSrv := newGrantingAuthzServer(t)
	az := authz.NewClient(authzSrv.URL)
	logger, _ := zap.NewDevelopment()
	h := New(st, pub, az, logger)

	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r, st
}

func TestCalculateWithholding(t *testing.T) {
	r, _ := setupTestRouter(t)

	reqPayload := domain.CalculateWithholdingRequest{
		LegalEntityID:          "entity-001",
		GrossPaymentAmount:     10000.0,
		TaxableBaseAmount:      10000.0,
		WithholdingRatePercent: 15.0,
		Currency:               "USD",
	}
	body, _ := json.Marshal(reqPayload)

	req := httptest.NewRequest("POST", "/v1/withholding-tax/calculate", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "user-001")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var res domain.CalculateWithholdingResponse
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if res.WithheldAmount != 1500.0 {
		t.Errorf("expected withheld amount 1500.0, got %f", res.WithheldAmount)
	}
}

func TestCreateAndRemitObligation(t *testing.T) {
	r, _ := setupTestRouter(t)

	reqPayload := domain.CreateObligationRequest{
		LegalEntityID:          "entity-001",
		JurisdictionID:         "US-CA",
		CounterpartyID:         "vendor-99",
		PaymentReference:       "INV-2026-001",
		PaymentType:            "ROYALTIES",
		GrossPaymentAmount:     20000.0,
		WithholdingRatePercent: 10.0,
		Currency:               "USD",
		EffectiveFrom:          "2026-01-01",
		CreatedBy:              "admin",
	}
	body, _ := json.Marshal(reqPayload)

	req := httptest.NewRequest("POST", "/v1/withholding-tax", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "user-001")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", w.Code)
	}

	var created domain.WithholdingTaxObligation
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if created.WithheldAmount != 2000.0 {
		t.Errorf("expected withheld amount 2000.0, got %f", created.WithheldAmount)
	}

	// Remit obligation
	remitPayload := domain.RemitObligationRequest{
		RemittanceReference: "REMIT-2026-ABC",
		RemittedBy:          "tax_officer",
	}
	remitBody, _ := json.Marshal(remitPayload)
	remitReq := httptest.NewRequest("POST", "/v1/withholding-tax/"+created.ObligationID+"/remit", bytes.NewBuffer(remitBody))
	remitReq.Header.Set("Content-Type", "application/json")
	remitReq.Header.Set("X-Principal-Id", "user-001")
	remitW := httptest.NewRecorder()

	r.ServeHTTP(remitW, remitReq)

	if remitW.Code != http.StatusOK {
		t.Fatalf("expected status 200 on remit, got %d", remitW.Code)
	}

	var remitted domain.WithholdingTaxObligation
	if err := json.NewDecoder(remitW.Body).Decode(&remitted); err != nil {
		t.Fatalf("failed to decode remitted response: %v", err)
	}
	if remitted.Status != domain.StatusRemitted {
		t.Errorf("expected status REMITTED, got %s", remitted.Status)
	}
}
