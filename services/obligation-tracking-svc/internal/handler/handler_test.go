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
	if o.ExtractedByAI {
		o.Status = domain.ObligationStatusCandidate
	} else {
		o.Status = domain.ObligationStatusPlanned
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
	existing, ok := s.obligations[o.ObligationID]
	if !ok {
		return domain.ErrObligationNotFound
	}
	if existing.Status != domain.ObligationStatusCandidate && existing.Status != domain.ObligationStatusPlanned {
		return domain.ErrWrongStatus
	}
	s.obligations[o.ObligationID] = o
	return nil
}

func (s *stubStore) ValidateExtractedObligation(_ context.Context, id, validatedBy string) (*domain.Obligation, error) {
	o, ok := s.obligations[id]
	if !ok {
		return nil, domain.ErrObligationNotFound
	}
	if o.Status != domain.ObligationStatusCandidate {
		return nil, domain.ErrWrongStatus
	}
	o.Status = domain.ObligationStatusPlanned
	o.ValidatedBy = &validatedBy
	return o, nil
}

func (s *stubStore) Schedule(_ context.Context, id string, req *domain.ScheduleObligationRequest) (*domain.Obligation, error) {
	o, ok := s.obligations[id]
	if !ok {
		return nil, domain.ErrObligationNotFound
	}
	if o.Status != domain.ObligationStatusPlanned {
		return nil, domain.ErrWrongStatus
	}
	if req.TriggerDescription == "" || req.CalculationMethod == "" {
		return nil, domain.ErrAmbiguousDueDateBasis
	}
	o.Status = domain.ObligationStatusActive
	o.TriggerDescription = req.TriggerDescription
	o.CalculationMethod = req.CalculationMethod
	o.ScheduledBy = &req.ScheduledBy
	return o, nil
}

func (s *stubStore) MarkDue(_ context.Context, id, markedBy string) (*domain.Obligation, error) {
	o, ok := s.obligations[id]
	if !ok {
		return nil, domain.ErrObligationNotFound
	}
	if o.Status != domain.ObligationStatusActive {
		return nil, domain.ErrWrongStatus
	}
	o.Status = domain.ObligationStatusDue
	o.DueMarkedBy = &markedBy
	return o, nil
}

func (s *stubStore) MarkInProgress(_ context.Context, id, markedBy string) (*domain.Obligation, error) {
	o, ok := s.obligations[id]
	if !ok {
		return nil, domain.ErrObligationNotFound
	}
	if o.Status != domain.ObligationStatusDue {
		return nil, domain.ErrWrongStatus
	}
	o.Status = domain.ObligationStatusInProgress
	o.InProgressBy = &markedBy
	return o, nil
}

func (s *stubStore) actionable(id string) (*domain.Obligation, error) {
	o, ok := s.obligations[id]
	if !ok {
		return nil, domain.ErrObligationNotFound
	}
	switch o.Status {
	case domain.ObligationStatusActive, domain.ObligationStatusDue, domain.ObligationStatusInProgress:
		return o, nil
	default:
		return nil, domain.ErrWrongStatus
	}
}

func (s *stubStore) Complete(_ context.Context, id, satisfiedBy string) (*domain.Obligation, error) {
	o, err := s.actionable(id)
	if err != nil {
		return nil, err
	}
	o.Status = domain.ObligationStatusSatisfied
	o.SatisfiedBy = &satisfiedBy
	return o, nil
}

func (s *stubStore) Waive(_ context.Context, id string, req *domain.WaiveObligationRequest) (*domain.Obligation, error) {
	if req.WaiverAuthorityReference == "" {
		return nil, domain.ErrWaiverAuthorityRequired
	}
	o, err := s.actionable(id)
	if err != nil {
		return nil, err
	}
	o.Status = domain.ObligationStatusWaived
	o.WaivedBy = &req.WaivedBy
	o.WaiverAuthorityReference = &req.WaiverAuthorityReference
	return o, nil
}

func (s *stubStore) RecordBreach(_ context.Context, id string, req *domain.RecordBreachRequest) (*domain.Obligation, error) {
	o, err := s.actionable(id)
	if err != nil {
		return nil, err
	}
	o.Status = domain.ObligationStatusBreached
	o.BreachedBy = &req.BreachedBy
	return o, nil
}

func (s *stubStore) Dispute(_ context.Context, id string, req *domain.DisputeObligationRequest) (*domain.Obligation, error) {
	o, err := s.actionable(id)
	if err != nil {
		return nil, err
	}
	o.Status = domain.ObligationStatusDisputed
	o.DisputedBy = &req.DisputedBy
	return o, nil
}

func (s *stubStore) Supersede(_ context.Context, id, supersededBy string) (*domain.Obligation, error) {
	o, ok := s.obligations[id]
	if !ok {
		return nil, domain.ErrObligationNotFound
	}
	if o.Status.IsFinal() {
		return nil, domain.ErrWrongStatus
	}
	o.Status = domain.ObligationStatusSuperseded
	o.SupersededBy = &supersededBy
	return o, nil
}

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthzClient struct{}

func (s *stubAuthzClient) CheckAllowed(_ context.Context, _, _, _ string) error {
	return nil
}

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, &stubAuthzClient{}, logger)
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

func TestCreateObligation(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateObligationRequest{
		LegalEntityID:  "le-001",
		SourceType:     "CONTRACT",
		SourceID:       "ctr-123",
		Title:          "Deliver Q1 SLA report",
		Description:    "SLA reporting obligation extracted from vendor contract",
		ObligationType: domain.ObligationTypeContractual,
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
	if resp.Title != "Deliver Q1 SLA report" {
		t.Errorf("unexpected title: %s", resp.Title)
	}
	if resp.Status != domain.ObligationStatusPlanned {
		t.Errorf("expected PLANNED, got %s", resp.Status)
	}
}

// AI extraction cannot activate an obligation without validation (LEG-07
// §9.1): an extracted obligation starts as a CANDIDATE, not PLANNED.
func TestCreateObligation_ExtractedByAI_StartsAsCandidate(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateObligationRequest{
		LegalEntityID: "le-001", Title: "Auto-extracted notice obligation",
		ObligationType: domain.ObligationTypeContractual, DueDate: "2026-04-15",
		ExtractedByAI: true, CreatedBy: "ai-extraction-pipeline",
	}
	w := httptest.NewRecorder()
	h.CreateObligation(w, buildRequest(http.MethodPost, "/v1/obligations", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.Obligation
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != domain.ObligationStatusCandidate {
		t.Errorf("expected CANDIDATE for an AI-extracted obligation, got %s", resp.Status)
	}
}

// TestCreateObligation_RejectsNonContractualType proves obligation-tracking-svc
// no longer accepts statutory/regulatory/internal-policy obligations —
// per docs/original_doc/zoiko_suite_doc4.txt:531, those route to
// obligations-svc; this service owns contract-derived obligations only.
func TestCreateObligation_RejectsNonContractualType(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateObligationRequest{
		LegalEntityID:  "le-001",
		Title:          "Annual Tax Filing",
		ObligationType: "STATUTORY",
		DueDate:        "2026-04-15",
		CreatedBy:      "user-001",
	}
	w := httptest.NewRecorder()
	h.CreateObligation(w, buildRequest(http.MethodPost, "/v1/obligations", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-contractual obligation_type, got %d — %s", w.Code, w.Body.String())
	}
}

// TestCreateObligation_RejectsInvalidSourceType proves a bare regulation
// source (no contract involved) is refused — that belongs in obligations-svc.
func TestCreateObligation_RejectsInvalidSourceType(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateObligationRequest{
		LegalEntityID:  "le-001",
		SourceType:     "REGULATION",
		Title:          "Some obligation",
		ObligationType: domain.ObligationTypeContractual,
		DueDate:        "2026-04-15",
		CreatedBy:      "user-001",
	}
	w := httptest.NewRecorder()
	h.CreateObligation(w, buildRequest(http.MethodPost, "/v1/obligations", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for source_type=REGULATION, got %d — %s", w.Code, w.Body.String())
	}
}

func TestObligationFullLifecycle_ReachesSatisfied(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	body := domain.CreateObligationRequest{
		LegalEntityID:  "le-001",
		SourceType:     "CLAUSE",
		SourceID:       "clause-456",
		Title:          "Quarterly deliverable review",
		ObligationType: domain.ObligationTypeContractual,
		DueDate:        "2026-03-31",
		CreatedBy:      "user-001",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/obligations", body))
	var created domain.Obligation
	_ = json.NewDecoder(wCreate.Body).Decode(&created)
	if created.Status != domain.ObligationStatusPlanned {
		t.Fatalf("setup: expected PLANNED, got %s", created.Status)
	}

	wSchedule := httptest.NewRecorder()
	r.ServeHTTP(wSchedule, buildRequest(http.MethodPost, "/v1/obligations/"+created.ObligationID+"/schedule",
		domain.ScheduleObligationRequest{TriggerDescription: "Contract signature date + 90 days", CalculationMethod: "fixed offset", ScheduledBy: "legal-ops-1"}))
	if wSchedule.Code != http.StatusOK {
		t.Fatalf("schedule: expected 200, got %d — %s", wSchedule.Code, wSchedule.Body.String())
	}

	wComplete := httptest.NewRecorder()
	r.ServeHTTP(wComplete, buildRequest(http.MethodPost, "/v1/obligations/"+created.ObligationID+"/complete", nil))
	if wComplete.Code != http.StatusOK {
		t.Fatalf("complete: expected 200, got %d — %s", wComplete.Code, wComplete.Body.String())
	}
	var satisfied domain.Obligation
	_ = json.NewDecoder(wComplete.Body).Decode(&satisfied)
	if satisfied.Status != domain.ObligationStatusSatisfied {
		t.Errorf("expected SATISFIED, got %s", satisfied.Status)
	}
}

// Schedule refuses an ambiguous due-date basis (LEG-07 §9: "trigger/date
// ambiguity blocks due-date certification").
func TestSchedule_MissingTriggerOrCalculation_Returns400(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/obligations", domain.CreateObligationRequest{
		LegalEntityID: "le-001", Title: "T", ObligationType: domain.ObligationTypeContractual,
		DueDate: "2026-03-31", CreatedBy: "user-001",
	}))
	var created domain.Obligation
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/obligations/"+created.ObligationID+"/schedule",
		domain.ScheduleObligationRequest{ScheduledBy: "legal-ops-1"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for ambiguous due-date basis, got %d — %s", w.Code, w.Body.String())
	}
}

// Waive requires documented authority (LEG-07 §9.1).
func TestWaive_MissingAuthorityReference_Returns400(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/obligations", domain.CreateObligationRequest{
		LegalEntityID: "le-001", Title: "T", ObligationType: domain.ObligationTypeContractual,
		DueDate: "2026-03-31", CreatedBy: "user-001",
	}))
	var created domain.Obligation
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	wSchedule := httptest.NewRecorder()
	r.ServeHTTP(wSchedule, buildRequest(http.MethodPost, "/v1/obligations/"+created.ObligationID+"/schedule",
		domain.ScheduleObligationRequest{TriggerDescription: "x", CalculationMethod: "y", ScheduledBy: "legal-ops-1"}))
	if wSchedule.Code != http.StatusOK {
		t.Fatalf("setup: schedule expected 200, got %d — %s", wSchedule.Code, wSchedule.Body.String())
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/obligations/"+created.ObligationID+"/waive",
		domain.WaiveObligationRequest{WaivedBy: "legal-ops-1"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a waiver with no authority reference, got %d — %s", w.Code, w.Body.String())
	}
}

// ValidateExtractedObligation is required before a CANDIDATE obligation may
// be scheduled or otherwise acted on.
func TestSchedule_OnUnvalidatedCandidate_Returns409(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/obligations", domain.CreateObligationRequest{
		LegalEntityID: "le-001", Title: "T", ObligationType: domain.ObligationTypeContractual,
		DueDate: "2026-03-31", ExtractedByAI: true, CreatedBy: "ai-extraction-pipeline",
	}))
	var created domain.Obligation
	_ = json.NewDecoder(wCreate.Body).Decode(&created)
	if created.Status != domain.ObligationStatusCandidate {
		t.Fatalf("setup: expected CANDIDATE, got %s", created.Status)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/obligations/"+created.ObligationID+"/schedule",
		domain.ScheduleObligationRequest{TriggerDescription: "x", CalculationMethod: "y", ScheduledBy: "legal-ops-1"}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 scheduling an unvalidated candidate, got %d — %s", w.Code, w.Body.String())
	}
}
