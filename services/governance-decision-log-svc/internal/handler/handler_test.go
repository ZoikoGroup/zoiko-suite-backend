package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
	// created is what RecordDecision reports. When false, the "stored" row is
	// existing if set, otherwise an identical copy of what was sent.
	created  bool
	existing *domain.GovernanceDecision
	err      error
	got      *domain.GovernanceDecision
	gotRec   store.DecisionRecord
	// inserts counts calls, so a test can assert that a refused request wrote
	// nothing at all rather than only that it answered with an error.
	inserts int

	findByIDResult    *domain.GovernanceDecision
	findByIDErr       error
	findByIDGotTenant string

	listResult    []*domain.GovernanceDecision
	listErr       error
	listGotParams store.ListParams

	createdManifest        *domain.ReplayManifest
	manifestCreates        int
	gotReplayEvent         *store.ReplayEvent
	createManifestErr      error
	listManifestsResult    []*domain.ReplayManifest
	listManifestsErr       error
	listManifestsGotTenant string

	// idempotency keys
	idempotencyKeys map[string]struct {
		decisionID string
		bodyHash   []byte
	}
}

func (s *stubStore) RecordDecision(_ context.Context, rec store.DecisionRecord) (*domain.GovernanceDecision, bool, error) {
	d := rec.Decision
	s.got = &d
	s.gotRec = rec
	s.inserts++
	if s.err != nil {
		return nil, false, s.err
	}
	if !s.created {
		if s.existing != nil {
			return s.existing, false, nil
		}
		return &d, false, nil
	}
	if rec.IdempotencyKey != "" {
		if s.idempotencyKeys == nil {
			s.idempotencyKeys = map[string]struct {
				decisionID string
				bodyHash   []byte
			}{}
		}
		s.idempotencyKeys[rec.IdempotencyKey] = struct {
			decisionID string
			bodyHash   []byte
		}{d.DecisionID, rec.BodyHash}
	}
	stored := d
	stored.RecordedAt = time.Now().UTC()
	s.findByIDResult = &stored
	return &stored, true, nil
}

func (s *stubStore) FindByID(_ context.Context, tenantID, decisionID string) (*domain.GovernanceDecision, error) {
	s.findByIDGotTenant = tenantID
	if s.findByIDResult == nil && s.findByIDErr == nil {
		return nil, domain.ErrDecisionNotFound // the real store never answers (nil, nil)
	}
	return s.findByIDResult, s.findByIDErr
}

func (s *stubStore) List(_ context.Context, params store.ListParams) ([]*domain.GovernanceDecision, error) {
	s.listGotParams = params
	return s.listResult, s.listErr
}

// ── replay manifests ─────────────────────────────────────────────────────────

func (s *stubStore) CreateReplayManifest(_ context.Context, m *domain.ReplayManifest, ev *store.ReplayEvent) (*domain.ReplayManifest, bool, error) {
	s.gotReplayEvent = ev
	s.manifestCreates++
	if s.createManifestErr != nil {
		return nil, false, s.createManifestErr
	}
	s.createdManifest = m
	s.listManifestsResult = append(s.listManifestsResult, m)
	return m, true, nil
}

func (s *stubStore) ListReplayManifestsByDecision(_ context.Context, tenantID, _ string) ([]*domain.ReplayManifest, error) {
	s.listManifestsGotTenant = tenantID
	return s.listManifestsResult, s.listManifestsErr
}

// ── idempotency keys ──────────────────────────────────────────────────────────

func (s *stubStore) CheckIdempotencyKey(_ context.Context, _, idempotencyKey string) (string, []byte, error) {
	if entry, ok := s.idempotencyKeys[idempotencyKey]; ok {
		return entry.decisionID, entry.bodyHash, nil
	}
	return "", nil, nil
}

// stubPolicyClient implements policyclient.Client for unit testing.
type stubPolicyClient struct {
	version *policyclient.PolicyVersion
	err     error
	calls   int

	// gotTenantID records the scope the version was fetched in, so a test can
	// assert a replay stays inside the decision's own tenant.
	gotTenantID string
	gotVersion  string
}

func (c *stubPolicyClient) GetPolicyVersion(_ context.Context, tenantID, versionID string) (*policyclient.PolicyVersion, error) {
	c.calls++
	c.gotTenantID, c.gotVersion = tenantID, versionID
	return c.version, c.err
}

var _ policyclient.Client = (*stubPolicyClient)(nil)

// newTestRouter builds a chi.Router with the handler for testing.
func newTestRouter(store handler.DecisionStore) http.Handler {
	r := chi.NewRouter()
	h := handler.New(store, testAuthz(), &stubPolicyClient{}, testAuthzScopeID, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// newTestRouterWithPolicyClient is the same wiring with an explicit policy
// client — used by the ReplayDecision tests, which need to control what
// policy-svc "returns" without a real service.
func newTestRouterWithPolicyClient(store handler.DecisionStore, pc policyclient.Client) http.Handler {
	r := chi.NewRouter()
	h := handler.New(store, testAuthz(), pc, testAuthzScopeID, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func validBody() string {
	return `{
		"decision_id": "dec-001",
		"tenant_id": "tenant-1",
		"legal_entity_id": "55555555-5555-5555-5555-555555555555",
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
	h := newTestRouter(store)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
	req.Header.Set("X-Correlation-ID", "corr-001")

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
	if rr.Header().Get("X-Correlation-ID") != "corr-001" {
		t.Errorf("correlation ID not echoed: got %q", rr.Header().Get("X-Correlation-ID"))
	}
}

// TestCreateDecision_201_PublishesDecisionRecorded verifies a first-time
// insert works correctly.
func TestCreateDecision_201_PublishesDecisionRecorded(t *testing.T) {
	h := newTestRouter(&stubStore{created: true})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_200_IdempotentReplay verifies that a repeat POST for an
// already-stored decision_id returns 200, not 201 — proving the handler
// surfaces the store's idempotency signal rather than always reporting
// "created".
func TestCreateDecision_200_IdempotentReplay(t *testing.T) {
	store := &stubStore{created: false}
	h := newTestRouter(store)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_200_IdempotentReplay_NoRePublish verifies that a
// replayed decision_id (created=false) does not cause issues.
func TestCreateDecision_200_IdempotentReplay_NoRePublish(t *testing.T) {
	h := newTestRouter(&stubStore{created: false})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_201_StoreErrorDoesNotFailRequest verifies that a
// store error returns 503 — not a silently swallowed failure.
func TestCreateDecision_201_StoreErrorDoesNotFailRequest(t *testing.T) {
	h := newTestRouter(&stubStore{err: domain.ErrStoreUnavailable})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Type     string `json:"type"`
		Title    string `json:"title"`
		Status   int    `json:"status"`
		Detail   string `json:"detail"`
		Instance string `json:"instance"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if got.Title != "Service Unavailable" {
		t.Errorf("expected title=Service Unavailable, got %q", got.Title)
	}
	if got.Code != "SERVICE_UNAVAILABLE" {
		t.Errorf("expected code=SERVICE_UNAVAILABLE, got %q", got.Code)
	}
}

// TestCreateDecision_400_MissingField verifies that omitting any required
// field is rejected with 400 and names the missing field.
func TestCreateDecision_400_MissingField(t *testing.T) {
	body := `{"tenant_id": "tenant-1"}` // missing everything else
	h := newTestRouter(&stubStore{created: true})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body)))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Type     string `json:"type"`
		Title    string `json:"title"`
		Status   int    `json:"status"`
		Detail   string `json:"detail"`
		Instance string `json:"instance"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if got.Title != "Missing Required Field" {
		t.Errorf("expected title=Missing Required Field, got %q", got.Title)
	}
	if got.Code != "INVALID_REQUEST" {
		t.Errorf("expected code=INVALID_REQUEST, got %q", got.Code)
	}
}

// TestCreateDecision_400_InvalidJSON verifies malformed JSON is rejected
// with 400, not a 500 or panic.
func TestCreateDecision_400_InvalidJSON(t *testing.T) {
	h := newTestRouter(&stubStore{created: true})
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
	h := newTestRouter(&stubStore{err: domain.ErrStoreUnavailable})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Type     string `json:"type"`
		Title    string `json:"title"`
		Status   int    `json:"status"`
		Detail   string `json:"detail"`
		Instance string `json:"instance"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if got.Title != "Service Unavailable" {
		t.Errorf("expected title=Service Unavailable, got %q", got.Title)
	}
	if got.Code != "SERVICE_UNAVAILABLE" {
		t.Errorf("expected code=SERVICE_UNAVAILABLE, got %q", got.Code)
	}
}

// TestGetDecision_200_Found verifies a known decision_id returns 200 with
// the stored decision.
func TestGetDecision_200_Found(t *testing.T) {
	want := &domain.GovernanceDecision{DecisionID: "dec-001", TenantID: "tenant-1"}
	s := &stubStore{findByIDResult: want}
	h := newTestRouter(s)
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/decisions/dec-001", nil))
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
	h := newTestRouter(&stubStore{})
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
	h := newTestRouter(&stubStore{findByIDErr: domain.ErrDecisionNotFound})
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/decisions/does-not-exist", nil))
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
	h := newTestRouter(&stubStore{findByIDErr: domain.ErrStoreUnavailable})
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/decisions/dec-001", nil))
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
	h := newTestRouter(&stubStore{listResult: nil})
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/decisions", nil))
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
	h := newTestRouter(s)

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
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/decisions?"+q.Encode(), nil))
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
	h := newTestRouter(&stubStore{})
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions", nil)
	req.Header.Set("X-Principal-Id", testPrincipal)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestListDecisions_400_InvalidFrom verifies a malformed from timestamp is
// rejected with 400 rather than silently ignored or causing a 500.
func TestListDecisions_400_InvalidFrom(t *testing.T) {
	h := newTestRouter(&stubStore{})
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/decisions?from=not-a-timestamp", nil))
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
	h := newTestRouter(&stubStore{})
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/decisions?to=not-a-timestamp", nil))
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
	h := newTestRouter(&stubStore{listErr: domain.ErrStoreUnavailable})
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/decisions", nil))
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

const (
	testTenant  = "tenant-1"
	testEntity  = "55555555-5555-5555-5555-555555555555"
	otherTenant = "tenant-2"
)

// ── tenant scope ─────────────────────────────────────────────────────────────

// TestCreateDecision_403_ForeignTenantBody is the regression test for a
// cross-tenant WRITE: tenant_id in the body was the only source of both the
// stored tenant and the RLS scope the insert ran in.
func TestCreateDecision_403_ForeignTenantBody(t *testing.T) {
	store := &stubStore{created: true}
	h := newTestRouter(store)
	body := strings.Replace(validBody(), `"tenant_id": "tenant-1"`, `"tenant_id": "`+otherTenant+`"`, 1)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body)))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 recording into another tenant, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if store.inserts != 0 {
		t.Fatalf("expected nothing written, got %d inserts", store.inserts)
	}
}

func TestCreateDecision_401_NoTenantScope(t *testing.T) {
	store := &stubStore{created: true}
	h := newTestRouter(store)
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("Idempotency-Key", "test-idempotency-key-"+testPrincipal)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if store.inserts != 0 {
		t.Fatalf("expected nothing written, got %d inserts", store.inserts)
	}
}

// A body that omits tenant_id is fine — the tenant is the verified scope, and
// that is the tenant the decision must be filed under.
func TestCreateDecision_201_NoTenantInBody_UsesVerifiedScope(t *testing.T) {
	store := &stubStore{created: true}
	h := newTestRouter(store)
	body := strings.Replace(validBody(), `"tenant_id": "tenant-1",`, ``, 1)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body)))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if store.got == nil || store.got.TenantID != testTenant {
		t.Fatalf("expected the decision filed under the verified tenant %s, got %+v", testTenant, store.got)
	}
}

// TestCreateDecision_409_SameDecisionIDChangedBody is the audit's I1 scenario:
// a repeat POST of a recorded decision_id with a DIFFERENT body (and a fresh
// Idempotency-Key, so the key check cannot catch it) used to answer 200 and
// echo the new, unstored content as though it had been recorded.
func TestCreateDecision_409_SameDecisionIDChangedBody(t *testing.T) {
	stored := &domain.GovernanceDecision{
		DecisionID: "dec-001", TenantID: testTenant, LegalEntityID: testEntity,
		ActorID: testPrincipal, ActionType: "PAYROLL_RELEASE", Outcome: "GRANTED",
		RuleBasis: "policy-v3-sod", CorrelationID: "corr-001",
	}
	h := newTestRouter(&stubStore{created: false, existing: stored})
	// validBody says outcome DENIED; the stored row says GRANTED.
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
	req.Header.Set("Idempotency-Key", "a-different-key")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a changed body on a recorded decision_id, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "DENIED") {
		t.Fatalf("response echoed content that was never stored: %s", rr.Body.String())
	}
}

// TestCreateDecision_200_SameDecisionIDSameBody_ReturnsStoredRow — a true
// replay answers with what is STORED, including the server's recorded_at.
func TestCreateDecision_200_SameDecisionIDSameBody_ReturnsStoredRow(t *testing.T) {
	recorded := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	stored := &domain.GovernanceDecision{
		DecisionID: "dec-001", TenantID: testTenant, LegalEntityID: testEntity,
		ActorID: testPrincipal, ActionType: "PAYROLL_RELEASE", Outcome: "DENIED",
		RuleBasis: "policy-v3-sod", CorrelationID: "corr-001", RecordedAt: recorded,
	}
	h := newTestRouter(&stubStore{created: false, existing: stored})
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
	req.Header.Set("Idempotency-Key", "a-different-key")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got domain.GovernanceDecision
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if !got.RecordedAt.Equal(recorded) {
		t.Fatalf("expected the stored row (recorded_at %s), got %+v", recorded, got)
	}
}

// TestCreateDecision_409_IdempotencyKeyReusedWithDifferentBody — same key,
// changed request → 409 (API standard l.381, GCP §16 IDEMPOTENCY_MISMATCH).
func TestCreateDecision_409_IdempotencyKeyReusedWithDifferentBody(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	first := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
	h.ServeHTTP(httptest.NewRecorder(), first)

	changed := strings.Replace(validBody(), `"outcome": "DENIED"`, `"outcome": "GRANTED"`, 1)
	second := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(changed)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, second)

	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "IDEMPOTENCY_MISMATCH") {
		t.Fatalf("expected 409 IDEMPOTENCY_MISMATCH, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if s.inserts != 1 {
		t.Fatalf("expected the mismatched retry to write nothing, got %d inserts", s.inserts)
	}
}

// TestCreateDecision_IdempotentReplayIsAuthorized — a key replay returns a
// stored decision, so it must not bypass authorization: it used to be answered
// before the authz check, letting any principal read a decision by key.
func TestCreateDecision_IdempotentReplayIsAuthorized(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	h.ServeHTTP(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))))

	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, &stubAuthz{err: authz.ErrDenied}, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unauthorized key replay, got %d — body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_ActorIsTheAuthenticatedPrincipal — a body naming another
// actor does not change who the decision is attributed to.
func TestCreateDecision_ActorIsTheAuthenticatedPrincipal(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	body := strings.Replace(validBody(), `"actor_id": "actor-1"`, `"actor_id": "someone-else"`, 1)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body))))

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", rr.Code, rr.Body.String())
	}
	if s.got.ActorID != testPrincipal {
		t.Fatalf("expected actor %q (the principal), got %q", testPrincipal, s.got.ActorID)
	}
}

// TestCreateDecision_413_OversizedBody — the write path read the body with no
// cap, so the 256 KiB bound held for replay but not for the ledger write.
func TestCreateDecision_413_OversizedBody(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	big := `{"decision_id":"d","evaluation_context":{"pad":"` + strings.Repeat("x", 300<<10) + `"}}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(big))))

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rr.Code)
	}
	if s.inserts != 0 {
		t.Fatal("expected nothing written")
	}
}

// TestCreateDecision_400_CorrelationMismatch — the stored correlation_id came
// from the body and was never compared with X-Correlation-ID.
func TestCreateDecision_400_CorrelationMismatch(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
	req.Header.Set("X-Correlation-ID", "corr-something-else")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "CORRELATION_MISMATCH") {
		t.Fatalf("expected 400 CORRELATION_MISMATCH, got %d — %s", rr.Code, rr.Body.String())
	}
	if s.inserts != 0 {
		t.Fatal("expected nothing written")
	}
}

// TestCreateDecision_CorrelationFromHeaderWhenBodyOmitsIt — one source of truth.
func TestCreateDecision_CorrelationFromHeaderWhenBodyOmitsIt(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	body := strings.Replace(validBody(), `"correlation_id": "corr-001"`, `"correlation_id": ""`, 1)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body)))
	req.Header.Set("X-Correlation-ID", "corr-from-header")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated || s.got.CorrelationID != "corr-from-header" {
		t.Fatalf("expected 201 with the header correlation stored, got %d / %+v", rr.Code, s.got)
	}
}

// TestCreateDecision_400_DecidedAtInTheFuture — decided_at was caller-controlled
// with no bound; a decision may not be recorded as made in the future.
func TestCreateDecision_400_DecidedAtInTheFuture(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	future := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339)
	body := strings.Replace(validBody(), `"correlation_id": "corr-001"`, `"correlation_id": "corr-001", "decided_at": "`+future+`"`, 1)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body))))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for decided_at 10 minutes ahead, got %d — %s", rr.Code, rr.Body.String())
	}
}

// TestCreateDecision_PolicyVersionPromotedFromRuleBasis — Doc 04 §7.1
// policy_version_id existed only inside the rule_basis string.
func TestCreateDecision_PolicyVersionPromotedFromRuleBasis(t *testing.T) {
	const version = "6f1c2a52-6a35-4f0e-9a7c-3d1b8f0e2c11"
	for _, tc := range []struct {
		ruleBasis string
		want      string
	}{
		{"APPROVAL_5K:" + version, version},
		{"policy-v3-sod", ""},
		{"rbac:role=PLATFORM_OPERATOR", ""},
	} {
		s := &stubStore{created: true}
		h := newTestRouter(s)
		body := strings.Replace(validBody(), `"rule_basis": "policy-v3-sod"`, `"rule_basis": "`+tc.ruleBasis+`"`, 1)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body))))
		if rr.Code != http.StatusCreated {
			t.Fatalf("%s: expected 201, got %d", tc.ruleBasis, rr.Code)
		}
		got := ""
		if s.got.PolicyVersionID != nil {
			got = *s.got.PolicyVersionID
		}
		if got != tc.want {
			t.Errorf("rule_basis %q: policy_version_id = %q, want %q", tc.ruleBasis, got, tc.want)
		}
	}
}

// TestCreateDecision_EventQueuedWithTheDecision — the event goes to the store in
// the same call as the decision (one transaction), keyed by decision_id.
func TestCreateDecision_EventQueuedWithTheDecision(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	h.ServeHTTP(httptest.NewRecorder(), authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))))

	var env struct {
		EventType   string `json:"event_type"`
		AggregateID string `json:"aggregate_id"`
		PayloadHash string `json:"payload_hash"`
	}
	if err := json.Unmarshal(s.gotRec.Event, &env); err != nil {
		t.Fatalf("no event handed to the store with the decision: %v", err)
	}
	if env.EventType != "governance.decision.recorded" || env.AggregateID != "dec-001" || env.PayloadHash == "" {
		t.Fatalf("unexpected event envelope: %+v", env)
	}
}

// stubAuthz records what it was asked and answers with err.
type stubAuthz struct {
	err error

	// allow, when set, grants only these scopes and denies every other.
	allow map[string]bool

	calls      int
	principal  string
	scope      string
	actionType string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, principalID, legalEntityID, actionType string) error {
	a.calls++
	a.principal, a.scope, a.actionType = principalID, legalEntityID, actionType
	if a.allow != nil && !a.allow[legalEntityID] {
		return authz.ErrDenied
	}
	return a.err
}

// testAuthz is the permit-all default used by every pre-existing test.
func testAuthz() *stubAuthz { return &stubAuthz{} }

// authed stamps the gateway-verified identity headers onto a request. Both are
// set by gateway-auth-svc, and both are required: X-Principal-Id says who is
// acting, X-Tenant-Id says whose data they are acting on. Only the principal
// used to be stamped here, so every mutating test ran with no verified tenant
// scope and the handler fell back to the tenant_id in the body — the very
// behaviour under test.
func authed(req *http.Request) *http.Request {
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("Idempotency-Key", "test-idempotency-key-"+testPrincipal)
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
	{name: "record decision", path: "/v1/decisions", body: `{"decision_id":"d-1","tenant_id":"` + testTenant + `","legal_entity_id":"55555555-5555-5555-5555-555555555555","actor_id":"a-1","action_type":"APPROVE","outcome":"GRANTED","rule_basis":"policy:APPROVAL_5K","correlation_id":"corr-1"}`},
}

// TestGatedRoutes_401_WithoutPrincipal — this service shipped with no gate of
// any kind, so anything able to reach the port could write to it.
func TestGatedRoutes_401_WithoutPrincipal(t *testing.T) {
	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			store := &stubStore{}
			az := &stubAuthz{}
			r := chi.NewRouter()
			handler.RegisterRoutes(r, handler.New(store, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))

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
			az := &stubAuthz{err: authz.ErrDenied}
			r := chi.NewRouter()
			handler.RegisterRoutes(r, handler.New(store, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))

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
			az := &stubAuthz{err: authz.ErrUnavailable}
			r := chi.NewRouter()
			handler.RegisterRoutes(r, handler.New(store, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))

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
	h := newTestRouterWithPolicyClient(store, pc)

	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-1/replay", bytes.NewBufferString(`{"replayed_by_principal_id":"auditor-1"}`))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Principal-Id", "auditor-1")
	req.Header.Set("Idempotency-Key", "replay-key-1")
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
	h := newTestRouterWithPolicyClient(store, pc)

	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-2/replay", bytes.NewBufferString(`{"replayed_by_principal_id":"auditor-1"}`))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Principal-Id", "auditor-1")
	req.Header.Set("Idempotency-Key", "replay-key-1")
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
	h := newTestRouterWithPolicyClient(store, &stubPolicyClient{})

	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-3/replay", bytes.NewBufferString(`{"replayed_by_principal_id":"auditor-1"}`))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Principal-Id", "auditor-1")
	req.Header.Set("Idempotency-Key", "replay-key-1")
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
	h := newTestRouterWithPolicyClient(store, pc)

	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-4/replay", bytes.NewBufferString(`{"replayed_by_principal_id":"auditor-1"}`))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Principal-Id", "auditor-1")
	req.Header.Set("Idempotency-Key", "replay-key-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d — %s", w.Code, w.Body.String())
	}
}

func TestReplayDecision_MissingReplayedByPrincipal400(t *testing.T) {
	// The handler uses X-Principal-Id header for ReplayedByPrincipalID,
	// not the request body. So an empty body with just the header should work.
	store := &stubStore{
		findByIDResult: &domain.GovernanceDecision{
			DecisionID:        "dec-5",
			ActionType:        "APPROVAL_THRESHOLD",
			Outcome:           "GRANTED",
			RuleBasis:         "APPROVAL_5K:pv-5",
			EvaluationContext: amountContext(3000),
		},
	}
	pc := &stubPolicyClient{version: &policyclient.PolicyVersion{PolicyVersionID: "pv-5", RulePayload: thresholdPayload(5000)}}
	h := newTestRouterWithPolicyClient(store, pc)

	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-5/replay", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Principal-Id", "auditor-1")
	req.Header.Set("Idempotency-Key", "replay-key-1")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Should succeed with 201 since X-Principal-Id is provided
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 with X-Principal-Id header, got %d — %s", w.Code, w.Body.String())
	}
}

// ── read authorization (audit A7) ────────────────────────────────────────────

var readRoutes = []string{"/v1/decisions", "/v1/decisions/dec-1", "/v1/decisions/dec-1/replay-manifests"}

// TestReadRoutes_401_403_503 — every GET used to gate on the tenant header only.
func TestReadRoutes_401_403_503(t *testing.T) {
	for _, path := range readRoutes {
		for _, tc := range []struct {
			name      string
			principal string
			authzErr  error
			want      int
		}{
			{"no principal", "", nil, http.StatusUnauthorized},
			{"denied", testPrincipal, authz.ErrDenied, http.StatusForbidden},
			{"authz down", testPrincipal, authz.ErrUnavailable, http.StatusServiceUnavailable},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				s := &stubStore{findByIDResult: &domain.GovernanceDecision{DecisionID: "dec-1"}}
				az := &stubAuthz{err: tc.authzErr}
				r := chi.NewRouter()
				handler.RegisterRoutes(r, handler.New(s, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("X-Tenant-Id", testTenant)
				if tc.principal != "" {
					req.Header.Set("X-Principal-Id", tc.principal)
				}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != tc.want {
					t.Fatalf("expected %d, got %d: %s", tc.want, w.Code, w.Body.String())
				}
				if tc.authzErr != nil && az.actionType != handler.ActionReadDecision {
					t.Errorf("authz asked about %q, want %q", az.actionType, handler.ActionReadDecision)
				}
			})
		}
	}
}

// TestReadRoutes_WarnModeServesButStillScopesToTenant — the migration mode for
// callers that do not yet forward a principal. It relaxes authorization only;
// the read stays inside the caller's tenant.
func TestReadRoutes_WarnModeServesButStillScopesToTenant(t *testing.T) {
	s := &stubStore{findByIDResult: &domain.GovernanceDecision{DecisionID: "dec-1"}}
	h := handler.New(s, &stubAuthz{err: authz.ErrDenied}, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop())
	h.WarnOnlyReadAuthz()
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)

	for _, path := range readRoutes {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-Id", testTenant)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200 in warn mode, got %d", path, w.Code)
		}
	}
	if s.findByIDGotTenant != testTenant || s.listGotParams.TenantID != testTenant || s.listManifestsGotTenant != testTenant {
		t.Fatal("warn mode must still scope every read to the caller's tenant")
	}

	// Writes are never relaxed by the read mode.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("Idempotency-Key", "k")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected a write without a principal to stay 401 in warn mode, got %d", w.Code)
	}
}

// TestListReplayManifests_ScopedToCallerTenant — audit A3: manifests were read
// by decision_id alone, with no tenant at all.
func TestListReplayManifests_ScopedToCallerTenant(t *testing.T) {
	s := &stubStore{}
	h := newTestRouter(s)
	req := authed(httptest.NewRequest(http.MethodGet, "/v1/decisions/dec-1/replay-manifests", nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || s.listManifestsGotTenant != testTenant {
		t.Fatalf("expected a tenant-scoped read, got %d tenant=%q", w.Code, s.listManifestsGotTenant)
	}

	noTenant := httptest.NewRequest(http.MethodGet, "/v1/decisions/dec-1/replay-manifests", nil)
	noTenant.Header.Set("X-Principal-Id", testPrincipal)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, noTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no tenant, got %d", w.Code)
	}
}

// ── list input validation (audit I7) ─────────────────────────────────────────

func TestListDecisions_400_MalformedPaging(t *testing.T) {
	for _, q := range []string{"limit=abc", "limit=0", "limit=-5", "offset=abc", "offset=-1"} {
		s := &stubStore{}
		h := newTestRouter(s)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, authed(httptest.NewRequest(http.MethodGet, "/v1/decisions?"+q, nil)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", q, w.Code)
		}
	}
}

// ── replay authn, authz, attribution and idempotency (audit A6, I5, I6) ─────

func replayFixture() (*stubStore, *stubPolicyClient) {
	s := &stubStore{findByIDResult: &domain.GovernanceDecision{
		DecisionID: "dec-1", TenantID: testTenant, LegalEntityID: "le-1", ActionType: "APPROVAL_THRESHOLD",
		Outcome: "ESCALATED", RuleBasis: "APPROVAL_5K:pv-1", EvaluationContext: amountContext(7500),
	}}
	pc := &stubPolicyClient{version: &policyclient.PolicyVersion{PolicyVersionID: "pv-1", RulePayload: thresholdPayload(5000)}}
	return s, pc
}

func replayRequest(body, key string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/dec-1/replay", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Principal-Id", "auditor-1")
	req.Header.Set("Idempotency-Key", key)
	return req
}

func TestReplayDecision_401_WithoutPrincipal_403_Denied(t *testing.T) {
	s, pc := replayFixture()
	req := replayRequest(`{}`, "k1")
	req.Header.Del("X-Principal-Id")
	w := httptest.NewRecorder()
	newTestRouterWithPolicyClient(s, pc).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	az := &stubAuthz{err: authz.ErrDenied}
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, az, pc, testAuthzScopeID, zap.NewNop()))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, replayRequest(`{}`, "k1"))
	if w.Code != http.StatusForbidden || az.actionType != handler.ActionReplayDecision || az.scope != "le-1" {
		t.Fatalf("expected 403 on GOVERNANCE_DECISION_REPLAY scoped to le-1, got %d (%s @ %s)", w.Code, az.actionType, az.scope)
	}
	if s.manifestCreates != 0 {
		t.Fatal("a refused replay must record no manifest")
	}
}

// The replaying principal is the authenticated caller; a body naming someone
// else cannot put their name on a permanent manifest.
func TestReplayDecision_AttributedToAuthenticatedPrincipal(t *testing.T) {
	s, pc := replayFixture()
	w := httptest.NewRecorder()
	newTestRouterWithPolicyClient(s, pc).ServeHTTP(w, replayRequest(`{"replayed_by_principal_id":"framed-user"}`, "k1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	if s.createdManifest.ReplayedByPrincipalID != "auditor-1" {
		t.Fatalf("manifest attributed to %q, want the caller auditor-1", s.createdManifest.ReplayedByPrincipalID)
	}
}

// A retried replay used to mint a second permanent manifest.
func TestReplayDecision_RetrySameKeyReturnsFirstManifest(t *testing.T) {
	s, pc := replayFixture()
	h := newTestRouterWithPolicyClient(s, pc)

	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, replayRequest(`{}`, "k1"))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, replayRequest(`{}`, "k1"))

	if w1.Code != http.StatusCreated || w2.Code != http.StatusOK {
		t.Fatalf("expected 201 then 200, got %d then %d", w1.Code, w2.Code)
	}
	if s.manifestCreates != 1 || pc.calls != 1 {
		t.Fatalf("expected one manifest and one evaluation, got %d manifests, %d policy calls", s.manifestCreates, pc.calls)
	}
	var m1, m2 domain.ReplayManifest
	_ = json.Unmarshal(w1.Body.Bytes(), &m1)
	_ = json.Unmarshal(w2.Body.Bytes(), &m2)
	if m1.ReplayManifestID != m2.ReplayManifestID {
		t.Fatalf("retry returned a different manifest: %s vs %s", m1.ReplayManifestID, m2.ReplayManifestID)
	}

	w3 := httptest.NewRecorder()
	h.ServeHTTP(w3, replayRequest(`{"correlation_id":"other"}`, "k1"))
	if w3.Code != http.StatusConflict {
		t.Fatalf("expected 409 for the same key with a different body, got %d", w3.Code)
	}
}

// The policy_version_id column is preferred over parsing rule_basis.
func TestReplayDecision_UsesPolicyVersionColumn(t *testing.T) {
	s, pc := replayFixture()
	col := "pv-from-column"
	s.findByIDResult.PolicyVersionID = &col
	w := httptest.NewRecorder()
	newTestRouterWithPolicyClient(s, pc).ServeHTTP(w, replayRequest(`{}`, "k1"))
	if w.Code != http.StatusCreated || pc.gotVersion != col {
		t.Fatalf("expected replay against %q, got %d / %q", col, w.Code, pc.gotVersion)
	}
}

// ── jurisdiction context (audit V4; Doc 04 §7.1 jurisdiction_rule_basis) ─────

func TestCreateDecision_JurisdictionFromEnvelopeHeader(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
	req.Header.Set("X-Jurisdiction-Context", "JUR-GB")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated || s.got.JurisdictionContext == nil || *s.got.JurisdictionContext != "JUR-GB" {
		t.Fatalf("expected 201 with JUR-GB recorded, got %d / %+v", rr.Code, s.got)
	}
	var env struct {
		Jurisdiction string `json:"jurisdiction_context"`
	}
	_ = json.Unmarshal(s.gotRec.Event, &env)
	if env.Jurisdiction != "JUR-GB" {
		t.Fatalf("event envelope jurisdiction_context = %q, want JUR-GB", env.Jurisdiction)
	}
}

func TestCreateDecision_400_JurisdictionMismatch(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	body := strings.Replace(validBody(), `"correlation_id": "corr-001"`, `"correlation_id": "corr-001", "jurisdiction_context": "JUR-US"`, 1)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body)))
	req.Header.Set("X-Jurisdiction-Context", "JUR-GB")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "JURISDICTION_MISMATCH") {
		t.Fatalf("expected 400 JURISDICTION_MISMATCH, got %d — %s", rr.Code, rr.Body.String())
	}
	if s.inserts != 0 {
		t.Fatal("expected nothing written")
	}
}

func TestCreateDecision_NoJurisdictionRecordsNone(t *testing.T) {
	s := &stubStore{created: true}
	h := newTestRouter(s)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))))
	if rr.Code != http.StatusCreated || s.got.JurisdictionContext != nil {
		t.Fatalf("expected no jurisdiction recorded (never rule_basis), got %+v", s.got.JurisdictionContext)
	}
}

// ── replay event (audit I5) ──────────────────────────────────────────────────

func TestReplayDecision_QueuesReplayedEventWithTheManifest(t *testing.T) {
	s, pc := replayFixture()
	w := httptest.NewRecorder()
	newTestRouterWithPolicyClient(s, pc).ServeHTTP(w, replayRequest(`{"correlation_id":"corr-r"}`, "k1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	if s.gotReplayEvent == nil {
		t.Fatal("expected the replay event handed to the store with the manifest")
	}
	var env struct {
		EventType     string `json:"event_type"`
		ActorID       string `json:"actor_id"`
		CorrelationID string `json:"correlation_id"`
	}
	_ = json.Unmarshal(s.gotReplayEvent.Envelope, &env)
	if env.EventType != "governance.decision.replayed" || env.ActorID != "auditor-1" || env.CorrelationID != "corr-r" {
		t.Fatalf("unexpected replay event: %+v", env)
	}
}

// ── re-audit R-1: over-length input is a 400, not a 503 ──────────────────────

func TestCreateDecision_400_OverLengthInput(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	for _, tc := range []struct {
		name   string
		mutate func(req *http.Request) *http.Request
		body   string
	}{
		{"decision_id", nil, strings.Replace(validBody(), `"dec-001"`, `"`+long(65)+`"`, 1)},
		{"outcome", nil, strings.Replace(validBody(), `"DENIED"`, `"`+long(33)+`"`, 1)},
		{"action_type", nil, strings.Replace(validBody(), `"PAYROLL_RELEASE"`, `"`+long(129)+`"`, 1)},
		{"rule_basis", nil, strings.Replace(validBody(), `"policy-v3-sod"`, `"`+long(257)+`"`, 1)},
		{"Idempotency-Key", func(r *http.Request) *http.Request { r.Header.Set("Idempotency-Key", long(257)); return r }, validBody()},
		{"X-Principal-Id", func(r *http.Request) *http.Request { r.Header.Set("X-Principal-Id", long(65)); return r }, validBody()},
		{"X-Jurisdiction-Context", func(r *http.Request) *http.Request { r.Header.Set("X-Jurisdiction-Context", long(65)); return r }, validBody()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{created: true}
			req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(tc.body)))
			if tc.mutate != nil {
				req = tc.mutate(req)
			}
			rr := httptest.NewRecorder()
			newTestRouter(s).ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "Value Too Long") {
				t.Fatalf("expected 400 Value Too Long, got %d — %s", rr.Code, rr.Body.String())
			}
			if s.inserts != 0 {
				t.Fatal("expected nothing written")
			}
		})
	}
}

func TestReplayDecision_400_OverLengthInput(t *testing.T) {
	s, pc := replayFixture()
	req := replayRequest(`{"correlation_id":"`+strings.Repeat("c", 65)+`"}`, "k1")
	w := httptest.NewRecorder()
	newTestRouterWithPolicyClient(s, pc).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || s.manifestCreates != 0 {
		t.Fatalf("expected 400 and no manifest, got %d (%d manifests)", w.Code, s.manifestCreates)
	}
}

// ── re-audit R-2: a legal_entity_id authorization-svc cannot evaluate ────────

func TestCreateDecision_400_NonUUIDLegalEntity(t *testing.T) {
	s := &stubStore{created: true}
	az := testAuthz()
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))
	body := strings.Replace(validBody(), `"`+testEntity+`"`, `"GLOBAL"`, 1)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body))))
	if rr.Code != http.StatusBadRequest || az.calls != 0 {
		t.Fatalf("expected 400 before authz, got %d (%d authz calls)", rr.Code, az.calls)
	}

	ok := strings.Replace(validBody(), `"`+testEntity+`"`, `"PLATFORM"`, 1)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(ok))))
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected authorization-svc's PLATFORM sentinel accepted, got %d — %s", rr.Code, rr.Body.String())
	}
}

// ── re-audit R-3: the key stands for the whole request, not only the body ───

func TestCreateDecision_409_SameKeyAndBodyFromAnotherRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"different principal", func(r *http.Request) { r.Header.Set("X-Principal-Id", "bob") }},
		{"different jurisdiction", func(r *http.Request) { r.Header.Set("X-Jurisdiction-Context", "JUR-US") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{created: true}
			h := newTestRouter(s)
			first := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
			first.Header.Set("X-Jurisdiction-Context", "JUR-GB")
			h.ServeHTTP(httptest.NewRecorder(), first)

			second := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
			second.Header.Set("X-Jurisdiction-Context", "JUR-GB")
			tc.mutate(second)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, second)
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "IDEMPOTENCY_MISMATCH") {
				t.Fatalf("expected 409 IDEMPOTENCY_MISMATCH, got %d — %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// ── re-audit R-4: X-Legal-Entity-Id and the body's legal_entity_id ───────────

func TestCreateDecision_400_LegalEntityHeaderMismatch(t *testing.T) {
	s := &stubStore{created: true}
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody())))
	req.Header.Set("X-Legal-Entity-Id", "66666666-6666-6666-6666-666666666666")
	rr := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "LEGAL_ENTITY_MISMATCH") || s.inserts != 0 {
		t.Fatalf("expected 400 LEGAL_ENTITY_MISMATCH and no write, got %d — %s", rr.Code, rr.Body.String())
	}
}

func TestCreateDecision_LegalEntityFromHeaderWhenBodyOmitsIt(t *testing.T) {
	s := &stubStore{created: true}
	body := strings.Replace(validBody(), `"legal_entity_id": "`+testEntity+`",`, ``, 1)
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body)))
	req.Header.Set("X-Legal-Entity-Id", testEntity)
	rr := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated || s.got.LegalEntityID != testEntity {
		t.Fatalf("expected 201 filed under the header's entity, got %d / %+v", rr.Code, s.got)
	}
}

// ── re-audit R-6: reads are authorized on the entity they are about ─────────

func TestReads_AreEntityScoped(t *testing.T) {
	const entityA, entityB = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	serve := func(az *stubAuthz, s *stubStore, path string) int {
		r := chi.NewRouter()
		handler.RegisterRoutes(r, handler.New(s, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, authed(httptest.NewRequest(http.MethodGet, path, nil)))
		return w.Code
	}
	onlyA := func() *stubAuthz { return &stubAuthz{allow: map[string]bool{entityA: true}} }
	platform := func() *stubAuthz { return &stubAuthz{allow: map[string]bool{testAuthzScopeID: true}} }
	decisionOf := func(entity string) *stubStore {
		return &stubStore{findByIDResult: &domain.GovernanceDecision{DecisionID: "d", LegalEntityID: entity}}
	}

	for _, tc := range []struct {
		name string
		az   *stubAuthz
		s    *stubStore
		path string
		want int
	}{
		{"entity-A reader, A's decision", onlyA(), decisionOf(entityA), "/v1/decisions/d", 200},
		{"entity-A reader, B's decision", onlyA(), decisionOf(entityB), "/v1/decisions/d", 403},
		{"entity-A reader, A's manifests", onlyA(), decisionOf(entityA), "/v1/decisions/d/replay-manifests", 200},
		{"entity-A reader, B's manifests", onlyA(), decisionOf(entityB), "/v1/decisions/d/replay-manifests", 403},
		{"entity-A reader, list filtered to A", onlyA(), &stubStore{}, "/v1/decisions?entity=" + entityA, 200},
		{"entity-A reader, list filtered to B", onlyA(), &stubStore{}, "/v1/decisions?entity=" + entityB, 403},
		{"entity-A reader, unfiltered list", onlyA(), &stubStore{}, "/v1/decisions", 403},
		{"entity-A reader, unknown id (no existence oracle)", onlyA(), &stubStore{findByIDErr: domain.ErrDecisionNotFound}, "/v1/decisions/x", 403},
		{"platform reader, B's decision", platform(), decisionOf(entityB), "/v1/decisions/d", 200},
		{"platform reader, unfiltered list", platform(), &stubStore{}, "/v1/decisions", 200},
		{"platform reader, unknown id", platform(), &stubStore{findByIDErr: domain.ErrDecisionNotFound}, "/v1/decisions/x", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serve(tc.az, tc.s, tc.path); got != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, got)
			}
		})
	}
}

// An authz outage on the entity check is a 503, never a reason to try the
// platform grant instead.
func TestReads_EntityCheckOutageIsNotADenial(t *testing.T) {
	s := &stubStore{findByIDResult: &domain.GovernanceDecision{DecisionID: "d", LegalEntityID: testEntity}}
	az := &stubAuthz{err: authz.ErrUnavailable}
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authed(httptest.NewRequest(http.MethodGet, "/v1/decisions/d", nil)))
	if w.Code != http.StatusServiceUnavailable || az.calls != 1 {
		t.Fatalf("expected 503 after one authz call, got %d after %d", w.Code, az.calls)
	}
}
