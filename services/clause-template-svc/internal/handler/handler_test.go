package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/clause-template-svc/internal/domain"
	"zoiko.io/clause-template-svc/internal/events"
)

type stubStore struct {
	clauses   map[string]*domain.Clause
	templates map[string]*domain.ContractTemplate
}

func newStubStore() *stubStore {
	return &stubStore{
		clauses:   make(map[string]*domain.Clause),
		templates: make(map[string]*domain.ContractTemplate),
	}
}

func (s *stubStore) CreateClause(_ context.Context, c *domain.Clause) error {
	if c.ClauseID == "" {
		c.ClauseID = "cls-test-001"
	}
	if c.Status == "" {
		c.Status = domain.StatusDraft
	}
	s.clauses[c.ClauseID] = c
	return nil
}

func (s *stubStore) GetClause(_ context.Context, id string) (*domain.Clause, error) {
	if c, ok := s.clauses[id]; ok {
		return c, nil
	}
	return nil, domain.ErrClauseNotFound
}

func (s *stubStore) ListClauses(_ context.Context, _, _ string) ([]domain.Clause, error) {
	var out []domain.Clause
	for _, c := range s.clauses {
		out = append(out, *c)
	}
	return out, nil
}

func (s *stubStore) UpdateClause(_ context.Context, c *domain.Clause) error {
	s.clauses[c.ClauseID] = c
	return nil
}

func (s *stubStore) CreateTemplate(_ context.Context, t *domain.ContractTemplate) error {
	if t.TemplateID == "" {
		t.TemplateID = "tmpl-test-001"
	}
	if t.Status == "" {
		t.Status = domain.StatusDraft
	}
	s.templates[t.TemplateID] = t
	return nil
}

func (s *stubStore) GetTemplate(_ context.Context, id string) (*domain.ContractTemplate, error) {
	if t, ok := s.templates[id]; ok {
		return t, nil
	}
	return nil, domain.ErrTemplateNotFound
}

func (s *stubStore) ListTemplates(_ context.Context, _, _ string) ([]domain.ContractTemplate, error) {
	var out []domain.ContractTemplate
	for _, t := range s.templates {
		out = append(out, *t)
	}
	return out, nil
}

func (s *stubStore) UpdateTemplate(_ context.Context, t *domain.ContractTemplate) error {
	s.templates[t.TemplateID] = t
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

func TestCreateClause(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateClauseRequest{
		LegalEntityID:  "le-001",
		Title:          "Confidentiality Standard",
		Category:       domain.ClauseCategoryConfidentiality,
		Body:           "All information shared shall remain confidential...",
		JurisdictionID: "us-delaware",
		EffectiveFrom:  "2026-01-01",
		CreatedBy:      "user-001",
	}
	w := httptest.NewRecorder()
	h.CreateClause(w, buildRequest(http.MethodPost, "/v1/clauses", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.Clause
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Title != "Confidentiality Standard" {
		t.Errorf("unexpected title: %s", resp.Title)
	}
	if resp.Status != domain.StatusDraft {
		t.Errorf("expected DRAFT, got %s", resp.Status)
	}
}

func TestCreateTemplate(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateTemplateRequest{
		LegalEntityID:  "le-001",
		Title:          "Standard NDA Template",
		ContractType:   "NDA",
		Description:    "Standard non-disclosure agreement template",
		ClauseIDs:      []string{"cls-001", "cls-002"},
		JurisdictionID: "us-delaware",
		EffectiveFrom:  "2026-01-01",
		CreatedBy:      "user-001",
	}
	w := httptest.NewRecorder()
	h.CreateTemplate(w, buildRequest(http.MethodPost, "/v1/templates", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.ContractTemplate
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Title != "Standard NDA Template" {
		t.Errorf("unexpected title: %s", resp.Title)
	}
	if resp.Status != domain.StatusDraft {
		t.Errorf("expected DRAFT, got %s", resp.Status)
	}
}
