package handler_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"zoiko.io/compliance-risk-scoring-svc/internal/authz"
	"zoiko.io/compliance-risk-scoring-svc/internal/service"
)

// TestReadsRequirePrincipal: every tenant-data read is 401 without
// X-Principal-Id, before authorization or storage is touched.
func TestReadsRequirePrincipal(t *testing.T) {
	policy := allowAll()
	c := newClient(t, routerWith(nil, policy, fakePinger{}))
	for _, path := range []string{
		"/v1/risk-scores",
		"/v1/risk-scores/" + uuid.NewString(),
		"/v1/risk-scores/subjects/LEGAL_ENTITY/" + c.entity + "/history",
		"/v1/risk-scores/thresholds",
		"/v1/risk-scores/thresholds/effective",
		"/v1/risk-scores/thresholds/" + uuid.NewString(),
	} {
		expectProblem(t, c.do(http.MethodGet, path, nil, drop("X-Principal-Id")), http.StatusUnauthorized, "missing_principal")
	}
	if len(policy.calls) != 0 {
		t.Fatalf("authz must not be asked without a principal: %v", policy.calls)
	}
}

// TestReadAuthorizationDenied: a principal without the READ action gets 403
// and an unreachable authorization-svc is 503 (fail closed). These reads are
// refused before the store is used, so no database is needed.
func TestReadAuthorizationDenied(t *testing.T) {
	policy := allowAll()
	policy.outcomes[service.ActionRiskScoreRead] = authz.ErrAuthorizationDenied
	policy.outcomes[service.ActionRiskThresholdRuleRead] = authz.ErrAuthzServiceUnavailable
	c := newClient(t, routerWith(nil, policy, fakePinger{}))

	expectProblem(t, c.do(http.MethodGet, "/v1/risk-scores", nil), http.StatusForbidden, "authorization_denied")
	expectProblem(t, c.do(http.MethodGet, "/v1/risk-scores/subjects/LEGAL_ENTITY/"+c.entity+"/history", nil),
		http.StatusForbidden, "authorization_denied")
	expectProblem(t, c.do(http.MethodGet, "/v1/risk-scores/thresholds", nil), http.StatusServiceUnavailable, "authorization_unavailable")
	expectProblem(t, c.do(http.MethodGet, "/v1/risk-scores/thresholds/effective", nil), http.StatusServiceUnavailable, "authorization_unavailable")
	expectProblem(t, c.do(http.MethodGet, "/v1/risk-scores/thresholds/"+uuid.NewString(), nil),
		http.StatusServiceUnavailable, "authorization_unavailable")
}

// TestReadAuthorizationScope: a list is authorized against its
// legal_entity_id filter, else X-Legal-Entity-Id, else the tenant.
func TestReadAuthorizationScope(t *testing.T) {
	policy := allowAll()
	policy.outcomes[service.ActionRiskScoreRead] = authz.ErrAuthorizationDenied // stop before the store
	c := newClient(t, routerWith(nil, policy, fakePinger{}))

	c.do(http.MethodGet, "/v1/risk-scores?legal_entity_id=LE-filter", nil)
	c.do(http.MethodGet, "/v1/risk-scores", nil)
	c.do(http.MethodGet, "/v1/risk-scores", nil, drop("X-Legal-Entity-Id"))

	want := []string{
		c.tenant + "|principal-1|LE-filter|" + service.ActionRiskScoreRead,
		c.tenant + "|principal-1|" + c.entity + "|" + service.ActionRiskScoreRead,
		c.tenant + "|principal-1|" + c.tenant + "|" + service.ActionRiskScoreRead,
	}
	if len(policy.calls) != len(want) {
		t.Fatalf("calls=%v", policy.calls)
	}
	for i := range want {
		if policy.calls[i] != want[i] {
			t.Fatalf("call %d: got %q want %q", i, policy.calls[i], want[i])
		}
	}
}

// TestGetScoreReadAuthorization (Postgres): GET by id is authorized against
// the stored score's legal entity; a principal denied RISK_SCORE_READ gets
// 403, not the score.
func TestGetScoreReadAuthorization(t *testing.T) {
	s, _ := testStore(t)
	policy := allowAll()
	c := newClient(t, routerWith(s, policy, s))

	created := expectStatus(t, c.do(http.MethodPost, "/v1/risk-scores/calculate",
		calcBody(c.entity, "read authz", counts(1, 0, 0, 0, 0))), http.StatusCreated)
	id := created["id"].(string)

	policy.deny["reader-denied|"+service.ActionRiskScoreRead] = true
	expectProblem(t, c.do(http.MethodGet, "/v1/risk-scores/"+id, nil, as("reader-denied"), drop("X-Legal-Entity-Id")),
		http.StatusForbidden, "authorization_denied")
	last := policy.calls[len(policy.calls)-1]
	if last != c.tenant+"|reader-denied|"+c.entity+"|"+service.ActionRiskScoreRead {
		t.Fatalf("GET must be authorized against the score's legal entity: %q", last)
	}
	expectStatus(t, c.do(http.MethodGet, "/v1/risk-scores/"+id, nil, as("reader-2")), http.StatusOK)
}
