package handler_test

// HTTP-level tests for decision-support-svc.
//
// These replace the tests written against the pre-rewrite handler
// (handler.New(store, publisher, authz, history, logger)). The handler now
// depends only on handler.RecommendationService, so each test drives the
// real service.Service over the in-memory store from internal/testsupport,
// through the same middleware chain main.go builds (tenant context +
// envelope enforcement). The old tests' assertions are kept:
//
//   - missing principal -> 401
//   - decision-log outage degrades to NO_HISTORY / confidence 0 (201, not 503)
//   - no prior decisions -> NO_HISTORY
//   - majority GRANTED -> APPROVE, confidence 0.75, 4 sampled, one
//     "recommendation proposed" event (now an outbox row, not a direct publish)
//   - majority DENIED -> REJECT
//   - a retried create resolves to the same recommendation_id (idempotency
//     moved from correlation_id to the Idempotency-Key header)
//
// plus the contract checks: probes reachable without identity headers,
// authz allow and deny paths, cross-tenant 404, validation 400, decision
// lifecycle with If-Match.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/decision-support-svc/internal/authz"
	"zoiko.io/decision-support-svc/internal/domain"
	svcenvelope "zoiko.io/decision-support-svc/internal/envelope"
	"zoiko.io/decision-support-svc/internal/events"
	"zoiko.io/decision-support-svc/internal/handler"
	"zoiko.io/decision-support-svc/internal/health"
	svcmiddleware "zoiko.io/decision-support-svc/internal/middleware"
	"zoiko.io/decision-support-svc/internal/service"
	"zoiko.io/decision-support-svc/internal/testsupport"
)

var (
	tenantA     = uuid.NewString()
	tenantB     = uuid.NewString()
	legalEntity = uuid.NewString()
)

const (
	requester = "principal-requester"
	reviewer  = "principal-reviewer"
	outsider  = "principal-without-grants"
)

// policyAuthorizer is a policy-driven authz stub: a request is GRANTED only
// when (principal, action) is in the grant set, DENIED otherwise. Down
// simulates an authorization-svc outage.
type policyAuthorizer struct {
	mu     sync.Mutex
	grants map[string]bool
	Down   bool
	seen   []authz.Request
}

func newPolicy() *policyAuthorizer {
	p := &policyAuthorizer{grants: map[string]bool{}}
	for _, principal := range []string{requester, reviewer} {
		for _, action := range []string{service.ActionRequest, service.ActionView, service.ActionDecide} {
			p.grants[principal+"|"+action] = true
		}
	}
	return p
}

func (p *policyAuthorizer) CheckAllowed(_ context.Context, req authz.Request) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, req)
	if p.Down {
		return domain.ErrAuthzServiceUnavailable
	}
	if p.grants[req.PrincipalID+"|"+req.ActionType] {
		return nil
	}
	return domain.ErrAuthorizationDenied
}

type fixture struct {
	store   *testsupport.MemStore
	authz   *policyAuthorizer
	history *testsupport.History
	router  chi.Router
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		store:   testsupport.NewMemStore(),
		authz:   newPolicy(),
		history: &testsupport.History{},
	}
	svc := service.New(f.store, f.authz, f.history, zap.NewNop())

	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeWriteStrict, nil))
	handler.RegisterRoutes(r, handler.New(svc, zap.NewNop()))
	probes := health.NewWithPinger(nil, zap.NewNop())
	r.Get("/healthz", probes.Liveness)
	r.Get("/readyz", probes.Readiness)
	f.router = r
	return f
}

type reqOpts struct {
	tenant    string
	principal string
	idemKey   string
	ifMatch   string
	noIdem    bool
	raw       []byte
}

func (f *fixture) do(t *testing.T, method, path string, body any, o reqOpts) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	switch {
	case o.raw != nil:
		buf.Write(o.raw)
	case body != nil:
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Correlation-ID", "corr-"+uuid.NewString())
	req.Header.Set("X-Source-Channel", "api")
	req.Header.Set("X-Legal-Entity-Id", legalEntity)
	if o.tenant != "-" {
		tenant := o.tenant
		if tenant == "" {
			tenant = tenantA
		}
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if o.principal != "-" {
		principal := o.principal
		if principal == "" {
			principal = requester
		}
		req.Header.Set("X-Principal-Id", principal)
	}
	if method != http.MethodGet && !o.noIdem {
		key := o.idemKey
		if key == "" {
			key = uuid.NewString()
		}
		req.Header.Set("Idempotency-Key", key)
	}
	if o.ifMatch != "" {
		req.Header.Set("If-Match", o.ifMatch)
	}
	rr := httptest.NewRecorder()
	f.router.ServeHTTP(rr, req)
	return rr
}

func createBody() map[string]any {
	return map[string]any{
		"legal_entity_id":   legalEntity,
		"subject_type":      "purchase_order",
		"subject_reference": "po-123",
		"action_type":       "PO_ISSUE",
	}
}

func decodeRec(t *testing.T, rr *httptest.ResponseRecorder) domain.Recommendation {
	t.Helper()
	var rec domain.Recommendation
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &rec), rr.Body.String())
	return rec
}

func problemCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	assert.Equal(t, "application/problem+json", rr.Header().Get("Content-Type"))
	var p handler.Problem
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &p), rr.Body.String())
	assert.Equal(t, rr.Code, p.Status)
	return p.Code
}

func (f *fixture) create(t *testing.T) domain.Recommendation {
	t.Helper()
	rr := f.do(t, http.MethodPost, "/v1/recommendations/", createBody(), reqOpts{})
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	return decodeRec(t, rr)
}

// ── probes ───────────────────────────────────────────────────────────────────

func TestProbes_ReachableWithoutIdentityHeaders(t *testing.T) {
	f := newFixture(t)

	rr := f.do(t, http.MethodGet, "/healthz", nil, reqOpts{tenant: "-", principal: "-"})
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rr.Body.String())

	// No database behind this fixture: readiness must say so, not 200.
	rr = f.do(t, http.MethodGet, "/readyz", nil, reqOpts{tenant: "-", principal: "-"})
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Contains(t, rr.Body.String(), `"status":"unavailable"`)
}

// ── create (ported from the pre-rewrite tests) ───────────────────────────────

func TestCreate_MissingPrincipal_401(t *testing.T) {
	f := newFixture(t)
	rr := f.do(t, http.MethodPost, "/v1/recommendations/", createBody(), reqOpts{principal: "-"})
	require.Equal(t, http.StatusUnauthorized, rr.Code, rr.Body.String())
}

func TestCreate_MissingTenant_FailsClosed(t *testing.T) {
	f := newFixture(t)
	rr := f.do(t, http.MethodPost, "/v1/recommendations/", createBody(), reqOpts{tenant: "-"})
	require.Contains(t, []int{http.StatusBadRequest, http.StatusUnauthorized}, rr.Code, rr.Body.String())
	assert.Empty(t, f.store.Outbox, "nothing is written without a tenant")
}

func TestCreate_HistoryUnavailable_DegradesGracefully(t *testing.T) {
	f := newFixture(t)
	f.history.Err = domain.ErrAuthzServiceUnavailable

	rec := f.create(t)
	assert.Equal(t, domain.RecommendedActionNoHistory, rec.RecommendedAction)
	assert.Equal(t, 0.0, rec.ConfidenceScore)
	assert.Equal(t, domain.HistoryUnavailable, rec.HistoryStatus)
}

func TestCreate_NoPriorDecisions(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t)
	assert.Equal(t, domain.RecommendedActionNoHistory, rec.RecommendedAction)
}

func TestCreate_MajorityGranted_RecommendsApprove(t *testing.T) {
	f := newFixture(t)
	f.history.Decisions = testsupport.Outcomes("GRANTED", "GRANTED", "GRANTED", "DENIED")

	rr := f.do(t, http.MethodPost, "/v1/recommendations/", createBody(), reqOpts{})
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	rec := decodeRec(t, rr)

	assert.Equal(t, domain.RecommendedActionApprove, rec.RecommendedAction)
	assert.Equal(t, 0.75, rec.ConfidenceScore)
	assert.Equal(t, 4, rec.PriorDecisionsSampled)
	assert.Equal(t, domain.StatusReviewRequired, rec.Status)
	assert.Equal(t, tenantA, rec.TenantID)
	assert.Equal(t, `"1"`, rr.Header().Get("ETag"))
	assert.Len(t, f.store.OutboxOfType(events.EventRecommendationProposed), 1,
		"one decision_support.recommendation.proposed event, written to the outbox in the same transaction")
}

func TestCreate_MajorityDenied_RecommendsReject(t *testing.T) {
	f := newFixture(t)
	f.history.Decisions = testsupport.Outcomes("DENIED", "DENIED", "GRANTED")
	rec := f.create(t)
	assert.Equal(t, domain.RecommendedActionReject, rec.RecommendedAction)
}

func TestCreate_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	f.history.Decisions = testsupport.Outcomes("GRANTED")
	key := uuid.NewString()

	rr1 := f.do(t, http.MethodPost, "/v1/recommendations/", createBody(), reqOpts{idemKey: key})
	require.Equal(t, http.StatusCreated, rr1.Code, rr1.Body.String())
	rr2 := f.do(t, http.MethodPost, "/v1/recommendations/", createBody(), reqOpts{idemKey: key})
	require.Equal(t, http.StatusCreated, rr2.Code, rr2.Body.String())

	rec1, rec2 := decodeRec(t, rr1), decodeRec(t, rr2)
	assert.Equal(t, rec1.RecommendationID, rec2.RecommendationID,
		"a retried request resolves to the original recommendation")
	assert.Equal(t, "true", rr2.Header().Get("Idempotent-Replayed"))
	assert.Len(t, f.store.OutboxOfType(events.EventRecommendationProposed), 1, "the replay writes nothing")

	changed := createBody()
	changed["subject_reference"] = "po-999"
	rr3 := f.do(t, http.MethodPost, "/v1/recommendations/", changed, reqOpts{idemKey: key})
	require.Equal(t, http.StatusUnprocessableEntity, rr3.Code, rr3.Body.String())
	assert.Equal(t, "idempotency_key_reused", problemCode(t, rr3))
}

func TestCreate_MissingIdempotencyKey_400(t *testing.T) {
	f := newFixture(t)
	rr := f.do(t, http.MethodPost, "/v1/recommendations/", createBody(), reqOpts{noIdem: true})
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}

func TestCreate_Validation_400(t *testing.T) {
	f := newFixture(t)

	rr := f.do(t, http.MethodPost, "/v1/recommendations/", nil, reqOpts{raw: []byte(`{not json`)})
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Equal(t, "validation_failed", problemCode(t, rr))

	body := createBody()
	delete(body, "action_type")
	rr = f.do(t, http.MethodPost, "/v1/recommendations/", body, reqOpts{})
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())

	body = createBody()
	body["oversight_class"] = "O4"
	rr = f.do(t, http.MethodPost, "/v1/recommendations/", body, reqOpts{})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Equal(t, "oversight_prohibited", problemCode(t, rr))
}

// ── authorization ────────────────────────────────────────────────────────────

func TestAuthz_AllowedAndDenied(t *testing.T) {
	f := newFixture(t)

	allowed := f.do(t, http.MethodPost, "/v1/recommendations/", createBody(), reqOpts{principal: requester})
	require.Equal(t, http.StatusCreated, allowed.Code, allowed.Body.String())

	denied := f.do(t, http.MethodPost, "/v1/recommendations/", createBody(), reqOpts{principal: outsider})
	require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	assert.Equal(t, "forbidden", problemCode(t, denied))
	assert.Len(t, f.store.OutboxOfType(events.EventRecommendationProposed), 1, "a denied request writes nothing")

	rec := decodeRec(t, allowed)
	deniedRead := f.do(t, http.MethodGet, "/v1/recommendations/"+rec.RecommendationID, nil, reqOpts{principal: outsider})
	require.Equal(t, http.StatusForbidden, deniedRead.Code, deniedRead.Body.String())

	// The policy saw the caller's real tenant, principal and legal entity.
	last := f.authz.seen[len(f.authz.seen)-1]
	assert.Equal(t, tenantA, last.TenantID)
	assert.Equal(t, outsider, last.PrincipalID)
	assert.Equal(t, legalEntity, last.LegalEntityID)
	assert.Equal(t, service.ActionView, last.ActionType)
}

func TestAuthz_ServiceDownFailsClosed_503(t *testing.T) {
	f := newFixture(t)
	f.authz.Down = true
	rr := f.do(t, http.MethodPost, "/v1/recommendations/", createBody(), reqOpts{})
	require.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	assert.Empty(t, f.store.Outbox)
}

// ── reads ────────────────────────────────────────────────────────────────────

func TestGet_CrossTenantIsNotFound(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t)

	rr := f.do(t, http.MethodGet, "/v1/recommendations/"+rec.RecommendationID, nil, reqOpts{})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, rec.RecommendationID, decodeRec(t, rr).RecommendationID)

	rr = f.do(t, http.MethodGet, "/v1/recommendations/"+rec.RecommendationID, nil, reqOpts{tenant: tenantB})
	require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())

	rr = f.do(t, http.MethodGet, "/v1/recommendations/"+uuid.NewString(), nil, reqOpts{})
	require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())

	rr = f.do(t, http.MethodGet, "/v1/recommendations/not-a-uuid", nil, reqOpts{})
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}

func TestList_TenantScoped(t *testing.T) {
	f := newFixture(t)
	f.create(t)
	f.create(t)

	rr := f.do(t, http.MethodGet, "/v1/recommendations/?legal_entity_id="+legalEntity, nil, reqOpts{})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var page struct {
		Items []domain.Recommendation `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &page))
	assert.Len(t, page.Items, 2)

	rr = f.do(t, http.MethodGet, "/v1/recommendations/?legal_entity_id="+legalEntity, nil, reqOpts{tenant: tenantB})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	page.Items = nil
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &page))
	assert.Empty(t, page.Items)
	assert.Contains(t, rr.Body.String(), `"items":[]`, "an empty page is [], not null")

	rr = f.do(t, http.MethodGet, "/v1/recommendations/?status=BOGUS", nil, reqOpts{})
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}

// ── decisions ────────────────────────────────────────────────────────────────

func TestAccept_FullLifecycle(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t)
	acceptPath := "/v1/recommendations/" + rec.RecommendationID + "/accept"
	body := map[string]any{"reason": "matches precedent"}

	rr := f.do(t, http.MethodPost, acceptPath, body, reqOpts{principal: reviewer})
	require.Equal(t, http.StatusPreconditionRequired, rr.Code, "If-Match is required: %s", rr.Body.String())

	rr = f.do(t, http.MethodPost, acceptPath, body, reqOpts{principal: reviewer, ifMatch: `"7"`})
	require.Equal(t, http.StatusPreconditionFailed, rr.Code, rr.Body.String())

	rr = f.do(t, http.MethodPost, acceptPath, body, reqOpts{principal: requester, ifMatch: rec.ETag()})
	require.Equal(t, http.StatusForbidden, rr.Code, "O2 requester cannot accept their own recommendation: %s", rr.Body.String())
	assert.Equal(t, "segregation_of_duties", problemCode(t, rr))

	rr = f.do(t, http.MethodPost, acceptPath, body, reqOpts{principal: reviewer, ifMatch: rec.ETag(), tenant: tenantB})
	require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())

	key := uuid.NewString()
	rr = f.do(t, http.MethodPost, acceptPath, body, reqOpts{principal: reviewer, ifMatch: rec.ETag(), idemKey: key})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	accepted := decodeRec(t, rr)
	assert.Equal(t, domain.StatusAccepted, accepted.Status)
	assert.Equal(t, 2, accepted.Version)
	assert.False(t, accepted.DownstreamActionExecuted, "accepting advice never executes the action")
	assert.Len(t, f.store.OutboxOfType(events.EventRecommendationAccepted), 1)

	rr = f.do(t, http.MethodPost, acceptPath, body, reqOpts{principal: reviewer, ifMatch: rec.ETag(), idemKey: key})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "true", rr.Header().Get("Idempotent-Replayed"))
	assert.Len(t, f.store.OutboxOfType(events.EventRecommendationAccepted), 1, "the replay writes nothing")

	rr = f.do(t, http.MethodPost, "/v1/recommendations/"+rec.RecommendationID+"/reject",
		map[string]any{"reason": "changed my mind"}, reqOpts{principal: reviewer, ifMatch: accepted.ETag()})
	require.Equal(t, http.StatusConflict, rr.Code, "ACCEPTED is terminal: %s", rr.Body.String())

	rr = f.do(t, http.MethodGet, "/v1/recommendations/"+rec.RecommendationID+"/dispositions", nil, reqOpts{principal: reviewer})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var history struct {
		Items []domain.Disposition `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &history))
	require.Len(t, history.Items, 2)
	assert.Equal(t, domain.StatusReviewRequired, history.Items[0].ToStatus)
	assert.Equal(t, domain.StatusAccepted, history.Items[1].ToStatus)
}

func TestDecide_DeniedByPolicy_403(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t)
	f.authz.mu.Lock()
	f.authz.grants[outsider+"|"+service.ActionView] = true // may read, may not decide
	f.authz.mu.Unlock()

	rr := f.do(t, http.MethodPost, "/v1/recommendations/"+rec.RecommendationID+"/reject",
		map[string]any{"reason": "no"}, reqOpts{principal: outsider, ifMatch: rec.ETag()})
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	assert.Empty(t, f.store.OutboxOfType(events.EventRecommendationRejected))
}

func TestReject_RequiresReason_400(t *testing.T) {
	f := newFixture(t)
	rec := f.create(t)
	rr := f.do(t, http.MethodPost, "/v1/recommendations/"+rec.RecommendationID+"/reject",
		map[string]any{}, reqOpts{principal: reviewer, ifMatch: rec.ETag()})
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}

func TestStoreOutage_Is503WithoutDetail(t *testing.T) {
	f := newFixture(t)
	f.store.FailWith = errors.New("connection refused: secret-host:5432")
	rr := f.do(t, http.MethodGet, "/v1/recommendations/"+uuid.NewString(), nil, reqOpts{})
	require.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "secret-host", "infrastructure detail is never echoed")
}
