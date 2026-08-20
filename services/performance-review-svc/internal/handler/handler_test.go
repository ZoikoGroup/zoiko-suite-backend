package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/performance-review-svc/internal/clients"
	"zoiko.io/performance-review-svc/internal/domain"
	"zoiko.io/performance-review-svc/internal/handler"
	"zoiko.io/performance-review-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	cyclesByID           map[string]*domain.ReviewCycle
	cyclesByCorrelation  map[string]*domain.ReviewCycle
	reviewsByID          map[string]*domain.ReviewRecord
	reviewsByCorrelation map[string]*domain.ReviewRecord
}

func newStubStore() *stubStore {
	return &stubStore{
		cyclesByID:           make(map[string]*domain.ReviewCycle),
		cyclesByCorrelation:  make(map[string]*domain.ReviewCycle),
		reviewsByID:          make(map[string]*domain.ReviewRecord),
		reviewsByCorrelation: make(map[string]*domain.ReviewRecord),
	}
}

func (s *stubStore) CreateCycle(_ context.Context, c *domain.ReviewCycle) (bool, error) {
	if existing, ok := s.cyclesByCorrelation[c.CorrelationID]; ok {
		*c = *existing
		return false, nil
	}
	cp := *c
	s.cyclesByID[c.CycleID] = &cp
	s.cyclesByCorrelation[c.CorrelationID] = &cp
	return true, nil
}

func (s *stubStore) GetCycle(_ context.Context, cycleID string) (*domain.ReviewCycle, error) {
	c, ok := s.cyclesByID[cycleID]
	if !ok {
		return nil, domain.ErrCycleNotFound
	}
	cp := *c
	return &cp, nil
}

func (s *stubStore) ListCycles(_ context.Context, legalEntityID, status string) ([]domain.ReviewCycle, error) {
	var out []domain.ReviewCycle
	for _, c := range s.cyclesByID {
		out = append(out, *c)
	}
	return out, nil
}

func (s *stubStore) CloseCycle(_ context.Context, cycleID string) (*domain.ReviewCycle, error) {
	c, ok := s.cyclesByID[cycleID]
	if !ok {
		return nil, domain.ErrCycleNotFound
	}
	if c.Status != domain.CycleStatusOpen {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	c.Status = domain.CycleStatusClosed
	c.ClosedAt = &now
	c.UpdatedAt = now
	cp := *c
	return &cp, nil
}

func (s *stubStore) CreateReview(_ context.Context, r *domain.ReviewRecord) (bool, error) {
	if existing, ok := s.reviewsByCorrelation[r.CorrelationID]; ok {
		*r = *existing
		return false, nil
	}
	cp := *r
	s.reviewsByID[r.ReviewID] = &cp
	s.reviewsByCorrelation[r.CorrelationID] = &cp
	return true, nil
}

func (s *stubStore) GetReview(_ context.Context, reviewID string) (*domain.ReviewRecord, error) {
	r, ok := s.reviewsByID[reviewID]
	if !ok {
		return nil, domain.ErrReviewNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *stubStore) ListReviews(_ context.Context, legalEntityID, cycleID, employeeID, status string) ([]domain.ReviewRecord, error) {
	var out []domain.ReviewRecord
	for _, r := range s.reviewsByID {
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) SubmitReview(_ context.Context, reviewID string, rating int, comments string) (*domain.ReviewRecord, error) {
	r, ok := s.reviewsByID[reviewID]
	if !ok {
		return nil, domain.ErrReviewNotFound
	}
	if r.Status != domain.ReviewStatusDraft {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	r.Status = domain.ReviewStatusSubmitted
	r.Rating = &rating
	r.Comments = comments
	r.SubmittedAt = &now
	r.UpdatedAt = now
	cp := *r
	return &cp, nil
}

func (s *stubStore) CompleteReview(_ context.Context, reviewID string) (*domain.ReviewRecord, error) {
	r, ok := s.reviewsByID[reviewID]
	if !ok {
		return nil, domain.ErrReviewNotFound
	}
	if r.Status != domain.ReviewStatusSubmitted {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	r.Status = domain.ReviewStatusCompleted
	r.CompletedAt = &now
	r.UpdatedAt = now
	cp := *r
	return &cp, nil
}

type stubPublisher struct {
	created, completed int
}

func (p *stubPublisher) PublishReviewCreated(_ context.Context, _ domain.ReviewRecord) { p.created++ }
func (p *stubPublisher) PublishReviewCompleted(_ context.Context, _ string, _ domain.ReviewRecord) {
	p.completed++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubEmployeeVerifier struct {
	employee *clients.Employee
	err      error
}

func (e *stubEmployeeVerifier) GetEmployee(_ context.Context, _, _, _ string) (*clients.Employee, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.employee, nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, emp *stubEmployeeVerifier) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, emp, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func createCycleBody(correlationID string) map[string]any {
	return map[string]any{
		"legal_entity_id": "le-us",
		"cycle_name":      "2026 Annual Review",
		"period_start":    "2026-01-01",
		"period_end":      "2026-12-31",
		"correlation_id":  correlationID,
	}
}

func createOpenCycle(t *testing.T, r chi.Router) domain.ReviewCycle {
	rr := doReq(r, http.MethodPost, "/v1/review-cycles/", createCycleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("cycle setup failed: %d %s", rr.Code, rr.Body.String())
	}
	var c domain.ReviewCycle
	_ = json.NewDecoder(rr.Body).Decode(&c)
	return c
}

// ── CreateCycle tests ──────────────────────────────────────────────────────────

func TestCreateCycle_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeVerifier{})
	rr := doReq(r, http.MethodPost, "/v1/review-cycles/", createCycleBody(uuid.NewString()), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateCycle_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeVerifier{})
	c := createOpenCycle(t, r)
	if c.Status != domain.CycleStatusOpen {
		t.Errorf("expected OPEN got %q", c.Status)
	}
}

// ── CreateReview tests ─────────────────────────────────────────────────────────

func reviewBody(cycleID, correlationID string) map[string]any {
	return map[string]any{
		"legal_entity_id":       "le-us",
		"cycle_id":              cycleID,
		"employee_id":           "emp-1",
		"reviewer_principal_id": "manager-1",
		"correlation_id":        correlationID,
	}
}

func TestCreateReview_EmployeeNotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeVerifier{employee: nil})
	cycle := createOpenCycle(t, r)

	rr := doReq(r, http.MethodPost, "/v1/review-records/", reviewBody(cycle.CycleID, uuid.NewString()), "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateReview_EmployeeServiceUnavailable(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeVerifier{err: domain.ErrEmployeeServiceUnavailable})
	cycle := createOpenCycle(t, r)

	rr := doReq(r, http.MethodPost, "/v1/review-records/", reviewBody(cycle.CycleID, uuid.NewString()), "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateReview_CycleNotOpen(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeVerifier{employee: &clients.Employee{EmployeeID: "emp-1"}})
	cycle := createOpenCycle(t, r)
	closeRR := doReq(r, http.MethodPost, "/v1/review-cycles/"+cycle.CycleID+"/close", nil, "admin-1")
	if closeRR.Code != http.StatusOK {
		t.Fatalf("close setup failed: %d", closeRR.Code)
	}

	rr := doReq(r, http.MethodPost, "/v1/review-records/", reviewBody(cycle.CycleID, uuid.NewString()), "principal-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateReview_HappyPath(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubEmployeeVerifier{employee: &clients.Employee{EmployeeID: "emp-1"}})
	cycle := createOpenCycle(t, r)

	rr := doReq(r, http.MethodPost, "/v1/review-records/", reviewBody(cycle.CycleID, uuid.NewString()), "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var rev domain.ReviewRecord
	_ = json.NewDecoder(rr.Body).Decode(&rev)
	if rev.Status != domain.ReviewStatusDraft {
		t.Errorf("expected DRAFT got %q", rev.Status)
	}
	if pub.created != 1 {
		t.Errorf("expected 1 review.created event, got %d", pub.created)
	}
}

func TestCreateReview_IdempotentReplay(t *testing.T) {
	correlationID := uuid.NewString()
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeVerifier{employee: &clients.Employee{EmployeeID: "emp-1"}})
	cycle := createOpenCycle(t, r)

	rr1 := doReq(r, http.MethodPost, "/v1/review-records/", reviewBody(cycle.CycleID, correlationID), "principal-1")
	var rev1 domain.ReviewRecord
	_ = json.NewDecoder(rr1.Body).Decode(&rev1)

	rr2 := doReq(r, http.MethodPost, "/v1/review-records/", reviewBody(cycle.CycleID, correlationID), "principal-1")
	var rev2 domain.ReviewRecord
	_ = json.NewDecoder(rr2.Body).Decode(&rev2)

	if rev2.ReviewID != rev1.ReviewID {
		t.Fatalf("retried create resolved to a different review_id (%s) than the original (%s)", rev2.ReviewID, rev1.ReviewID)
	}
}

// ── submit / complete tests ────────────────────────────────────────────────────

func createDraftReview(t *testing.T, r chi.Router, cycleID string) domain.ReviewRecord {
	rr := doReq(r, http.MethodPost, "/v1/review-records/", reviewBody(cycleID, uuid.NewString()), "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("review setup failed: %d %s", rr.Code, rr.Body.String())
	}
	var rev domain.ReviewRecord
	_ = json.NewDecoder(rr.Body).Decode(&rev)
	return rev
}

func TestSubmitReview_InvalidRating(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeVerifier{employee: &clients.Employee{EmployeeID: "emp-1"}})
	cycle := createOpenCycle(t, r)
	rev := createDraftReview(t, r, cycle.CycleID)

	rr := doReq(r, http.MethodPost, "/v1/review-records/"+rev.ReviewID+"/submit", map[string]any{"rating": 9, "comments": "great work"}, "manager-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestSubmitAndCompleteReview_HappyPath(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubEmployeeVerifier{employee: &clients.Employee{EmployeeID: "emp-1"}})
	cycle := createOpenCycle(t, r)
	rev := createDraftReview(t, r, cycle.CycleID)

	submitRR := doReq(r, http.MethodPost, "/v1/review-records/"+rev.ReviewID+"/submit", map[string]any{"rating": 4, "comments": "solid quarter"}, "manager-1")
	if submitRR.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", submitRR.Code, submitRR.Body.String())
	}

	completeRR := doReq(r, http.MethodPost, "/v1/review-records/"+rev.ReviewID+"/complete", nil, "manager-1")
	if completeRR.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", completeRR.Code, completeRR.Body.String())
	}
	var final domain.ReviewRecord
	_ = json.NewDecoder(completeRR.Body).Decode(&final)
	if final.Status != domain.ReviewStatusCompleted {
		t.Errorf("expected COMPLETED got %q", final.Status)
	}
	if final.Rating == nil || *final.Rating != 4 {
		t.Errorf("expected rating 4 to persist, got %v", final.Rating)
	}
	if pub.completed != 1 {
		t.Errorf("expected 1 review.completed event, got %d", pub.completed)
	}
}

func TestCompleteReview_RequiresSubmittedStatus(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeVerifier{employee: &clients.Employee{EmployeeID: "emp-1"}})
	cycle := createOpenCycle(t, r)
	rev := createDraftReview(t, r, cycle.CycleID) // still DRAFT, never submitted

	rr := doReq(r, http.MethodPost, "/v1/review-records/"+rev.ReviewID+"/complete", nil, "manager-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d: %s", rr.Code, rr.Body.String())
	}
}
