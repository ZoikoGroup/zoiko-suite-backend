package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/decision-support-svc/internal/clients"
	"zoiko.io/decision-support-svc/internal/domain"
	"zoiko.io/decision-support-svc/internal/handler"
	"zoiko.io/decision-support-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	byID          map[string]*domain.Recommendation
	byCorrelation map[string]*domain.Recommendation
}

func newStubStore() *stubStore {
	return &stubStore{byID: make(map[string]*domain.Recommendation), byCorrelation: make(map[string]*domain.Recommendation)}
}

func (s *stubStore) CreateRecommendation(_ context.Context, r *domain.Recommendation) (bool, error) {
	if existing, ok := s.byCorrelation[r.CorrelationID]; ok {
		*r = *existing
		return false, nil
	}
	cp := *r
	s.byID[r.RecommendationID] = &cp
	s.byCorrelation[r.CorrelationID] = &cp
	return true, nil
}

func (s *stubStore) GetRecommendation(_ context.Context, recommendationID string) (*domain.Recommendation, error) {
	r, ok := s.byID[recommendationID]
	if !ok {
		return nil, domain.ErrRecommendationNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *stubStore) ListRecommendations(_ context.Context, legalEntityID, subjectReference string) ([]domain.Recommendation, error) {
	var out []domain.Recommendation
	for _, r := range s.byID {
		out = append(out, *r)
	}
	return out, nil
}

type stubPublisher struct{ recommended int }

func (p *stubPublisher) PublishRecommended(_ context.Context, _ domain.Recommendation) {
	p.recommended++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubHistory struct {
	decisions []clients.PriorDecision
	err       error
}

func (h *stubHistory) ListPriorDecisions(_ context.Context, _, _, _ string, _ int) ([]clients.PriorDecision, error) {
	if h.err != nil {
		return nil, h.err
	}
	return h.decisions, nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, hist *stubHistory) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, hist, zap.NewNop())
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

func recRequestBody(correlationID string) map[string]any {
	return map[string]any{
		"legal_entity_id":   "le-us",
		"subject_type":      "purchase_order",
		"subject_reference": "po-123",
		"action_type":       "PO_ISSUE",
		"correlation_id":    correlationID,
	}
}

// ── RequestRecommendation tests ────────────────────────────────────────────────

func TestRequestRecommendation_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubHistory{})
	rr := doReq(r, http.MethodPost, "/v1/recommendations/", recRequestBody(uuid.NewString()), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestRequestRecommendation_HistoryUnavailable_DegradesGracefully(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubHistory{err: domain.ErrAuthzServiceUnavailable})
	rr := doReq(r, http.MethodPost, "/v1/recommendations/", recRequestBody(uuid.NewString()), "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 (degraded, not 503) got %d: %s", rr.Code, rr.Body.String())
	}
	var rec domain.Recommendation
	_ = json.NewDecoder(rr.Body).Decode(&rec)
	if rec.RecommendedAction != domain.RecommendedActionNoHistory {
		t.Errorf("expected NO_HISTORY got %q", rec.RecommendedAction)
	}
	if rec.ConfidenceScore != 0 {
		t.Errorf("expected 0 confidence got %v", rec.ConfidenceScore)
	}
}

func TestRequestRecommendation_NoPriorDecisions(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubHistory{decisions: []clients.PriorDecision{}})
	rr := doReq(r, http.MethodPost, "/v1/recommendations/", recRequestBody(uuid.NewString()), "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var rec domain.Recommendation
	_ = json.NewDecoder(rr.Body).Decode(&rec)
	if rec.RecommendedAction != domain.RecommendedActionNoHistory {
		t.Errorf("expected NO_HISTORY got %q", rec.RecommendedAction)
	}
}

func TestRequestRecommendation_MajorityGranted_RecommendsApprove(t *testing.T) {
	pub := &stubPublisher{}
	hist := &stubHistory{decisions: []clients.PriorDecision{
		{Outcome: "GRANTED"}, {Outcome: "GRANTED"}, {Outcome: "GRANTED"}, {Outcome: "DENIED"},
	}}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, hist)
	rr := doReq(r, http.MethodPost, "/v1/recommendations/", recRequestBody(uuid.NewString()), "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var rec domain.Recommendation
	_ = json.NewDecoder(rr.Body).Decode(&rec)
	if rec.RecommendedAction != domain.RecommendedActionApprove {
		t.Errorf("expected APPROVE got %q", rec.RecommendedAction)
	}
	if rec.ConfidenceScore != 0.75 {
		t.Errorf("expected confidence 0.75 got %v", rec.ConfidenceScore)
	}
	if rec.PriorDecisionsSampled != 4 {
		t.Errorf("expected 4 sampled got %d", rec.PriorDecisionsSampled)
	}
	if pub.recommended != 1 {
		t.Errorf("expected 1 decision_support.recommended event, got %d", pub.recommended)
	}
}

func TestRequestRecommendation_MajorityDenied_RecommendsReject(t *testing.T) {
	hist := &stubHistory{decisions: []clients.PriorDecision{
		{Outcome: "DENIED"}, {Outcome: "DENIED"}, {Outcome: "GRANTED"},
	}}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, hist)
	rr := doReq(r, http.MethodPost, "/v1/recommendations/", recRequestBody(uuid.NewString()), "principal-1")
	var rec domain.Recommendation
	_ = json.NewDecoder(rr.Body).Decode(&rec)
	if rec.RecommendedAction != domain.RecommendedActionReject {
		t.Errorf("expected REJECT got %q", rec.RecommendedAction)
	}
}

func TestRequestRecommendation_IdempotentReplay(t *testing.T) {
	correlationID := uuid.NewString()
	hist := &stubHistory{decisions: []clients.PriorDecision{{Outcome: "GRANTED"}}}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, hist)

	rr1 := doReq(r, http.MethodPost, "/v1/recommendations/", recRequestBody(correlationID), "principal-1")
	var rec1 domain.Recommendation
	_ = json.NewDecoder(rr1.Body).Decode(&rec1)

	rr2 := doReq(r, http.MethodPost, "/v1/recommendations/", recRequestBody(correlationID), "principal-1")
	var rec2 domain.Recommendation
	_ = json.NewDecoder(rr2.Body).Decode(&rec2)

	if rec2.RecommendationID != rec1.RecommendationID {
		t.Fatalf("retried request resolved to a different recommendation_id (%s) than the original (%s)", rec2.RecommendationID, rec1.RecommendationID)
	}
}
