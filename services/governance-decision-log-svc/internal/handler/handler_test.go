package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/authz"
	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/handler"
	"zoiko.io/governance-decision-log-svc/internal/policyclient"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

// stubStore implements handler.DecisionStore for unit testing.
// No DB, no network — purely in-memory.
type stubStore struct {
	created bool
	err     error
	got     *domain.GovernanceDecision

	findByIDResult    *domain.GovernanceDecision
	findByIDErr       error
	findByIDGotTenant string

	listResult    []*domain.GovernanceDecision
	listErr       error
	listGotParams store.ListParams

	createdManifest     *domain.ReplayManifest
	createManifestErr   error
	listManifestsResult []*domain.ReplayManifest
	listManifestsErr    error
}

func (s *stubStore) Insert(_ context.Context, d domain.GovernanceDecision) (bool, error) {
	s.got = &d
	return s.created, s.err
}

func (s *stubStore) FindByID(_ context.Context, tenantID, decisionID string) (*domain.GovernanceDecision, error) {
	s.findByIDGotTenant = tenantID
	return s.findByIDResult, s.findByIDErr
}

func (s *stubStore) List(_ context.Context, params store.ListParams) ([]*domain.GovernanceDecision, error) {
	s.listGotParams = params
	return s.listResult, s.listErr
}

// ── replay manifests ─────────────────────────────────────────────────────────

func (s *stubStore) CreateReplayManifest(_ context.Context, m *domain.ReplayManifest) error {
	s.createdManifest = m
	return s.createManifestErr
}

func (s *stubStore) ListReplayManifestsByDecision(_ context.Context, _ string) ([]*domain.ReplayManifest, error) {
	return s.listManifestsResult, s.listManifestsErr
}

// stubPolicyClient implements policyclient.Client for unit testing.
type stubPolicyClient struct {
	version *policyclient.PolicyVersion
	err     error
}

func (c *stubPolicyClient) GetPolicyVersion(_ context.Context, _ string) (*policyclient.PolicyVersion, error) {
	return c.version, c.err
}

var _ policyclient.Client = (*stubPolicyClient)(nil)

// stubPublisher implements handler.EventPublisher for unit testing.
// No Kafka, no network — purely in-memory, records what it was asked to publish.
type stubPublisher struct {
	err         error
	publishes   int
	gotDecision *domain.GovernanceDecision
}

func (p *stubPublisher) PublishDecisionRecorded(_ context.Context, d domain.GovernanceDecision) error {
	p.publishes++
	p.gotDecision = &d
	return p.err
}

func newTestRouter(store handler.DecisionStore, pub handler.EventPublisher) http.Handler {
	return newTestRouterWithPolicyClient(store, pub, &stubPolicyClient{})
}

// newTestRouterWithPolicyClient is the same wiring with an explicit policy
// client — used by the ReplayDecision tests, which need to control what
// policy-svc "returns" without a real service.
func newTestRouterWithPolicyClient(store handler.DecisionStore, pub handler.EventPublisher, pc policyclient.Client) http.Handler {
	r := chi.NewRouter()
	h := handler.New(store, pub, testAuthz(), pc, testAuthzScopeID, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func validBody() string {
	return `{
		"decision_id": "dec-001",
		"tenant_id": "tenant-1",
		"legal_entity_id": "entity-1",
		"actor_id": "actor-1",
		"action_type": "PAYROLL_RELEASE",
		"outcome": "DENIED",
		"rule_basis": "policy-v3-sod",
		"correlation_id": "corr-001"
	}`
}

// TestCreateDecision_201_FirstInsert verifies the happy path: a brand new
// decision_id returns 201 with the stored decision echoed back.
func TestCreateDecision_201_FirstInsert(t *testing.T) {
	store := &stubStore{created: true}
	h := newTestRouter(store, &stubPublisher{})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
	req.Header.Set("X-Correlation-ID", "corr-req-001")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got domain.GovernanceDecision
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if got.DecisionID != "dec-001" {
		t.Errorf("decision_id mismatch: got %q", got.DecisionID)
	}
	if got.DecidedAt.IsZero() {
		t.Errorf("expected decided_at to default to server time when omitted")
	}
	if rr.Header().Get("X-Correlation-ID") != "corr-req-001" {
		t.Errorf("correlation ID not echoed: got %q", rr.Header().Get("X-Correlation-ID"))
	}
}

// TestCreateDecision_201_PublishesDecisionRecorded verifies a first-time
// insert publishes governance.decision.recorded exactly once, with the
// stored decision as the payload.
func TestCreateDecision_201_PublishesDecisionRecorded(t *testing.T) {
	pub := &stubPublisher{}
	h := newTestRouter(&stubStore{created: true}, pub)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if pub.publishes != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", pub.publishes)
	}
	if pub.gotDecision == nil || pub.gotDecision.DecisionID != "dec-001" {
		t.Errorf("expected published decision dec-001, got %+v", pub.gotDecision)
	}
}

// TestCreateDecision_200_IdempotentReplay verifies that a repeat POST for an
// already-stored decision_id returns 200, not 201 — proving the handler
// surfaces the store's idempotency signal rather than always reporting
// "created".
func TestCreateDecision_200_IdempotentReplay(t *testing.T) {
	store := &stubStore{created: false}
	h := newTestRouter(store, &stubPublisher{})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_200_IdempotentReplay_DoesNotRePublish verifies that a
// replayed decision_id (created=false) does not re-emit
// governance.decision.recorded — only the first insert is a new fact.
func TestCreateDecision_200_IdempotentReplay_DoesNotRePublish(t *testing.T) {
	pub := &stubPublisher{}
	h := newTestRouter(&stubStore{created: false}, pub)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if pub.publishes != 0 {
		t.Fatalf("expected 0 publishes on idempotent replay, got %d", pub.publishes)
	}
}

// TestCreateDecision_201_PublishFailureDoesNotFailRequest verifies that a
// publish failure is logged but does not change the HTTP response — the
// write already succeeded and event delivery is a stubbed, non-blocking
// concern.
func TestCreateDecision_201_PublishFailureDoesNotFailRequest(t *testing.T) {
	pub := &stubPublisher{err: errors.New("kafka unreachable")}
	h := newTestRouter(&stubStore{created: true}, pub)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 despite publish failure, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_400_MissingField verifies that omitting any required
// field is rejected with 400 and names the missing field.
func TestCreateDecision_400_MissingField(t *testing.T) {
	body := `{"tenant_id": "tenant-1"}` // missing everything else
	h := newTestRouter(&stubStore{created: true}, &stubPublisher{})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body)))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if got["error"] != "missing_field" {
		t.Errorf("expected error=missing_field, got %q", got["error"])
	}
}

// TestCreateDecision_400_InvalidJSON verifies malformed JSON is rejected
// with 400, not a 500 or panic.
func TestCreateDecision_400_InvalidJSON(t *testing.T) {
	h := newTestRouter(&stubStore{created: true}, &stubPublisher{})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{not json`)))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_503_StoreUnavailable verifies that a store error
// returns 503 — not a silently swallowed failure.
func TestCreateDecision_503_StoreUnavailable(t *testing.T) {
	h := newTestRouter(&stubStore{err: domain.ErrStoreUnavailable}, &stubPublisher{})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if got["error"] != "store_unavailable" {
		t.Errorf("expected error=store_unavailable, got %q", got["error"])
	}
}

// TestGetDecision_200_Found verifies a known decision_id returns 200 with
// the stored decision.
func TestGetDecision_200_Found(t *testing.T) {
	want := &domain.GovernanceDecision{DecisionID: "dec-001", TenantID: "tenant-1"}
	s := &stubStore{findByIDResult: want}
	h := newTestRouter(s, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions/dec-001", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got domain.GovernanceDecision
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if got.DecisionID != "dec-001" {
		t.Errorf("decision_id mismatch: got %q", got.DecisionID)
	}
	if s.findByIDGotTenant != "tenant-1" {
		t.Errorf("expected tenant-1 forwarded to store, got %q", s.findByIDGotTenant)
	}
}

// TestGetDecision_400_MissingTenantID verifies that GetDecision requires
// X-Tenant-Id — closing the gap where decision-support-svc's
// GovernanceLogClient sends this header but the handler used to ignore it
// entirely, returning cross-tenant data unscoped.
func TestGetDecision_400_MissingTenantID(t *testing.T) {
	h := newTestRouter(&stubStore{}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions/dec-001", nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestGetDecision_404_NotFound verifies an unknown decision_id returns 404,
// distinct from a store failure (503).
func TestGetDecision_404_NotFound(t *testing.T) {
	h := newTestRouter(&stubStore{findByIDErr: domain.ErrDecisionNotFound}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions/does-not-exist", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestGetDecision_503_StoreUnavailable verifies a non-not-found store error
// returns 503, never conflated with a legitimate 404.
func TestGetDecision_503_StoreUnavailable(t *testing.T) {
	h := newTestRouter(&stubStore{findByIDErr: domain.ErrStoreUnavailable}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions/dec-001", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestListDecisions_200_Empty verifies an empty result set serialises as an
// empty JSON array, never null.
func TestListDecisions_200_Empty(t *testing.T) {
	h := newTestRouter(&stubStore{listResult: nil}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Errorf("expected empty JSON array, got %q", rr.Body.String())
	}
}

// TestListDecisions_FiltersComposeIntoListParams verifies every query
// parameter is parsed and forwarded to the store, and that they compose
// (all filters can be set simultaneously).
func TestListDecisions_FiltersComposeIntoListParams(t *testing.T) {
	s := &stubStore{listResult: []*domain.GovernanceDecision{{DecisionID: "dec-001"}}}
	h := newTestRouter(s, &stubPublisher{})

	q := url.Values{
		"actor":      {"actor-1"},
		"entity":     {"entity-1"},
		"action":     {"PAYROLL_RELEASE"},
		"rule_basis": {"policy-v3-sod"},
		"from":       {"2024-01-01T00:00:00Z"},
		"to":         {"2024-12-31T23:59:59Z"},
		"limit":      {"10"},
		"offset":     {"5"},
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions?"+q.Encode(), nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	p := s.listGotParams
	if p.TenantID != "tenant-1" {
		t.Errorf("expected tenant_id forwarded to store, got %q", p.TenantID)
	}
	if p.ActorID != "actor-1" || p.LegalEntityID != "entity-1" || p.ActionType != "PAYROLL_RELEASE" || p.RuleBasis != "policy-v3-sod" {
		t.Errorf("filters not forwarded correctly: %+v", p)
	}
	if p.From.IsZero() || p.To.IsZero() {
		t.Errorf("expected from/to to be parsed, got %+v", p)
	}
	if p.Limit != 10 || p.Offset != 5 {
		t.Errorf("expected limit=10 offset=5, got limit=%d offset=%d", p.Limit, p.Offset)
	}
}

// TestListDecisions_400_MissingTenantID verifies ListDecisions requires
// X-Tenant-Id, same as GetDecision.
func TestListDecisions_400_MissingTenantID(t *testing.T) {
	h := newTestRouter(&stubStore{}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions", nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestListDecisions_400_InvalidFrom verifies a malformed from timestamp is
// rejected with 400 rather than silently ignored or causing a 500.
func TestListDecisions_400_InvalidFrom(t *testing.T) {
	h := newTestRouter(&stubStore{}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions?from=not-a-timestamp", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestListDecisions_400_InvalidTo verifies a malformed to timestamp is
// rejected with 400.
func TestListDecisions_400_InvalidTo(t *testing.T) {
	h := newTestRouter(&stubStore{}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions?to=not-a-timestamp", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestListDecisions_503_StoreUnavailable verifies a store failure returns
// 503, not a silently empty list.
func TestListDecisions_503_StoreUnavailable(t *testing.T) {
	h := newTestRouter(&stubStore{listErr: domain.ErrStoreUnavailable}, &stubPublisher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// ── authorization test scaffolding ───────────────────────────────────────────

// testAuthzScopeID stands in for config.AuthZPlatformScopeID.
const testAuthzScopeID = "00000000-0000-0000-0000-0000000000f3"

// testPrincipal is what the gateway ForwardAuth middleware sets in
// X-Principal-Id after verifying the caller identity envelope.
const testPrincipal = "principal-test-admin"

// stubAuthz records what it was asked and answers with err.
type stubAuthz struct {
	err error

	calls      int
	principal  string
	scope      string
	actionType string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, principalID, legalEntityID, actionType string) error {
	a.calls++
	a.principal, a.scope, a.actionType = principalID, legalEntityID, actionType
	return a.err
}

// testAuthz is the permit-all default used by every pre-existing test.
func testAuthz() *stubAuthz { return &stubAuthz{} }

// authed stamps the gateway-verified principal header onto a request.
// Every mutating route now requires it.
func authed(req *http.Request) *http.Request {
	req.Header.Set("X-Principal-Id", testPrincipal)
	return req
}

// ── authorization contract ───────────────────────────────────────────────────

// gatedRoutes is every route that mutates state, so a route added later
// cannot quietly skip the gate.
var gatedRoutes = []struct {
	name string
	path string
	body string
}{
	{name: "record decision", path: "/v1/decisions", body: `{"decision_id":"d-1","tenant_id":"t-1","legal_entity_id":"le-1","actor_id":"a-1","action_type":"APPROVE","outcome":"GRANTED","rule_basis":"policy:APPROVAL_5K","correlation_id":"corr-1"}`},
}

// TestGatedRoutes_401_WithoutPrincipal — this service shipped with no gate of
// any kind, so anything able to reach the port could write to it.
func TestGatedRoutes_401_WithoutPrincipal(t *testing.T) {
	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := &stubStore{}
			pub := &stubPublisher{}
			az := &stubAuthz{}
			r := chi.NewRouter()
			handler.RegisterRoutes(r, handler.New(store, pub, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))

			// Deliberately NOT wrapped in authed().
			req := httptest.NewRequest(http.MethodPost, route.path, bytes.NewBufferString(route.body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 without a principal, got %d: %s", w.Code, w.Body.String())
			}
			if az.calls != 0 {
				t.Error("authorization was consulted before the caller was even identified")
			}
		})
	}
}

// TestGatedRoutes_403_Denied — a denial must stop the write.
func TestGatedRoutes_403_Denied(t *testing.T) {
	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := &stubStore{}
			pub := &stubPublisher{}
			az := &stubAuthz{err: authz.ErrDenied}
			r := chi.NewRouter()
			handler.RegisterRoutes(r, handler.New(store, pub, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))

			req := authed(httptest.NewRequest(http.MethodPost, route.path, bytes.NewBufferString(route.body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
			}
			if az.principal != testPrincipal {
				t.Errorf("authz saw principal %q, want %q", az.principal, testPrincipal)
			}
			if az.scope == "" {
				t.Error("an empty legal_entity_id would be rejected by authorization-svc with 400")
			}
		})
	}
}

// TestGatedRoutes_503_AuthzUnavailableFailsClosed — an unreachable
// authorization service must block the mutation, not wave it through.
func TestGatedRoutes_503_AuthzUnavailableFailsClosed(t *testing.T) {
	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := &stubStore{}
			pub := &stubPublisher{}
			az := &stubAuthz{err: authz.ErrUnavailable}
			r := chi.NewRouter()
			handler.RegisterRoutes(r, handler.New(store, pub, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))

			req := authed(httptest.NewRequest(http.MethodPost, route.path, bytes.NewBufferString(route.body)))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// ── ReplayDecision ───────────────────────────────────────────────────────────

func thresholdPayload(threshold float64) []byte {
	b, _ := json.Marshal(map[string]float64{"threshold_amount": threshold})
	return b
}

func amountContext(amount float64) []byte {
	b, _ := json.Marshal(map[string]float64{"amount": amount})
	return b
}

// TestReplayDecision_ReproducesOriginalOutcome is the core doc7 reproducibility
// proof: replaying a decision against the EXACT policy version it used, with
// the SAME facts, must reproduce the SAME outcome.
func TestReplayDecision_ReproducesOriginalOutcome(t *testing.T) {
	store := &stubStore{
		findByIDResult: &domain.GovernanceDecision{
			DecisionID:        "dec-1",
			ActionType:        "APPROVAL_THRESHOLD",
			Outcome:           "ESCALATED",
			RuleBasis:         "APPROVAL_5K:pv-1",
			EvaluationContext: amountContext(7500),
		},
	}
	pc := &stubPolicyClient{version: &policyclient.PolicyVersion{PolicyVersionID: "pv-1", RulePayload: thresholdPayload(5000)}}
	h := newTestRouterWithPolicyClient(store, &stubPublisher{}, pc)

	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-1/replay", bytes.NewBufferString(`{"replayed_by_principal_id":"auditor-1"}`))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var manifest domain.ReplayManifest
	if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if manifest.ReplayedOutcome != "ESCALATED" || !manifest.OutcomesMatch {
		t.Fatalf("expected replay to reproduce ESCALATED and match, got %+v", manifest)
	}
	if store.createdManifest == nil {
		t.Fatal("expected a replay manifest to be persisted")
	}
}

// TestReplayDecision_DetectsDrift proves the manifest can also record a
// genuine MISMATCH — e.g. if the policy version's rule_payload were somehow
// different from what was actually used at decision time, or a data
// integrity issue elsewhere. The manifest must faithfully record this, not
// silently coerce it into a match.
func TestReplayDecision_DetectsDrift(t *testing.T) {
	store := &stubStore{
		findByIDResult: &domain.GovernanceDecision{
			DecisionID:        "dec-2",
			ActionType:        "APPROVAL_THRESHOLD",
			Outcome:           "GRANTED", // originally recorded as within threshold
			RuleBasis:         "APPROVAL_5K:pv-2",
			EvaluationContext: amountContext(7500),
		},
	}
	// If the fetched version's threshold is different from what was
	// actually in force at decision time, replay will disagree.
	pc := &stubPolicyClient{version: &policyclient.PolicyVersion{PolicyVersionID: "pv-2", RulePayload: thresholdPayload(5000)}}
	h := newTestRouterWithPolicyClient(store, &stubPublisher{}, pc)

	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-2/replay", bytes.NewBufferString(`{"replayed_by_principal_id":"auditor-1"}`))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (a mismatch is still a recorded fact, not an error), got %d — %s", w.Code, w.Body.String())
	}
	var manifest domain.ReplayManifest
	_ = json.Unmarshal(w.Body.Bytes(), &manifest)
	if manifest.OutcomesMatch {
		t.Fatalf("expected outcomes_match=false for a genuine drift, got %+v", manifest)
	}
	if manifest.OriginalOutcome != "GRANTED" || manifest.ReplayedOutcome != "ESCALATED" {
		t.Fatalf("expected original=GRANTED replayed=ESCALATED, got %+v", manifest)
	}
}

func TestReplayDecision_UnsupportedActionType501(t *testing.T) {
	store := &stubStore{
		findByIDResult: &domain.GovernanceDecision{DecisionID: "dec-3", ActionType: "SOD_RULE", Outcome: "GRANTED", RuleBasis: "x:pv-3"},
	}
	h := newTestRouterWithPolicyClient(store, &stubPublisher{}, &stubPolicyClient{})

	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-3/replay", bytes.NewBufferString(`{"replayed_by_principal_id":"auditor-1"}`))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d — %s", w.Code, w.Body.String())
	}
}

func TestReplayDecision_PolicyVersionNotFound404(t *testing.T) {
	store := &stubStore{
		findByIDResult: &domain.GovernanceDecision{DecisionID: "dec-4", ActionType: "APPROVAL_THRESHOLD", Outcome: "GRANTED", RuleBasis: "x:pv-missing"},
	}
	pc := &stubPolicyClient{err: policyclient.ErrPolicyVersionNotFound}
	h := newTestRouterWithPolicyClient(store, &stubPublisher{}, pc)

	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-4/replay", bytes.NewBufferString(`{"replayed_by_principal_id":"auditor-1"}`))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d — %s", w.Code, w.Body.String())
	}
}

func TestReplayDecision_MissingReplayedByPrincipal400(t *testing.T) {
	h := newTestRouter(&stubStore{}, &stubPublisher{})

	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-5/replay", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — %s", w.Code, w.Body.String())
	}
}
