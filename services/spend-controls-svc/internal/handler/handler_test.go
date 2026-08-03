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
	"go.uber.org/zap"

	"zoiko.io/spend-controls-svc/internal/domain"
	"zoiko.io/spend-controls-svc/internal/handler"
	"zoiko.io/spend-controls-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	policies     map[string]*domain.SpendPolicy
	consumptions map[string]*domain.SpendConsumption // keyed by correlation_id
}

func newStubStore() *stubStore {
	return &stubStore{
		policies:     make(map[string]*domain.SpendPolicy),
		consumptions: make(map[string]*domain.SpendConsumption),
	}
}

func (s *stubStore) CreatePolicy(_ context.Context, p *domain.SpendPolicy) error {
	s.policies[p.SpendPolicyID] = p
	return nil
}

func (s *stubStore) ListPolicies(_ context.Context, legalEntityID, category string) ([]domain.SpendPolicy, error) {
	var out []domain.SpendPolicy
	for _, p := range s.policies {
		if legalEntityID != "" && p.LegalEntityID != legalEntityID {
			continue
		}
		if category != "" && p.Category != category {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *stubStore) FindActivePolicy(_ context.Context, legalEntityID, category string) (*domain.SpendPolicy, error) {
	for _, p := range s.policies {
		if p.LegalEntityID == legalEntityID && p.Category == category && p.ActiveFlag {
			return p, nil
		}
	}
	return nil, nil
}

func (s *stubStore) SumConsumption(_ context.Context, spendPolicyID string, since time.Time) (float64, error) {
	var total float64
	for _, c := range s.consumptions {
		if c.SpendPolicyID == spendPolicyID && c.DecisionOutcome == "ALLOWED" && !c.RecordedAt.Before(since) {
			total += c.Amount
		}
	}
	return total, nil
}

func (s *stubStore) FindConsumptionByCorrelation(_ context.Context, correlationID string) (*domain.SpendConsumption, error) {
	if c, ok := s.consumptions[correlationID]; ok {
		return c, nil
	}
	return nil, nil
}

func (s *stubStore) RecordConsumption(_ context.Context, c *domain.SpendConsumption) (bool, error) {
	if existing, ok := s.consumptions[c.CorrelationID]; ok {
		*c = *existing
		return false, nil
	}
	s.consumptions[c.CorrelationID] = c
	return true, nil
}

func (s *stubStore) ListConsumptions(_ context.Context, legalEntityID, spendPolicyID string) ([]domain.SpendConsumption, error) {
	var out []domain.SpendConsumption
	for _, c := range s.consumptions {
		if legalEntityID != "" && c.LegalEntityID != legalEntityID {
			continue
		}
		if spendPolicyID != "" && c.SpendPolicyID != spendPolicyID {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

type stubPublisher struct {
	breached, blocked int
}

func (p *stubPublisher) PublishThresholdBreached(_ context.Context, _ string, _ domain.SpendCheckRequest, _ domain.SpendPolicy, _ float64) {
	p.breached++
}
func (p *stubPublisher) PublishBlockApplied(_ context.Context, _ string, _ domain.SpendCheckRequest, _ domain.SpendPolicy) {
	p.blocked++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, zap.NewNop())
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

// ── CreatePolicy Tests ─────────────────────────────────────────────────────────

func TestCreatePolicy_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", map[string]any{
		"legal_entity_id":  "le-us",
		"category":         "PROCUREMENT",
		"period":           "MONTHLY",
		"threshold_amount": 10000.0,
		"currency_code":    "USD",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreatePolicy_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", map[string]any{
		"legal_entity_id":  "le-us",
		"category":         "PROCUREMENT",
		"period":           "MONTHLY",
		"threshold_amount": 10000.0,
		"currency_code":    "USD",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestCreatePolicy_InvalidPeriod(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", map[string]any{
		"legal_entity_id":  "le-us",
		"category":         "PROCUREMENT",
		"period":           "WEEKLY",
		"threshold_amount": 10000.0,
		"currency_code":    "USD",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestCreatePolicy_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", map[string]any{
		"legal_entity_id":  "le-us",
		"category":         "PROCUREMENT",
		"period":           "MONTHLY",
		"threshold_amount": 10000.0,
		"currency_code":    "USD",
	}, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var p domain.SpendPolicy
	if err := json.NewDecoder(rr.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !p.ActiveFlag {
		t.Error("expected new policy to be active")
	}
}

// ── SubmitCheck Tests ──────────────────────────────────────────────────────────

func createPolicy(t *testing.T, r chi.Router, threshold float64, period string) domain.SpendPolicy {
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", map[string]any{
		"legal_entity_id":  "le-us",
		"category":         "PROCUREMENT",
		"period":           period,
		"threshold_amount": threshold,
		"currency_code":    "USD",
	}, "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("policy setup failed: %d %s", rr.Code, rr.Body.String())
	}
	var p domain.SpendPolicy
	_ = json.NewDecoder(rr.Body).Decode(&p)
	return p
}

func TestSubmitCheck_NoPolicyConfigured(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/spend-checks/", map[string]any{
		"legal_entity_id": "le-us",
		"category":        "TRAVEL", // no policy exists for this category
		"amount":          500.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-1",
	}, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.SpendCheckResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.DecisionOutcome != "ALLOWED" {
		t.Errorf("expected ALLOWED got %q", resp.DecisionOutcome)
	}
	if resp.DecisionBasis != "no_policy_configured" {
		t.Errorf("expected no_policy_configured got %q", resp.DecisionBasis)
	}
}

func TestSubmitCheck_WithinThreshold(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	createPolicy(t, r, 10000.0, "MONTHLY")

	rr := doReq(r, http.MethodPost, "/v1/spend-checks/", map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          2000.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-2",
	}, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.SpendCheckResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.DecisionOutcome != "ALLOWED" {
		t.Errorf("expected ALLOWED got %q", resp.DecisionOutcome)
	}
	if resp.ConsumptionID == "" {
		t.Error("expected a consumption_id to be recorded")
	}
	if pub.breached != 0 || pub.blocked != 0 {
		t.Error("expected no breach events on an allowed check")
	}
}

func TestSubmitCheck_ExceedsThreshold_Blocked(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	createPolicy(t, r, 1000.0, "MONTHLY")

	rr := doReq(r, http.MethodPost, "/v1/spend-checks/", map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          5000.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-3",
	}, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var resp domain.SpendCheckResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.DecisionOutcome != "BLOCKED" {
		t.Errorf("expected BLOCKED got %q", resp.DecisionOutcome)
	}
	if resp.ConsumptionID != "" {
		t.Error("blocked spend must not be recorded as consumption")
	}
	if pub.breached != 1 || pub.blocked != 1 {
		t.Errorf("expected 1 breach + 1 block event, got breached=%d blocked=%d", pub.breached, pub.blocked)
	}

	// A second, smaller request on a fresh correlation_id must be evaluated
	// independently — proving the first BLOCKED attempt did not silently
	// consume budget it was never granted.
	rr2 := doReq(r, http.MethodPost, "/v1/spend-checks/", map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          500.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-4",
	}, "principal-1")
	var resp2 domain.SpendCheckResponse
	_ = json.NewDecoder(rr2.Body).Decode(&resp2)
	if resp2.DecisionOutcome != "ALLOWED" {
		t.Errorf("expected the smaller follow-up request to be ALLOWED (a blocked attempt must not consume budget), got %q", resp2.DecisionOutcome)
	}
}

func TestSubmitCheck_IdempotentReplay(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	createPolicy(t, r, 10000.0, "MONTHLY")

	body := map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          2000.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-retry",
	}

	rr1 := doReq(r, http.MethodPost, "/v1/spend-checks/", body, "principal-1")
	var resp1 domain.SpendCheckResponse
	_ = json.NewDecoder(rr1.Body).Decode(&resp1)

	rr2 := doReq(r, http.MethodPost, "/v1/spend-checks/", body, "principal-1")
	var resp2 domain.SpendCheckResponse
	_ = json.NewDecoder(rr2.Body).Decode(&resp2)

	if resp2.ConsumptionID != resp1.ConsumptionID {
		t.Fatalf("retried check resolved to a different consumption_id (%s) than the original (%s)", resp2.ConsumptionID, resp1.ConsumptionID)
	}
	if resp2.DecisionBasis != "replayed_prior_decision" {
		t.Errorf("expected replay basis on retry, got %q", resp2.DecisionBasis)
	}

	// Consumption must have been recorded exactly once — a naive
	// re-evaluation on retry would double count the same spend.
	consumptions, _ := s.ListConsumptions(context.Background(), "le-us", "")
	if len(consumptions) != 1 {
		t.Fatalf("expected exactly 1 consumption record, got %d", len(consumptions))
	}
}
