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

	"zoiko.io/obligation-tracking-svc/internal/domain"
	"zoiko.io/obligation-tracking-svc/internal/events"
)

type stubStore struct {
	obligations map[string]*domain.Obligation
}

func newStubStore() *stubStore {
	return &stubStore{
		obligations: make(map[string]*domain.Obligation),
	}
}

func (s *stubStore) CreateObligation(_ context.Context, o *domain.Obligation) error {
	if o.ObligationID == "" {
		o.ObligationID = "obg-test-001"
	}
	if o.Status == "" {
		o.Status = domain.ObligationStatusPending
	}
	s.obligations[o.ObligationID] = o
	return nil
}

func (s *stubStore) GetObligation(_ context.Context, id string) (*domain.Obligation, error) {
	if o, ok := s.obligations[id]; ok {
		return o, nil
	}
	return nil, domain.ErrObligationNotFound
}

func (s *stubStore) ListObligations(_ context.Context, _, _, _ string) ([]domain.Obligation, error) {
	var out []domain.Obligation
	for _, o := range s.obligations {
		out = append(out, *o)
	}
	return out, nil
}

func (s *stubStore) UpdateObligation(_ context.Context, o *domain.Obligation) error {
	s.obligations[o.ObligationID] = o
	return nil
}

func (s *stubStore) FulfillObligation(_ context.Context, id string, req *domain.FulfillObligationRequest) (*domain.Obligation, error) {
	o, ok := s.obligations[id]
	if !ok {
		return nil, domain.ErrObligationNotFound
	}
	if o.Status == domain.ObligationStatusFulfilled {
		return nil, domain.ErrObligationAlreadyFulfilled
	}
	o.Status = domain.ObligationStatusFulfilled
	o.FulfilledBy = &req.FulfilledBy
	return o, nil
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

func TestCreateObligation(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateObligationRequest{
		LegalEntityID:  "le-001",
		SourceType:     "CONTRACT",
		SourceID:       "ctr-123",
		Title:          "Annual Tax Filing",
		Description:    "Submit corporate income tax return",
		ObligationType: domain.ObligationTypeStatutory,
		RiskLevel:      domain.RiskLevelHigh,
		DueDate:        "2026-04-15",
		EffectiveFrom:  "2026-01-01",
		CreatedBy:      "user-001",
	}
	w := httptest.NewRecorder()
	h.CreateObligation(w, buildRequest(http.MethodPost, "/v1/obligations", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.Obligation
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Title != "Annual Tax Filing" {
		t.Errorf("unexpected title: %s", resp.Title)
	}
	if resp.Status != domain.ObligationStatusPending {
		t.Errorf("expected PENDING, got %s", resp.Status)
	}
}

func TestFulfillObligation(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// First create an obligation
	body := domain.CreateObligationRequest{
		LegalEntityID:  "le-001",
		Title:          "Quarterly Audit Review",
		ObligationType: domain.ObligationTypeRegulatory,
		DueDate:        "2026-03-31",
		CreatedBy:      "user-001",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/obligations", body))
	var created domain.Obligation
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Fulfill it
	fulfillBody := domain.FulfillObligationRequest{
		FulfilledBy:     "auditor-001",
		FulfillmentNote: "Audit completed successfully",
	}
	wFulfill := httptest.NewRecorder()
	r.ServeHTTP(wFulfill, buildRequest(http.MethodPost, "/v1/obligations/"+created.ObligationID+"/fulfill", fulfillBody))
	if wFulfill.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wFulfill.Code, wFulfill.Body.String())
	}
	var fulfilled domain.Obligation
	_ = json.NewDecoder(wFulfill.Body).Decode(&fulfilled)
	if fulfilled.Status != domain.ObligationStatusFulfilled {
		t.Errorf("expected FULFILLED, got %s", fulfilled.Status)
	}
}
