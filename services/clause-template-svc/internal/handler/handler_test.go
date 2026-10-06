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

	"zoiko.io/clause-template-svc/internal/domain"
	"zoiko.io/clause-template-svc/internal/events"
)

type stubStore struct {
	clauses    map[string]*domain.Clause
	templates  map[string]*domain.ContractTemplate
	deviations map[string]*domain.DeviationRule
}

func newStubStore() *stubStore {
	return &stubStore{
		clauses:    make(map[string]*domain.Clause),
		templates:  make(map[string]*domain.ContractTemplate),
		deviations: make(map[string]*domain.DeviationRule),
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

func (s *stubStore) UpdateClause(_ context.Context, c *domain.Clause, _ string) error {
	existing, ok := s.clauses[c.ClauseID]
	if !ok {
		return domain.ErrClauseNotFound
	}
	if existing.Status != domain.StatusDraft && existing.Status != domain.StatusLegalReview {
		return domain.ErrWrongStatus
	}
	s.clauses[c.ClauseID] = c
	return nil
}

func (s *stubStore) SubmitClauseForLegalReview(_ context.Context, id, submittedBy string) (*domain.Clause, error) {
	c, ok := s.clauses[id]
	if !ok {
		return nil, domain.ErrClauseNotFound
	}
	if c.Status != domain.StatusDraft {
		return nil, domain.ErrWrongStatus
	}
	c.Status = domain.StatusLegalReview
	c.SubmittedBy = &submittedBy
	return c, nil
}

func (s *stubStore) ApproveClause(_ context.Context, id, approvedBy string) (*domain.Clause, error) {
	c, ok := s.clauses[id]
	if !ok {
		return nil, domain.ErrClauseNotFound
	}
	if c.Status != domain.StatusLegalReview {
		return nil, domain.ErrWrongStatus
	}
	if c.SubmittedBy != nil && *c.SubmittedBy == approvedBy {
		return nil, domain.ErrSelfApprovalNotAllowed
	}
	c.Status = domain.StatusApproved
	c.ApprovedBy = &approvedBy
	return c, nil
}

func (s *stubStore) ActivateClause(_ context.Context, id, activatedBy string) (*domain.Clause, error) {
	c, ok := s.clauses[id]
	if !ok {
		return nil, domain.ErrClauseNotFound
	}
	if c.Status != domain.StatusApproved {
		return nil, domain.ErrWrongStatus
	}
	c.Status = domain.StatusActive
	c.ActivatedBy = &activatedBy
	return c, nil
}

func (s *stubStore) RetireClause(_ context.Context, id, retiredBy string) (*domain.Clause, error) {
	c, ok := s.clauses[id]
	if !ok {
		return nil, domain.ErrClauseNotFound
	}
	if c.Status != domain.StatusActive {
		return nil, domain.ErrWrongStatus
	}
	c.Status = domain.StatusRetired
	c.RetiredBy = &retiredBy
	return c, nil
}

func (s *stubStore) SupersedeClause(_ context.Context, id, supersededBy string) (*domain.Clause, error) {
	c, ok := s.clauses[id]
	if !ok {
		return nil, domain.ErrClauseNotFound
	}
	if c.Status != domain.StatusActive {
		return nil, domain.ErrWrongStatus
	}
	c.Status = domain.StatusSuperseded
	c.SupersededBy = &supersededBy
	return c, nil
}

func (s *stubStore) ListClauseVersions(_ context.Context, _ string) ([]domain.ClauseVersion, error) {
	return nil, nil
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

func (s *stubStore) ApproveTemplate(_ context.Context, id, approvedBy string) (*domain.ContractTemplate, error) {
	t, ok := s.templates[id]
	if !ok {
		return nil, domain.ErrTemplateNotFound
	}
	if t.Status != domain.StatusDraft {
		return nil, domain.ErrWrongStatus
	}
	t.Status = domain.StatusActive
	t.ApprovedBy = &approvedBy
	return t, nil
}

func (s *stubStore) CreateDeviationRule(_ context.Context, d *domain.DeviationRule) error {
	if d.DeviationID == "" {
		d.DeviationID = "dev-test-001"
	}
	if d.Status == "" {
		d.Status = domain.DeviationStatusProposed
	}
	s.deviations[d.DeviationID] = d
	return nil
}

func (s *stubStore) GetDeviationRule(_ context.Context, id string) (*domain.DeviationRule, error) {
	if d, ok := s.deviations[id]; ok {
		return d, nil
	}
	return nil, domain.ErrDeviationNotFound
}

func (s *stubStore) ListDeviationRules(_ context.Context, _, _, _ string) ([]domain.DeviationRule, error) {
	var out []domain.DeviationRule
	for _, d := range s.deviations {
		out = append(out, *d)
	}
	return out, nil
}

func (s *stubStore) ApproveDeviationRule(_ context.Context, id, approvedBy string) (*domain.DeviationRule, error) {
	d, ok := s.deviations[id]
	if !ok {
		return nil, domain.ErrDeviationNotFound
	}
	if d.Status != domain.DeviationStatusProposed {
		return nil, domain.ErrWrongStatus
	}
	if d.ProposedBy == approvedBy {
		return nil, domain.ErrSelfApprovalNotAllowed
	}
	d.Status = domain.DeviationStatusApproved
	d.ApprovedBy = &approvedBy
	return d, nil
}

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// stubAuthz grants every request by default; tests can flip decision to
// force a denial or unavailable-service response.
type stubAuthz struct {
	err error
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	return a.err
}

var _ AuthZClient = (*stubAuthz)(nil)

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, &stubAuthz{}, logger)
}

func buildRequest(method, path string, body interface{}) *http.Request {
	return buildRequestAs(method, path, body, "user-test-01")
}

func buildRequestAs(method, path string, body interface{}, principalID string) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", "tenant-test-01")
	r.Header.Set("X-Principal-Id", principalID)
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

// newChiRouter builds the real production router (RegisterRoutes), needed
// for any route reading chi.URLParam(r, "id").
func newChiRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r
}

// Maker-checker (LEG-06 §8): the principal who submitted a clause for legal
// review may not be the one who approves it.
func TestApproveClause_BySameSubmitter_Returns403(t *testing.T) {
	store := newStubStore()
	submitter := "user-test-01" // matches buildRequest's X-Principal-Id
	store.clauses["cls-001"] = &domain.Clause{
		ClauseID: "cls-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.StatusLegalReview, SubmittedBy: &submitter,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubPublisher{}, &stubAuthz{}, logger)
	router := newChiRouter(h)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/clauses/cls-001/approve", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 approving a clause submitted by the same principal, got %d — %s", w.Code, w.Body.String())
	}
}

func TestClauseFullLifecycle_ReachesActive(t *testing.T) {
	store := newStubStore()
	submitter := "drafter-1"
	store.clauses["cls-001"] = &domain.Clause{
		ClauseID: "cls-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.StatusDraft, SubmittedBy: &submitter,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubPublisher{}, &stubAuthz{}, logger)
	router := newChiRouter(h)

	// Submit (DRAFT -> LEGAL_REVIEW), then approve as a different principal.
	store.clauses["cls-001"].Status = domain.StatusDraft
	wSubmit := httptest.NewRecorder()
	router.ServeHTTP(wSubmit, buildRequest(http.MethodPost, "/v1/clauses/cls-001/submit-legal-review", nil))
	if wSubmit.Code != http.StatusOK {
		t.Fatalf("submit-legal-review: expected 200, got %d — %s", wSubmit.Code, wSubmit.Body.String())
	}

	wApprove := httptest.NewRecorder()
	router.ServeHTTP(wApprove, buildRequestAs(http.MethodPost, "/v1/clauses/cls-001/approve", nil, "legal-lead-1"))
	if wApprove.Code != http.StatusOK {
		t.Fatalf("approve: expected 200, got %d — %s", wApprove.Code, wApprove.Body.String())
	}

	wActivate := httptest.NewRecorder()
	router.ServeHTTP(wActivate, buildRequestAs(http.MethodPost, "/v1/clauses/cls-001/activate", nil, "legal-lead-1"))
	if wActivate.Code != http.StatusOK {
		t.Fatalf("activate: expected 200, got %d — %s", wActivate.Code, wActivate.Body.String())
	}
	var resp domain.Clause
	_ = json.NewDecoder(wActivate.Body).Decode(&resp)
	if resp.Status != domain.StatusActive {
		t.Errorf("expected ACTIVE, got %s", resp.Status)
	}
}

func TestUpdateClause_RefusedOnceApproved(t *testing.T) {
	store := newStubStore()
	store.clauses["cls-001"] = &domain.Clause{
		ClauseID: "cls-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.StatusApproved, Title: "Original", Body: "Original body", Category: domain.ClauseCategoryOther,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubPublisher{}, &stubAuthz{}, logger)
	router := newChiRouter(h)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPut, "/v1/clauses/cls-001", domain.UpdateClauseRequest{Title: "Sneaky edit"}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 updating an APPROVED clause, got %d — %s", w.Code, w.Body.String())
	}
}

// Accountable approval (LEG-06 §8.1): the proposer may not approve their own
// deviation rule.
func TestApproveDeviationRule_BySameProposer_Returns403(t *testing.T) {
	store := newStubStore()
	store.deviations["dev-001"] = &domain.DeviationRule{
		DeviationID: "dev-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.DeviationStatusProposed, ProposedBy: "user-test-01",
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubPublisher{}, &stubAuthz{}, logger)
	router := newChiRouter(h)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/deviation-rules/dev-001/approve", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 approving own deviation rule, got %d — %s", w.Code, w.Body.String())
	}
}

func TestCreateDeviationRule_InvalidRisk_Returns400(t *testing.T) {
	h := newTestHandler()
	router := newChiRouter(h)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/deviation-rules", domain.CreateDeviationRuleRequest{
		LegalEntityID: "le-001", JurisdictionID: "us-de", Description: "x", RiskClassification: "EXTREME",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid risk classification, got %d — %s", w.Code, w.Body.String())
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
