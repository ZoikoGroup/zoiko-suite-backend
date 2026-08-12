package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/spend-controls-svc/internal/domain"
	"zoiko.io/spend-controls-svc/internal/handler"
	"zoiko.io/spend-controls-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	policies     map[string]*domain.SpendPolicy
	consumptions map[string]*domain.SpendConsumption // keyed by correlation_id

	// tenantMissing makes every method behave as the real store does when no
	// tenant scope reached it.
	tenantMissing bool
	failWith      error
}

func newStubStore() *stubStore {
	return &stubStore{
		policies:     make(map[string]*domain.SpendPolicy),
		consumptions: make(map[string]*domain.SpendConsumption),
	}
}

func (s *stubStore) guard() error {
	if s.tenantMissing {
		return domain.ErrTenantMissing
	}
	return s.failWith
}

func (s *stubStore) CreatePolicy(_ context.Context, p *domain.SpendPolicy) (int, error) {
	if err := s.guard(); err != nil {
		return 0, err
	}
	// Mirrors the real store: end-date any limit this one replaces.
	superseded := 0
	for _, existing := range s.policies {
		if existing.LegalEntityID == p.LegalEntityID && existing.Category == p.Category && existing.ActiveFlag {
			existing.ActiveFlag = false
			superseded++
		}
	}
	s.policies[p.SpendPolicyID] = p
	return superseded, nil
}

func (s *stubStore) DeactivatePolicy(_ context.Context, spendPolicyID string) error {
	if err := s.guard(); err != nil {
		return err
	}
	p, ok := s.policies[spendPolicyID]
	if !ok || !p.ActiveFlag {
		return domain.ErrPolicyNotFound
	}
	p.ActiveFlag = false
	return nil
}

func (s *stubStore) ListPolicies(_ context.Context, legalEntityID, category string, activeOnly bool) ([]domain.SpendPolicy, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var out []domain.SpendPolicy
	for _, p := range s.policies {
		if legalEntityID != "" && p.LegalEntityID != legalEntityID {
			continue
		}
		if category != "" && p.Category != category {
			continue
		}
		if activeOnly && !p.ActiveFlag {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

// EvaluateSpend mirrors the real store's semantics: replay wins, currency must
// match, only ALLOWED rows in the matching currency count toward the total, and
// both outcomes are recorded.
func (s *stubStore) EvaluateSpend(_ context.Context, in domain.SpendEvaluation) (*domain.SpendDecision, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}

	if existing, ok := s.consumptions[in.CorrelationID]; ok {
		d := &domain.SpendDecision{
			Outcome:        existing.DecisionOutcome,
			Basis:          "replayed_prior_decision",
			ConsumptionID:  existing.ConsumptionID,
			Replayed:       true,
			ProjectedTotal: existing.Amount,
		}
		for _, p := range s.policies {
			if p.SpendPolicyID == existing.SpendPolicyID {
				d.Policy = p
			}
		}
		return d, nil
	}

	var policy *domain.SpendPolicy
	for _, p := range s.policies {
		if p.LegalEntityID == in.LegalEntityID && p.Category == in.Category && p.ActiveFlag {
			policy = p
		}
	}
	if policy == nil {
		return &domain.SpendDecision{
			Outcome:        "ALLOWED",
			Basis:          "no_policy_configured",
			ProjectedTotal: in.Amount,
		}, nil
	}
	if policy.CurrencyCode != in.CurrencyCode {
		return nil, domain.ErrCurrencyMismatch
	}

	var prior float64
	if policy.Period != "PER_TRANSACTION" {
		for _, c := range s.consumptions {
			if c.SpendPolicyID == policy.SpendPolicyID &&
				c.DecisionOutcome == "ALLOWED" &&
				c.CurrencyCode == in.CurrencyCode {
				prior += c.Amount
			}
		}
	}
	projected := prior + in.Amount

	d := &domain.SpendDecision{
		Policy:           policy,
		PriorConsumption: prior,
		ProjectedTotal:   projected,
		ConsumptionID:    uuid.NewString(),
	}
	if projected > policy.ThresholdAmount {
		d.Outcome, d.Basis = "BLOCKED", "threshold_exceeded"
	} else {
		d.Outcome, d.Basis = "ALLOWED", "within_threshold"
	}

	s.consumptions[in.CorrelationID] = &domain.SpendConsumption{
		ConsumptionID:   d.ConsumptionID,
		LegalEntityID:   in.LegalEntityID,
		SpendPolicyID:   policy.SpendPolicyID,
		Amount:          in.Amount,
		CurrencyCode:    in.CurrencyCode,
		SourceReference: in.SourceReference,
		CorrelationID:   in.CorrelationID,
		DecisionOutcome: d.Outcome,
		RecordedAt:      time.Now().UTC(),
	}
	return d, nil
}

func (s *stubStore) PolicyUsageTotals(_ context.Context, legalEntityID, category string) ([]domain.PolicyUsageTotal, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var out []domain.PolicyUsageTotal
	for _, p := range s.policies {
		if !p.ActiveFlag {
			continue
		}
		if legalEntityID != "" && p.LegalEntityID != legalEntityID {
			continue
		}
		if category != "" && p.Category != category {
			continue
		}
		t := domain.PolicyUsageTotal{SpendPolicyID: p.SpendPolicyID}
		for _, c := range s.consumptions {
			if c.SpendPolicyID != p.SpendPolicyID {
				continue
			}
			if c.DecisionOutcome == "BLOCKED" {
				t.RefusedCount++
				continue
			}
			if c.CurrencyCode == p.CurrencyCode {
				t.Consumed += c.Amount
			}
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *stubStore) ListConsumptions(_ context.Context, legalEntityID, spendPolicyID string) ([]domain.SpendConsumption, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
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

type stubAuthZ struct {
	err error
	// scopes records what each check was asked to authorize against, so a test
	// can assert the check happened at all — and against what.
	scopes []string
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, scope, _ string) error {
	a.scopes = append(a.scopes, scope)
	return a.err
}

// ── router factory ─────────────────────────────────────────────────────────────

const testTenant = "tenant-abc"

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	return newRouterWithTenant(s, pub, authz, testTenant)
}

// newRouterWithTenant allows an empty tenant, which is what a request arriving
// without X-Tenant-Id looks like to the handler.
func newRouterWithTenant(s *stubStore, pub *stubPublisher, authz *stubAuthZ, tenant string) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if tenant != "" {
				req = req.WithContext(middleware.WithTenant(req.Context(), tenant))
			}
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
	return doRaw(r, method, path, buf.String(), principalID)
}

// doRaw posts a body verbatim, so a test can send JSON no Go struct would
// produce — a misspelled key, an oversized payload.
func doRaw(r chi.Router, method, path, body, principalID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func errorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %s", rr.Body.String())
	}
	return body.Error
}

func decodeCheck(t *testing.T, rr *httptest.ResponseRecorder) domain.SpendCheckResponse {
	t.Helper()
	var resp domain.SpendCheckResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not a check response: %s", rr.Body.String())
	}
	return resp
}

func policyBody(threshold float64, period, currency string) map[string]any {
	return map[string]any{
		"legal_entity_id":  "le-us",
		"category":         "PROCUREMENT",
		"period":           period,
		"threshold_amount": threshold,
		"currency_code":    currency,
	}
}

// ── the error body shape ───────────────────────────────────────────────────────

// This service used to answer {"error_code":…,"error_message":…}, unique in the
// suite. The admin console parses `error`/`detail`/`field`/`message`, so every
// failure reached the UI as a bare status code with no explanation at all.
func TestErrorBody_UsesPlatformKeys(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(10000, "WEEKLY", "USD"), "principal-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("not JSON: %s", rr.Body.String())
	}
	if _, ok := raw["error"]; !ok {
		t.Fatalf(`error body must carry an "error" key, got %v`, raw)
	}
	if _, gone := raw["error_code"]; gone {
		t.Fatalf(`error body must no longer carry "error_code", got %v`, raw)
	}
	if raw["error"] != "invalid_period" {
		t.Fatalf(`expected error "invalid_period", got %v`, raw["error"])
	}
}

// ── tenant scope ───────────────────────────────────────────────────────────────

// A request with no X-Tenant-Id used to reach the store, be rejected there, and
// be reported as 503 store_unavailable — so a missing header read as a database
// outage.
func TestMissingTenant_Is401Not503(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"create policy", http.MethodPost, "/v1/spend-policies/", policyBody(10000, "MONTHLY", "USD")},
		{"list policies", http.MethodGet, "/v1/spend-policies/?legal_entity_id=le-us", nil},
		{"list consumptions", http.MethodGet, "/v1/spend-consumptions/?legal_entity_id=le-us", nil},
		{"submit check", http.MethodPost, "/v1/spend-checks/", map[string]any{
			"legal_entity_id": "le-us", "category": "PROCUREMENT",
			"amount": 10.0, "currency_code": "USD", "correlation_id": "c1",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubStore()
			s.tenantMissing = true
			r := newRouterWithTenant(s, &stubPublisher{}, &stubAuthZ{}, "")

			rr := doReq(r, tc.method, tc.path, tc.body, "principal-1")

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
			}
			if code := errorCode(t, rr); code != "identity_missing" {
				t.Fatalf("expected identity_missing, got %q", code)
			}
		})
	}
}

// A genuine store failure must still be a 503 — the point of the mapping is to
// stop over-reporting outages, not to stop reporting them.
func TestGenuineStoreFailure_Still503(t *testing.T) {
	s := newStubStore()
	s.failWith = errors.New("connection refused")
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(10000, "MONTHLY", "USD"), "principal-1")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	// The raw driver text must not be echoed to the caller.
	if strings.Contains(rr.Body.String(), "connection refused") {
		t.Fatalf("internal error text leaked to the client: %s", rr.Body.String())
	}
}

// ── read authorization ─────────────────────────────────────────────────────────

// The authz check used to run only when legal_entity_id was supplied, so omitting
// one optional query parameter skipped authorization entirely and returned every
// policy in the tenant to a principal holding no view grant.
func TestReads_AuthorizeEvenWithoutLegalEntityFilter(t *testing.T) {
	for _, path := range []string{"/v1/spend-policies/", "/v1/spend-consumptions/"} {
		t.Run(path, func(t *testing.T) {
			authz := &stubAuthZ{err: domain.ErrAuthorizationDenied}
			r := newRouter(newStubStore(), &stubPublisher{}, authz)

			rr := doReq(r, http.MethodGet, path, nil, "principal-1")

			if rr.Code != http.StatusForbidden {
				t.Fatalf("an unscoped read must still be authorized; got %d", rr.Code)
			}
			if len(authz.scopes) != 1 {
				t.Fatalf("expected exactly one authorization check, got %d", len(authz.scopes))
			}
			// With no entity named, the tenant is the scope the caller must hold
			// the grant on.
			if authz.scopes[0] != testTenant {
				t.Fatalf("expected the tenant as the fallback scope, got %q", authz.scopes[0])
			}
		})
	}
}

func TestReads_AuthorizeAgainstTheNamedEntity(t *testing.T) {
	authz := &stubAuthZ{}
	r := newRouter(newStubStore(), &stubPublisher{}, authz)

	rr := doReq(r, http.MethodGet, "/v1/spend-policies/?legal_entity_id=le-us", nil, "principal-1")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(authz.scopes) != 1 || authz.scopes[0] != "le-us" {
		t.Fatalf("expected the named entity as the scope, got %v", authz.scopes)
	}
}

// ── decoder strictness ─────────────────────────────────────────────────────────

// source_reference is optional, so a misspelling was silently discarded and the
// check succeeded with no reference to the thing that caused the spend.
func TestSubmitCheck_UnknownField_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	body := `{"legal_entity_id":"le-us","category":"PROCUREMENT","amount":10,
	          "currency_code":"USD","correlation_id":"c1","source_refrence":"PO-1"}`

	rr := doRaw(r, http.MethodPost, "/v1/spend-checks/", body, "principal-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "unknown_field" {
		t.Fatalf("expected unknown_field, got %q", code)
	}
}

func TestSubmitCheck_OversizedBody_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	body := `{"legal_entity_id":"` + strings.Repeat("x", 70<<10) + `"}`

	rr := doRaw(r, http.MethodPost, "/v1/spend-checks/", body, "principal-1")

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rr.Code)
	}
}

// ── currency ───────────────────────────────────────────────────────────────────

// The threshold and the amount used to be compared as bare numbers regardless of
// currency, so a 9,000 USD spend passed a 10,000 GBP threshold and was booked
// against that budget. Nothing in this platform holds an FX rate.
func TestSubmitCheck_CurrencyMismatch_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	if rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(10000, "MONTHLY", "GBP"), "admin-1"); rr.Code != http.StatusCreated {
		t.Fatalf("policy setup failed: %s", rr.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/spend-checks/", map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          9000.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-fx",
	}, "principal-1")

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for a cross-currency check, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "currency_mismatch" {
		t.Fatalf("expected currency_mismatch, got %q", code)
	}
	// And nothing may have been booked against the GBP budget.
	consumptions, _ := s.ListConsumptions(context.Background(), "le-us", "")
	if len(consumptions) != 0 {
		t.Fatalf("a refused cross-currency check must record nothing, got %d rows", len(consumptions))
	}
}

// ── decisions ──────────────────────────────────────────────────────────────────

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
	resp := decodeCheck(t, rr)
	if resp.DecisionOutcome != "ALLOWED" {
		t.Errorf("expected ALLOWED got %q", resp.DecisionOutcome)
	}
	// "Nothing constrains this" is a distinct fact from "a policy permits it",
	// and the console renders them differently.
	if resp.DecisionBasis != "no_policy_configured" {
		t.Errorf("expected no_policy_configured got %q", resp.DecisionBasis)
	}
}

func TestSubmitCheck_WithinThreshold(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	if rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(10000, "MONTHLY", "USD"), "admin-1"); rr.Code != http.StatusCreated {
		t.Fatalf("policy setup failed: %s", rr.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/spend-checks/", map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          2000.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-2",
	}, "principal-1")

	resp := decodeCheck(t, rr)
	if resp.DecisionOutcome != "ALLOWED" || resp.DecisionBasis != "within_threshold" {
		t.Errorf("expected ALLOWED/within_threshold, got %q/%q", resp.DecisionOutcome, resp.DecisionBasis)
	}
	if resp.ConsumptionID == "" {
		t.Error("expected a consumption_id to be recorded")
	}
	// The figures the decision was made against must be reported, not left for
	// the reader to reconstruct.
	if resp.ThresholdAmount != 10000 || resp.ProjectedTotal != 2000 {
		t.Errorf("expected threshold 10000 and projected 2000, got %v and %v", resp.ThresholdAmount, resp.ProjectedTotal)
	}
	if resp.CurrencyCode != "USD" {
		t.Errorf("expected the decision to name its currency, got %q", resp.CurrencyCode)
	}
	if pub.breached != 0 || pub.blocked != 0 {
		t.Error("expected no breach events on an allowed check")
	}
}

func TestSubmitCheck_ExceedsThreshold_Blocked(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	if rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(1000, "MONTHLY", "USD"), "admin-1"); rr.Code != http.StatusCreated {
		t.Fatalf("policy setup failed: %s", rr.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/spend-checks/", map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          5000.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-3",
	}, "principal-1")

	resp := decodeCheck(t, rr)
	if resp.DecisionOutcome != "BLOCKED" {
		t.Errorf("expected BLOCKED got %q", resp.DecisionOutcome)
	}
	if resp.ProjectedTotal != 5000 || resp.ThresholdAmount != 1000 {
		t.Errorf("a refusal must state what it compared: projected %v vs threshold %v", resp.ProjectedTotal, resp.ThresholdAmount)
	}
	if pub.breached != 1 || pub.blocked != 1 {
		t.Errorf("expected 1 breach + 1 block event, got breached=%d blocked=%d", pub.breached, pub.blocked)
	}

	// A blocked attempt IS recorded now — refusals used to exist only as Kafka
	// events, leaving no queryable trace and making decision_outcome a column
	// that was always 'ALLOWED'.
	if resp.ConsumptionID == "" {
		t.Error("a blocked attempt must be recorded so the refusal is auditable")
	}

	// But it must not consume the budget it was refused: a smaller follow-up on a
	// fresh correlation id has to be evaluated against a still-empty total.
	rr2 := doReq(r, http.MethodPost, "/v1/spend-checks/", map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          500.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-4",
	}, "principal-1")
	resp2 := decodeCheck(t, rr2)
	if resp2.DecisionOutcome != "ALLOWED" {
		t.Errorf("a blocked attempt must not consume budget; follow-up got %q", resp2.DecisionOutcome)
	}
	if resp2.PriorConsumption != 0 {
		t.Errorf("expected the blocked amount to be excluded from the running total, got prior=%v", resp2.PriorConsumption)
	}
}

func TestSubmitCheck_IdempotentReplay(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	if rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(10000, "MONTHLY", "USD"), "admin-1"); rr.Code != http.StatusCreated {
		t.Fatalf("policy setup failed: %s", rr.Body.String())
	}

	body := map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          2000.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-retry",
	}

	resp1 := decodeCheck(t, doReq(r, http.MethodPost, "/v1/spend-checks/", body, "principal-1"))
	resp2 := decodeCheck(t, doReq(r, http.MethodPost, "/v1/spend-checks/", body, "principal-1"))

	if resp2.ConsumptionID != resp1.ConsumptionID {
		t.Fatalf("retried check resolved to a different consumption_id (%s) than the original (%s)", resp2.ConsumptionID, resp1.ConsumptionID)
	}
	if resp2.DecisionBasis != "replayed_prior_decision" || !resp2.Replayed {
		t.Errorf("expected a flagged replay, got basis %q replayed=%v", resp2.DecisionBasis, resp2.Replayed)
	}
	// A replay used to answer with an outcome and no figures at all, so a retried
	// check displayed a decision whose basis could not be read.
	if resp2.ThresholdAmount != 10000 {
		t.Errorf("a replayed decision must still report the threshold it was judged against, got %v", resp2.ThresholdAmount)
	}

	// Consumption recorded exactly once — a re-evaluation on retry would double
	// count the same spend.
	consumptions, _ := s.ListConsumptions(context.Background(), "le-us", "")
	if len(consumptions) != 1 {
		t.Fatalf("expected exactly 1 consumption record, got %d", len(consumptions))
	}
}

// A replayed block must not re-publish: downstream consumers would read a second
// breach where the caller merely retried the first.
func TestSubmitCheck_ReplayedBlock_DoesNotRepublish(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	if rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(100, "MONTHLY", "USD"), "admin-1"); rr.Code != http.StatusCreated {
		t.Fatalf("policy setup failed: %s", rr.Body.String())
	}

	body := map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          5000.0,
		"currency_code":   "USD",
		"correlation_id":  "corr-block-retry",
	}
	_ = doReq(r, http.MethodPost, "/v1/spend-checks/", body, "principal-1")
	_ = doReq(r, http.MethodPost, "/v1/spend-checks/", body, "principal-1")

	if pub.breached != 1 || pub.blocked != 1 {
		t.Fatalf("expected exactly one breach and one block event across a retry, got breached=%d blocked=%d", pub.breached, pub.blocked)
	}
}

// ── CreatePolicy ───────────────────────────────────────────────────────────────

func TestCreatePolicy_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(10000, "MONTHLY", "USD"), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreatePolicy_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(10000, "MONTHLY", "USD"), "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

// Fail closed: an unreachable authorization-svc refuses the action rather than
// permitting it, and that refusal is distinguishable from a denial.
func TestCreatePolicy_AuthzUnavailable_FailsClosed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(10000, "MONTHLY", "USD"), "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d", rr.Code)
	}
	if code := errorCode(t, rr); code != "authz_unavailable" {
		t.Fatalf("expected authz_unavailable, got %q", code)
	}
}

func TestCreatePolicy_InvalidPeriod(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(10000, "WEEKLY", "USD"), "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestCreatePolicy_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(10000, "MONTHLY", "USD"), "principal-1")
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
	if p.TenantID != testTenant {
		t.Errorf("expected the policy to carry the request's tenant, got %q", p.TenantID)
	}
}

// ── active_flag actually means something ───────────────────────────────────────

// active_flag was written TRUE on create and never changed by any code path, so a
// second limit for the same category left BOTH rows active while evaluation used
// only the newest. A register headed "limits in force" then listed limits that
// were not in force, indistinguishably from the one that was.
func TestCreatePolicy_SupersedesThePriorLimitForTheSameCategory(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	first := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(100, "MONTHLY", "USD"), "principal-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first create failed: %s", first.Body.String())
	}
	var firstResp struct {
		Superseded int `json:"superseded"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &firstResp)
	if firstResp.Superseded != 0 {
		t.Errorf("the first limit supersedes nothing, got %d", firstResp.Superseded)
	}

	second := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(900, "MONTHLY", "USD"), "principal-1")
	var secondResp struct {
		Superseded int `json:"superseded"`
	}
	_ = json.Unmarshal(second.Body.Bytes(), &secondResp)
	if secondResp.Superseded != 1 {
		t.Errorf("the second limit should supersede exactly one, got %d", secondResp.Superseded)
	}

	// Exactly one limit is in force for the category, and it is the new one.
	rr := doReq(r, http.MethodGet, "/v1/spend-policies/?legal_entity_id=le-us", nil, "principal-1")
	var inForce []domain.SpendPolicy
	if err := json.Unmarshal(rr.Body.Bytes(), &inForce); err != nil {
		t.Fatalf("not a policy list: %s", rr.Body.String())
	}
	if len(inForce) != 1 {
		t.Fatalf("expected exactly 1 limit in force, got %d", len(inForce))
	}
	if inForce[0].ThresholdAmount != 900 {
		t.Errorf("the newest limit should be the one in force, got %v", inForce[0].ThresholdAmount)
	}
}

// The superseded row is kept, so "what was the limit in March" stays answerable.
func TestListPolicies_ActiveFalseShowsTheFullHistory(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	_ = doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(100, "MONTHLY", "USD"), "principal-1")
	_ = doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(900, "MONTHLY", "USD"), "principal-1")

	rr := doReq(r, http.MethodGet, "/v1/spend-policies/?legal_entity_id=le-us&active=false", nil, "principal-1")
	var all []domain.SpendPolicy
	if err := json.Unmarshal(rr.Body.Bytes(), &all); err != nil {
		t.Fatalf("not a policy list: %s", rr.Body.String())
	}
	if len(all) != 2 {
		t.Fatalf("expected both the current and the superseded limit, got %d", len(all))
	}
}

// Withdrawing a limit was impossible: active_flag could only ever be TRUE, so a
// category once governed stayed governed forever.
func TestDeactivatePolicy_WithdrawsTheLimit(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	created := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(100, "MONTHLY", "USD"), "principal-1")
	var policy domain.SpendPolicy
	_ = json.Unmarshal(created.Body.Bytes(), &policy)

	rr := doReq(r, http.MethodPost, "/v1/spend-policies/"+policy.SpendPolicyID+"/deactivate", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"withdrawn":true`) {
		t.Errorf("expected the response to report the withdrawal, got %s", rr.Body.String())
	}

	// Nothing is in force any more...
	list := doReq(r, http.MethodGet, "/v1/spend-policies/?legal_entity_id=le-us", nil, "principal-1")
	if got := strings.TrimSpace(list.Body.String()); got != "[]" {
		t.Fatalf("expected no limits in force after withdrawal, got %s", got)
	}

	// ...so a spend that the limit would have refused is now simply unevaluated.
	check := doReq(r, http.MethodPost, "/v1/spend-checks/", map[string]any{
		"legal_entity_id": "le-us",
		"category":        "PROCUREMENT",
		"amount":          99999.0,
		"currency_code":   "USD",
		"correlation_id":  "after-withdrawal",
	}, "principal-1")
	resp := decodeCheck(t, check)
	if resp.DecisionBasis != "no_policy_configured" {
		t.Errorf("with the limit withdrawn the category is ungoverned, got basis %q", resp.DecisionBasis)
	}
}

// Withdrawing twice is not an error: the caller's intent already holds.
func TestDeactivatePolicy_AlreadyWithdrawn_IsNotAnError(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	created := doReq(r, http.MethodPost, "/v1/spend-policies/", policyBody(100, "MONTHLY", "USD"), "principal-1")
	var policy domain.SpendPolicy
	_ = json.Unmarshal(created.Body.Bytes(), &policy)

	_ = doReq(r, http.MethodPost, "/v1/spend-policies/"+policy.SpendPolicyID+"/deactivate", nil, "principal-1")
	again := doReq(r, http.MethodPost, "/v1/spend-policies/"+policy.SpendPolicyID+"/deactivate", nil, "principal-1")

	if again.Code != http.StatusOK {
		t.Fatalf("expected 200 on a repeat withdrawal, got %d", again.Code)
	}
	if !strings.Contains(again.Body.String(), `"withdrawn":false`) {
		t.Errorf("a repeat must report that nothing changed, got %s", again.Body.String())
	}
}

func TestDeactivatePolicy_UnknownId_Is404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/spend-policies/"+uuid.NewString()+"/deactivate", nil, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// Removing a control is at least as consequential as adding one, so it takes the
// same grant.
func TestDeactivatePolicy_RequiresManageGrant(t *testing.T) {
	s := newStubStore()
	setup := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	created := doReq(setup, http.MethodPost, "/v1/spend-policies/", policyBody(100, "MONTHLY", "USD"), "principal-1")
	var policy domain.SpendPolicy
	_ = json.Unmarshal(created.Body.Bytes(), &policy)

	denied := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	rr := doReq(denied, http.MethodPost, "/v1/spend-policies/"+policy.SpendPolicyID+"/deactivate", nil, "principal-1")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.policies[policy.SpendPolicyID].ActiveFlag != true {
		t.Error("a refused withdrawal must leave the limit in force")
	}
}

func TestListPolicies_EmptyIsArrayNotNull(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodGet, "/v1/spend-policies/?legal_entity_id=le-us", nil, "principal-1")
	if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
		t.Fatalf("expected [] for an empty register, got %q", got)
	}
}
