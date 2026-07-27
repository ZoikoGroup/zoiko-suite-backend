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

	"zoiko.io/corporate-actions-svc/internal/domain"
	"zoiko.io/corporate-actions-svc/internal/events"
)

type stubStore struct {
	actions map[string]*domain.CorporateAction
}

func newStubStore() *stubStore {
	return &stubStore{
		actions: make(map[string]*domain.CorporateAction),
	}
}

func (s *stubStore) CreateAction(_ context.Context, a *domain.CorporateAction) error {
	if a.ActionID == "" {
		a.ActionID = "act-test-001"
	}
	if a.Status == "" {
		a.Status = domain.ActionStatusProposed
	}
	s.actions[a.ActionID] = a
	return nil
}

func (s *stubStore) GetAction(_ context.Context, id string) (*domain.CorporateAction, error) {
	if a, ok := s.actions[id]; ok {
		return a, nil
	}
	return nil, domain.ErrCorporateActionNotFound
}

func (s *stubStore) ListActions(_ context.Context, _, _, _ string) ([]domain.CorporateAction, error) {
	var out []domain.CorporateAction
	for _, a := range s.actions {
		out = append(out, *a)
	}
	return out, nil
}

func (s *stubStore) UpdateAction(_ context.Context, a *domain.CorporateAction) error {
	s.actions[a.ActionID] = a
	return nil
}

func (s *stubStore) ExecuteAction(_ context.Context, id string, req *domain.ExecuteCorporateActionRequest) (*domain.CorporateAction, error) {
	a, ok := s.actions[id]
	if !ok {
		return nil, domain.ErrCorporateActionNotFound
	}
	if a.Status == domain.ActionStatusExecuted {
		return nil, domain.ErrActionAlreadyExecuted
	}
	a.Status = domain.ActionStatusExecuted
	a.ExecutedBy = &req.ExecutedBy
	return a, nil
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

func TestCreateAction(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateCorporateActionRequest{
		LegalEntityID:   "le-001",
		Title:           "Series B Share Issuance",
		ActionType:      domain.ActionTypeShareIssuance,
		Description:     "Issuance of 1,000,000 Preferred Shares",
		EffectiveDate:   "2026-06-01",
		ValuationAmount: 10000000,
		Currency:        "USD",
		EffectiveFrom:   "2026-01-01",
		CreatedBy:       "cfo-001",
	}
	w := httptest.NewRecorder()
	h.CreateAction(w, buildRequest(http.MethodPost, "/v1/corporate-actions", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.CorporateAction
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Title != "Series B Share Issuance" {
		t.Errorf("unexpected title: %s", resp.Title)
	}
	if resp.Status != domain.ActionStatusProposed {
		t.Errorf("expected PROPOSED, got %s", resp.Status)
	}
}

func TestExecuteAction(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// First create an action
	body := domain.CreateCorporateActionRequest{
		LegalEntityID: "le-001",
		Title:         "Subsidiary Restructure",
		ActionType:    domain.ActionTypeRestructure,
		EffectiveDate: "2026-05-01",
		CreatedBy:     "ceo-001",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/corporate-actions", body))
	var created domain.CorporateAction
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Execute it
	execBody := domain.ExecuteCorporateActionRequest{
		ExecutedBy: "ceo-001",
	}
	wExec := httptest.NewRecorder()
	r.ServeHTTP(wExec, buildRequest(http.MethodPost, "/v1/corporate-actions/"+created.ActionID+"/execute", execBody))
	if wExec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wExec.Code, wExec.Body.String())
	}
	var executed domain.CorporateAction
	_ = json.NewDecoder(wExec.Body).Decode(&executed)
	if executed.Status != domain.ActionStatusExecuted {
		t.Errorf("expected EXECUTED, got %s", executed.Status)
	}
}
