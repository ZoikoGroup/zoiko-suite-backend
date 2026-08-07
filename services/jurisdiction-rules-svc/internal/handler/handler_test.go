package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ── stub store ────────────────────────────────────────────────────────────────

// stubStore implements handler.JurisdictionStore for unit testing.
// No DB, no network — purely in-memory.
type stubStore struct {
	jurisdiction  *domain.Jurisdiction
	jurisdictions []*domain.Jurisdiction
	ancestors     []*domain.Jurisdiction
	rules         []*domain.JurisdictionRule
	rulesErr      error
	err           error

	rulePack    *domain.RulePack
	rulePackErr error

	// admin-mutation fields — configured per-test, unused by the read tests above
	createdJurisdiction    *domain.Jurisdiction
	jurisdictionWasCreated bool
	createJurisdictionErr  error
	createJurisdictionArgs domain.CreateJurisdictionParams

	deactivatedJurisdiction *domain.Jurisdiction
	deactivateErr           error
	deactivateActorID       string

	ruleByID    *domain.JurisdictionRule
	ruleByIDErr error

	createdRule    *domain.JurisdictionRule
	ruleWasCreated bool
	createRuleErr  error
	createRuleArgs domain.CreateRuleParams

	transitionedRule *domain.JurisdictionRule
	transitionDidRun bool
	transitionErr    error
	transitionArgs   store.TransitionParams

	driftRule    *domain.JurisdictionRule
	driftEvent   *domain.DriftEvent
	driftChanged bool
	driftErr     error
	driftArgs    domain.RecordDriftParams

	driftEvents    []*domain.DriftEvent
	driftEventsErr error

	// paging captures what the handler passed down, so the tests can assert
	// that a rejected limit/offset never reached the store at all.
	listParams  store.ListParams
	rulesParams store.FindRulesParams
	storeCalled bool
}

func (s *stubStore) CreateJurisdiction(_ context.Context, p domain.CreateJurisdictionParams) (*domain.Jurisdiction, bool, error) {
	s.storeCalled = true
	s.createJurisdictionArgs = p
	return s.createdJurisdiction, s.jurisdictionWasCreated, s.createJurisdictionErr
}

func (s *stubStore) DeactivateJurisdiction(_ context.Context, _, actorID string) (*domain.Jurisdiction, error) {
	s.storeCalled = true
	s.deactivateActorID = actorID
	return s.deactivatedJurisdiction, s.deactivateErr
}

func (s *stubStore) FindRuleByID(_ context.Context, _ string) (*domain.JurisdictionRule, error) {
	s.storeCalled = true
	return s.ruleByID, s.ruleByIDErr
}

func (s *stubStore) CreateRule(_ context.Context, p domain.CreateRuleParams) (*domain.JurisdictionRule, bool, error) {
	s.storeCalled = true
	s.createRuleArgs = p
	return s.createdRule, s.ruleWasCreated, s.createRuleErr
}

func (s *stubStore) TransitionRuleStatus(_ context.Context, p store.TransitionParams) (*domain.JurisdictionRule, bool, error) {
	s.storeCalled = true
	s.transitionArgs = p
	return s.transitionedRule, s.transitionDidRun, s.transitionErr
}

func (s *stubStore) RecordDrift(_ context.Context, p domain.RecordDriftParams) (*domain.JurisdictionRule, *domain.DriftEvent, bool, error) {
	s.storeCalled = true
	s.driftArgs = p
	return s.driftRule, s.driftEvent, s.driftChanged, s.driftErr
}

func (s *stubStore) FindDriftEvents(_ context.Context, _ string, _, _ int) ([]*domain.DriftEvent, error) {
	s.storeCalled = true
	return s.driftEvents, s.driftEventsErr
}

func (s *stubStore) FindByID(_ context.Context, _ string) (*domain.Jurisdiction, error) {
	s.storeCalled = true
	return s.jurisdiction, s.err
}

func (s *stubStore) List(_ context.Context, p store.ListParams) ([]*domain.Jurisdiction, error) {
	s.storeCalled = true
	s.listParams = p
	return s.jurisdictions, s.err
}

func (s *stubStore) FindAncestors(_ context.Context, _ string) ([]*domain.Jurisdiction, error) {
	s.storeCalled = true
	return s.ancestors, s.err
}

func (s *stubStore) FindRulePack(_ context.Context, _, _ string, _ time.Time) (*domain.RulePack, error) {
	s.storeCalled = true
	return s.rulePack, s.rulePackErr
}

func (s *stubStore) FindRules(_ context.Context, params store.FindRulesParams) ([]*domain.JurisdictionRule, error) {
	s.storeCalled = true
	s.rulesParams = params
	if s.rulesErr != nil {
		return nil, s.rulesErr
	}
	var filtered []*domain.JurisdictionRule
	effectiveAt := params.EffectiveAt
	if effectiveAt.IsZero() {
		effectiveAt = time.Now().UTC()
	}
	for _, rule := range s.rules {
		if rule.JurisdictionID != params.JurisdictionID {
			continue
		}
		if params.Domain != "" && rule.RuleDomain != params.Domain {
			continue
		}
		if rule.RuleStatus == "DRAFT" {
			continue
		}
		if rule.EffectiveFrom.After(effectiveAt) {
			continue
		}
		if rule.EffectiveTo != nil && !rule.EffectiveTo.After(effectiveAt) {
			continue
		}
		filtered = append(filtered, rule)
	}
	// Sort by RuleDomain asc, EffectiveFrom asc
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].RuleDomain != filtered[j].RuleDomain {
			return filtered[i].RuleDomain < filtered[j].RuleDomain
		}
		return filtered[i].EffectiveFrom.Before(filtered[j].EffectiveFrom)
	})
	// Apply limit and offset
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	offset := params.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > len(filtered) {
		offset = len(filtered)
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	return filtered[offset:end], nil
}

// ── spy publisher ─────────────────────────────────────────────────────────────

// spyPublisher records every event the handler emits so tests can assert both
// that a real write publishes and — the more important half — that an
// idempotent replay does not.
type spyPublisher struct {
	emitted []string
	err     error
}

func (p *spyPublisher) record(eventType string) error {
	p.emitted = append(p.emitted, eventType)
	return p.err
}

func (p *spyPublisher) PublishJurisdictionCreated(context.Context, domain.Jurisdiction, string) error {
	return p.record("jurisdiction.created")
}

func (p *spyPublisher) PublishJurisdictionDeactivated(context.Context, domain.Jurisdiction, string) error {
	return p.record("jurisdiction.deactivated")
}

func (p *spyPublisher) PublishRuleUpdated(context.Context, domain.JurisdictionRule, string) error {
	return p.record("jurisdiction.rule.updated")
}

func (p *spyPublisher) PublishRuleActivated(context.Context, domain.JurisdictionRule, string) error {
	return p.record("jurisdiction.rule.activated")
}

func (p *spyPublisher) PublishLegalDriftDetected(context.Context, domain.JurisdictionRule, domain.DriftEvent, string) error {
	return p.record("legal.drift.detected")
}

func (p *spyPublisher) has(eventType string) bool {
	for _, e := range p.emitted {
		if e == eventType {
			return true
		}
	}
	return false
}

// ── helpers ───────────────────────────────────────────────────────────────────

// testAuthzScopeID stands in for config.AuthZPlatformScopeID.
const testAuthzScopeID = "00000000-0000-0000-0000-0000000000ff"

// newTestRouter wires a Handler onto a chi router exactly as main.go does.
// Read-path tests don't exercise AuthZ, so they get a permit-all stub.
func newTestRouter(store handler.JurisdictionStore) http.Handler {
	return newTestRouterWithAuthz(store, authz.NewStubAuthZClient(zap.NewNop()))
}

// newTestRouterWithAuthz is the same wiring, with an explicit AuthorizationClient
// — used by admin-mutation tests that need to exercise 403/503 authz paths.
func newTestRouterWithAuthz(store handler.JurisdictionStore, authzClient authz.AuthorizationClient) http.Handler {
	router, _ := newTestRouterWithPublisher(store, authzClient)
	return router
}

// newTestRouterWithPublisher additionally returns the spy publisher so a test
// can assert on emitted events.
func newTestRouterWithPublisher(store handler.JurisdictionStore, authzClient authz.AuthorizationClient) (http.Handler, *spyPublisher) {
	pub := &spyPublisher{}
	r := chi.NewRouter()
	h := handler.New(store, authzClient, pub, testAuthzScopeID, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r, pub
}

// executeRequest fires req against the given handler and returns the recorder.
func executeRequest(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestGetJurisdiction_200_ActiveExists verifies the happy path:
// an active, non-expired jurisdiction returns 200 with the full JSON body.
func TestGetJurisdiction_200_ActiveExists(t *testing.T) {
	now := time.Now().UTC()
	want := &domain.Jurisdiction{
		JurisdictionID:       "01J000000000000000000000AA",
		JurisdictionCode:     "GB",
		JurisdictionName:     "United Kingdom",
		JurisdictionType:     "COUNTRY",
		AuthorityType:        "FEDERAL",
		EffectiveFrom:        now.Add(-365 * 24 * time.Hour),
		ActiveFlag:           true,
		DataClassification:   "PUBLIC",
		CreatedAt:            now.Add(-365 * 24 * time.Hour),
		CreatedByPrincipalID: "principal-test",
		SchemaVersion:        "1.0",
	}

	h := newTestRouter(&stubStore{jurisdiction: want})
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/01J000000000000000000000AA", nil)
	req.Header.Set("X-Correlation-ID", "corr-001")

	rr := executeRequest(h, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}

	var got domain.Jurisdiction
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if got.JurisdictionID != want.JurisdictionID {
		t.Errorf("jurisdiction_id mismatch: got %q, want %q", got.JurisdictionID, want.JurisdictionID)
	}
	if got.JurisdictionCode != want.JurisdictionCode {
		t.Errorf("jurisdiction_code mismatch: got %q, want %q", got.JurisdictionCode, want.JurisdictionCode)
	}
	// data_classification is part of the response contract
	// (data_classification_audit.md §2.11) — consumers read the tier off the
	// record rather than inferring it.
	if got.DataClassification != "PUBLIC" {
		t.Errorf("data_classification = %q, want PUBLIC", got.DataClassification)
	}
	if rr.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", rr.Header().Get("Content-Type"))
	}
}

// TestGetJurisdiction_404_NotFound verifies that domain.ErrJurisdictionNotFound
// produces a 404 — not a 503.
// This covers: unknown ID, inactive (active_flag=false), and expired (effective_to in past).
// All three map to ErrJurisdictionNotFound in the store layer (SQL returns no rows).
func TestGetJurisdiction_404_NotFound(t *testing.T) {
	h := newTestRouter(&stubStore{err: domain.ErrJurisdictionNotFound})
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/does-not-exist", nil)

	rr := executeRequest(h, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d — body: %s", rr.Code, rr.Body.String())
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if body["error"] != "jurisdiction_not_found" {
		t.Errorf("expected error=jurisdiction_not_found, got %q", body["error"])
	}
}

// TestGetJurisdiction_503_StoreUnavailable is the critical test that was missing
// before this PR was opened. It proves that a database failure returns 503 and
// NOT 404. This distinction is what enforces the fail-closed contract:
// tenant-entity-registry-svc must reject an assignment when it gets 503,
// not silently accept it as "not found".
func TestGetJurisdiction_503_StoreUnavailable(t *testing.T) {
	h := newTestRouter(&stubStore{err: domain.ErrStoreUnavailable})
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/any-id", nil)
	req.Header.Set("X-Correlation-ID", "corr-503-test")

	rr := executeRequest(h, req)

	// This MUST be 503, not 404. If it were 404, a DB outage would look like
	// "jurisdiction not found" and the assignment could slip through depending
	// on caller behaviour. We test the status code explicitly.
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on store unavailability, got %d — body: %s", rr.Code, rr.Body.String())
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if body["error"] != "store_unavailable" {
		t.Errorf("expected error=store_unavailable, got %q", body["error"])
	}
}

// TestGetJurisdiction_503_IsDistinctFrom_404 explicitly asserts the status codes
// differ — making the pass/fail criterion machine-readable in CI and impossible
// to accidentally swap.
func TestGetJurisdiction_503_IsDistinctFrom_404(t *testing.T) {
	notFoundStore := &stubStore{err: domain.ErrJurisdictionNotFound}
	unavailableStore := &stubStore{err: domain.ErrStoreUnavailable}

	req404 := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/x", nil)
	req503 := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/x", nil)

	rr404 := executeRequest(newTestRouter(notFoundStore), req404)
	rr503 := executeRequest(newTestRouter(unavailableStore), req503)

	if rr404.Code != http.StatusNotFound {
		t.Errorf("not-found error: expected 404, got %d", rr404.Code)
	}
	if rr503.Code != http.StatusServiceUnavailable {
		t.Errorf("unavailable error: expected 503, got %d", rr503.Code)
	}
	if rr404.Code == rr503.Code {
		t.Errorf("FAIL: 404 and 503 paths returned the same status %d — fail-closed contract is broken", rr404.Code)
	}
}

// TestGetJurisdiction_CorrelationID verifies that X-Correlation-ID is echoed
// back in the response headers on both success and error paths.
func TestGetJurisdiction_CorrelationID(t *testing.T) {
	tests := []struct {
		name   string
		store  handler.JurisdictionStore
		corrID string
	}{
		{
			name:   "echo on 200",
			store:  &stubStore{jurisdiction: &domain.Jurisdiction{JurisdictionID: "x", SchemaVersion: "1.0"}},
			corrID: "trace-abc",
		},
		{
			name:   "echo on 404",
			store:  &stubStore{err: domain.ErrJurisdictionNotFound},
			corrID: "trace-def",
		},
		{
			name:   "echo on 503",
			store:  &stubStore{err: domain.ErrStoreUnavailable},
			corrID: "trace-ghi",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/any", nil)
			req.Header.Set("X-Correlation-ID", tc.corrID)
			rr := executeRequest(newTestRouter(tc.store), req)

			got := rr.Header().Get("X-Correlation-ID")
			if got != tc.corrID {
				t.Errorf("correlation ID not echoed: got %q, want %q", got, tc.corrID)
			}
		})
	}
}

// ── ListJurisdictions tests ───────────────────────────────────────────────────

// TestListJurisdictions_200_ReturnsArray verifies that a successful store.List
// returns 200 with a JSON array body.
func TestListJurisdictions_200_ReturnsArray(t *testing.T) {
	items := []*domain.Jurisdiction{
		{JurisdictionID: "aaa", JurisdictionCode: "DE", JurisdictionType: "COUNTRY", SchemaVersion: "1.0"},
		{JurisdictionID: "bbb", JurisdictionCode: "FR", JurisdictionType: "COUNTRY", SchemaVersion: "1.0"},
	}
	h := newTestRouter(&stubStore{jurisdictions: items})
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions?type=COUNTRY&active=true", nil)

	rr := executeRequest(h, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got []domain.Jurisdiction
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode list body: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 jurisdictions, got %d", len(got))
	}
}

// TestListJurisdictions_200_EmptyArrayNotNull ensures that when the store
// returns nil (no rows), the handler still returns [] and NOT null.
// Callers that iterate the JSON array must not get a null-pointer surprise.
func TestListJurisdictions_200_EmptyArrayNotNull(t *testing.T) {
	h := newTestRouter(&stubStore{jurisdictions: nil})
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions", nil)

	rr := executeRequest(h, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	// Must start with "[" not "null".
	if len(body) == 0 || body[0] != '[' {
		t.Errorf("expected JSON array, got: %s", body)
	}
}

// TestListJurisdictions_503_StoreUnavailable verifies that a store error
// on List returns 503.
func TestListJurisdictions_503_StoreUnavailable(t *testing.T) {
	h := newTestRouter(&stubStore{err: domain.ErrStoreUnavailable})
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions", nil)

	rr := executeRequest(h, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if body["error"] != "store_unavailable" {
		t.Errorf("expected error=store_unavailable, got %q", body["error"])
	}
}

// TestPaging_400_RejectsMalformedValues covers a class of bug rather than one
// case: limit and offset were parsed with the error discarded, so "limit=abc"
// silently became the default and "offset=-1" reached Postgres, which rejects
// a negative OFFSET — surfacing as 503 store_unavailable. A client typo must
// not be reported as a platform outage.
func TestPaging_400_RejectsMalformedValues(t *testing.T) {
	paths := []string{
		"/v1/jurisdictions?%s",
		"/v1/jurisdictions/j-1/rules?%s",
		"/v1/rules/r-1/drift-events?%s",
	}
	queries := map[string]string{
		"non-numeric limit":  "limit=abc",
		"negative limit":     "limit=-1",
		"non-numeric offset": "offset=xyz",
		"negative offset":    "offset=-1",
	}

	for _, pathTemplate := range paths {
		for name, query := range queries {
			t.Run(pathTemplate+" "+name, func(t *testing.T) {
				st := &stubStore{}
				path := pathTemplate[:len(pathTemplate)-2] + query
				rr := executeRequest(newTestRouter(st), httptest.NewRequest(http.MethodGet, path, nil))

				if rr.Code != http.StatusBadRequest {
					t.Fatalf("expected 400 for %q, got %d — body: %s", query, rr.Code, rr.Body.String())
				}
				if st.storeCalled {
					t.Error("store was queried with an invalid paging parameter — it should have been rejected first")
				}
			})
		}
	}
}

// TestListJurisdictions_PagingReachesStore is the other half of the test
// above: valid values must actually be applied, not just accepted.
func TestListJurisdictions_PagingReachesStore(t *testing.T) {
	st := &stubStore{}
	rr := executeRequest(newTestRouter(st), httptest.NewRequest(http.MethodGet, "/v1/jurisdictions?limit=7&offset=14", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if st.listParams.Limit != 7 || st.listParams.Offset != 14 {
		t.Errorf("store received limit=%d offset=%d, want 7/14", st.listParams.Limit, st.listParams.Offset)
	}
}

// ── GetAncestors tests ────────────────────────────────────────────────────────

// TestGetAncestors_200_Chain verifies that a non-empty ancestor list returns
// 200 with an ordered JSON array.
func TestGetAncestors_200_Chain(t *testing.T) {
	chain := []*domain.Jurisdiction{
		{JurisdictionID: "parent-id", JurisdictionCode: "EU", JurisdictionType: "SUPRANATIONAL", SchemaVersion: "1.0"},
	}
	h := newTestRouter(&stubStore{ancestors: chain})
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/child-id/ancestors", nil)

	rr := executeRequest(h, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got []domain.Jurisdiction
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode ancestors body: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 ancestor, got %d", len(got))
	}
	if got[0].JurisdictionID != "parent-id" {
		t.Errorf("expected ancestor ID parent-id, got %q", got[0].JurisdictionID)
	}
}

// TestGetAncestors_200_RootReturnsEmptyArray verifies that a root jurisdiction
// (no parent) returns 200 with an empty array — NOT 404.
func TestGetAncestors_200_RootReturnsEmptyArray(t *testing.T) {
	h := newTestRouter(&stubStore{ancestors: nil}) // nil = root, no ancestors
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/root-id/ancestors", nil)

	rr := executeRequest(h, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for root jurisdiction, got %d", rr.Code)
	}
	body := rr.Body.String()
	if len(body) == 0 || body[0] != '[' {
		t.Errorf("expected empty JSON array [], got: %s", body)
	}
}

// TestGetAncestors_404_JurisdictionNotFound verifies that an unknown
// jurisdiction_id returns 404 from the ancestors endpoint.
func TestGetAncestors_404_JurisdictionNotFound(t *testing.T) {
	h := newTestRouter(&stubStore{err: domain.ErrJurisdictionNotFound})
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/unknown-id/ancestors", nil)

	rr := executeRequest(h, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if body["error"] != "jurisdiction_not_found" {
		t.Errorf("expected error=jurisdiction_not_found, got %q", body["error"])
	}
}

// TestGetAncestors_503_StoreUnavailable verifies that a database failure
// during ancestor traversal returns 503 — not 404.
func TestGetAncestors_503_StoreUnavailable(t *testing.T) {
	h := newTestRouter(&stubStore{err: domain.ErrStoreUnavailable})
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/any-id/ancestors", nil)

	rr := executeRequest(h, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if body["error"] != "store_unavailable" {
		t.Errorf("expected error=store_unavailable, got %q", body["error"])
	}
}

// TestFindRules_SupersededRuleReturnedForHistoricalQuery verifies that when querying
// for a point-in-time where a SUPERSEDED rule is active, it is returned (and not the
// later ACTIVE rule).
func TestFindRules_SupersededRuleReturnedForHistoricalQuery(t *testing.T) {
	// Arrange: two rules for the same jurisdiction and domain.
	// Rule1: SUPERSEDED, active 2024-01-01 to 2025-01-01
	// Rule2: ACTIVE, active 2025-01-01 onward (no end date)
	// Query effective_at: 2024-06-01 (should return Rule1 only)

	// Helper to create a JurisdictionRule with given fields.
	makeRule := func(id, status string, start, end time.Time, payload map[string]any) *domain.JurisdictionRule {
		pBytes, _ := json.Marshal(payload)
		return &domain.JurisdictionRule{
			JurisdictionRuleID: id,
			JurisdictionID:     "test-jurisdiction-id",
			RuleDomain:         "TAX",
			RuleCode:           "RATE",
			RuleName:           "Tax Rate",
			EffectiveFrom:      start,
			EffectiveTo: func(t time.Time) *time.Time {
				if t.IsZero() {
					return nil
				}
				return &t
			}(end),
			RulePayload:          json.RawMessage(pBytes),
			RuleStatus:           status,
			LegalDriftState:      "CURRENT",
			DataClassification:   "INTERNAL",
			CreatedAt:            time.Now().UTC(),
			CreatedByPrincipalID: "principal-test",
			SchemaVersion:        "1.0",
		}
	}

	// Define times
	start1 := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	end1 := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	start2 := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	// end2 is zero (meaning nil)

	ruleSuperseded := makeRule(
		"rule-superseded",
		"SUPERSEDED",
		start1,
		end1,
		map[string]any{"rate": 0.20},
	)
	ruleActive := makeRule(
		"rule-active",
		"ACTIVE",
		start2,
		time.Time{}, // zero time indicates no end date
		map[string]any{"rate": 0.25},
	)

	st := &stubStore{
		rules: []*domain.JurisdictionRule{ruleSuperseded, ruleActive},
	}

	h := newTestRouter(st)
	// Build the request with query parameters
	req := httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/test-jurisdiction-id/rules?domain=TAX&effective_at=2024-06-01T00:00:00Z", nil)
	req.Header.Set("X-Correlation-ID", "corr-test")

	rr := executeRequest(h, req)

	// Assert
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d - body: %s", rr.Code, rr.Body.String())
	}

	var rules []domain.JurisdictionRule
	if err := json.NewDecoder(rr.Body).Decode(&rules); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}

	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}

	got := rules[0]
	if got.JurisdictionRuleID != "rule-superseded" {
		t.Errorf("expected rule ID 'rule-superseded', got %s", got.JurisdictionRuleID)
	}
	if got.RuleStatus != "SUPERSEDED" {
		t.Errorf("expected rule status 'SUPERSEDED', got %s", got.RuleStatus)
	}
	// Additionally, ensure the effective dates are as expected
	if !got.EffectiveFrom.Equal(start1) {
		t.Errorf("expected effective_from %v, got %v", start1, got.EffectiveFrom)
	}
	if !(*got.EffectiveTo).Equal(end1) {
		t.Errorf("expected effective_to %v, got %v", end1, *got.EffectiveTo)
	}
}

// TestGetRules_400_InvalidEffectiveAt verifies a malformed point-in-time is a
// client error, and never silently treated as "now".
func TestGetRules_400_InvalidEffectiveAt(t *testing.T) {
	st := &stubStore{}
	rr := executeRequest(newTestRouter(st),
		httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/j-1/rules?effective_at=yesterday", nil))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "invalid_effective_at" {
		t.Errorf("expected error=invalid_effective_at, got %q", got)
	}
	if st.storeCalled {
		t.Error("store was queried despite a malformed effective_at")
	}
}

// ── GetRulePack tests ─────────────────────────────────────────────────────────

// TestGetRulePack_200 verifies the resolved runtime pack is returned with the
// chain it was assembled from. resolved_from is what lets a caller record the
// rule basis of a governed action (§8.2's evidence obligation).
func TestGetRulePack_200(t *testing.T) {
	pack := &domain.RulePack{
		JurisdictionID: "state-1",
		EffectiveAt:    time.Date(2025, time.June, 1, 0, 0, 0, 0, time.UTC),
		ResolvedFrom:   []string{"state-1", "country-1"},
		Rules: []*domain.JurisdictionRule{
			{JurisdictionRuleID: "r-1", RuleDomain: "TAX", RuleCode: "RATE", RuleStatus: "ACTIVE"},
		},
	}
	h := newTestRouter(&stubStore{rulePack: pack})
	rr := executeRequest(h, httptest.NewRequest(http.MethodGet,
		"/v1/jurisdictions/state-1/rule-pack?domain=TAX&effective_at=2025-06-01T00:00:00Z", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got domain.RulePack
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode rule pack: %v", err)
	}
	if len(got.ResolvedFrom) != 2 || got.ResolvedFrom[0] != "state-1" {
		t.Errorf("resolved_from = %v, want [state-1 country-1]", got.ResolvedFrom)
	}
	if len(got.Rules) != 1 {
		t.Errorf("expected 1 resolved rule, got %d", len(got.Rules))
	}
}

func TestGetRulePack_404_JurisdictionNotFound(t *testing.T) {
	h := newTestRouter(&stubStore{rulePackErr: domain.ErrJurisdictionNotFound})
	rr := executeRequest(h, httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/unknown/rule-pack", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestGetRulePack_400_InvalidEffectiveAt(t *testing.T) {
	st := &stubStore{}
	rr := executeRequest(newTestRouter(st),
		httptest.NewRequest(http.MethodGet, "/v1/jurisdictions/j-1/rule-pack?effective_at=nope", nil))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if st.storeCalled {
		t.Error("store was queried despite a malformed effective_at")
	}
}

// ── GetRule / GetDriftEvents tests ────────────────────────────────────────────

func TestGetRule_200(t *testing.T) {
	h := newTestRouter(&stubStore{ruleByID: &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "ACTIVE"}})
	rr := executeRequest(h, httptest.NewRequest(http.MethodGet, "/v1/rules/r-1", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got domain.JurisdictionRule
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode rule: %v", err)
	}
	if got.JurisdictionRuleID != "r-1" {
		t.Errorf("expected r-1, got %q", got.JurisdictionRuleID)
	}
}

func TestGetRule_404(t *testing.T) {
	h := newTestRouter(&stubStore{ruleByIDErr: domain.ErrRuleNotFound})
	rr := executeRequest(h, httptest.NewRequest(http.MethodGet, "/v1/rules/nope", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "rule_not_found" {
		t.Errorf("expected error=rule_not_found, got %q", got)
	}
}

func TestGetDriftEvents_200_History(t *testing.T) {
	reason := "HMRC published a revised threshold"
	h := newTestRouter(&stubStore{driftEvents: []*domain.DriftEvent{
		{DriftEventID: "d-2", FromState: "DRIFTED", ToState: "UNDER_REVIEW"},
		{DriftEventID: "d-1", FromState: "CURRENT", ToState: "DRIFTED", Reason: &reason},
	}})
	rr := executeRequest(h, httptest.NewRequest(http.MethodGet, "/v1/rules/r-1/drift-events", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	var got []domain.DriftEvent
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode drift events: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 drift events, got %d", len(got))
	}
	if got[0].DriftEventID != "d-2" {
		t.Errorf("expected newest-first ordering, got %q first", got[0].DriftEventID)
	}
}

// TestGetDriftEvents_200_EmptyArrayNotNull — a rule that has never drifted has
// an empty history, which is a different fact from "no such rule" (404).
func TestGetDriftEvents_200_EmptyArrayNotNull(t *testing.T) {
	h := newTestRouter(&stubStore{driftEvents: nil})
	rr := executeRequest(h, httptest.NewRequest(http.MethodGet, "/v1/rules/r-1/drift-events", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if body := rr.Body.String(); len(body) == 0 || body[0] != '[' {
		t.Errorf("expected JSON array, got: %s", body)
	}
}

func TestGetDriftEvents_404_RuleNotFound(t *testing.T) {
	h := newTestRouter(&stubStore{driftEventsErr: domain.ErrRuleNotFound})
	rr := executeRequest(h, httptest.NewRequest(http.MethodGet, "/v1/rules/nope/drift-events", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}
