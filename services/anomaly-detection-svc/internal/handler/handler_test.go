package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/anomaly-detection-svc/internal/authz"
	"zoiko.io/anomaly-detection-svc/internal/domain"
	"zoiko.io/anomaly-detection-svc/internal/handler"
	"zoiko.io/anomaly-detection-svc/internal/store"
)

// HTTP-layer tests through the real router (tenant gate, §4 envelope,
// routes). The pre-rewrite handler_test.go drove a mock store through the
// old handler.New(store, publisher, authz, logger); the rewrite removed the
// publisher (events go through the outbox) and the old store interface, so
// its intent — detect/update/get/list and create/list rules succeed — is
// ported here (allowed path) and against real Postgres in
// handler_pg_test.go.

// policyAuthz is a policy-driven authorization-svc stand-in: an action is
// allowed only when listed; anything else is denied, as authz.Client does.
type policyAuthz struct {
	mu          sync.Mutex
	allowed     map[string]bool
	unavailable bool
	calls       []string
}

func allow(actions ...string) *policyAuthz {
	p := &policyAuthz{allowed: map[string]bool{}}
	for _, a := range actions {
		p.allowed[a] = true
	}
	return p
}

func (p *policyAuthz) CheckAllowed(_ context.Context, principalID, scopeID, action string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, principalID+"|"+scopeID+"|"+action)
	if p.unavailable {
		return authz.ErrAuthzServiceUnavailable
	}
	if !p.allowed[action] {
		return authz.ErrAuthorizationDenied
	}
	return nil
}

// fakeStore implements the few store methods these tests reach. Any other
// method panics (nil embedded interface), which fails the test loudly if a
// refused request ever reaches the store.
type fakeStore struct {
	store.Store
	mu       sync.Mutex
	rules    map[string]*domain.AnomalyRule
	lastCmd  domain.CommandContext
	creates  int
	pingFail bool
}

func newFakeStore() *fakeStore { return &fakeStore{rules: map[string]*domain.AnomalyRule{}} }

func (f *fakeStore) Ping(context.Context) error {
	if f.pingFail {
		return errors.New("db down")
	}
	return nil
}

func (f *fakeStore) CreateRule(_ context.Context, cmd domain.CommandContext, req domain.CreateRuleRequest) (domain.CommandResult[domain.AnomalyRule], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	f.lastCmd = cmd
	rule := &domain.AnomalyRule{RuleID: "arule-00000000-0000-0000-0000-00000000000" + string(rune('0'+f.creates)), TenantID: cmd.TenantID,
		RuleName: req.RuleName, DomainName: req.DomainName, MetricType: req.MetricType, ZScoreCutoff: req.ZScoreCutoff,
		IsActive: true, Version: 1, CreatedBy: cmd.PrincipalID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.rules[rule.TenantID+"/"+rule.RuleID] = rule
	return domain.CommandResult[domain.AnomalyRule]{Resource: rule}, nil
}

func (f *fakeStore) GetRule(_ context.Context, tenantID, ruleID string) (*domain.AnomalyRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rules[tenantID+"/"+ruleID]; ok {
		return r, nil
	}
	return nil, domain.ErrAnomalyRuleNotFound
}

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

func newRouter(st store.Store, az handler.Authorizer, ready bool) http.Handler {
	h := handler.New(st, az, zap.NewNop())
	if !ready {
		return handler.NewRouter(h, nil)
	}
	return handler.NewRouter(h, st)
}

type reqOpt func(*http.Request)

func noHeader(name string) reqOpt { return func(r *http.Request) { r.Header.Del(name) } }
func header(name, value string) reqOpt {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

var idemSeq int

func do(t *testing.T, router http.Handler, method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, reader)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", tenantA)
	r.Header.Set("X-Principal-Id", "principal-01")
	r.Header.Set("X-Legal-Entity-Id", "entity-001")
	r.Header.Set("X-Request-Id", "req-1")
	r.Header.Set("X-Correlation-ID", "corr-1")
	r.Header.Set("X-Source-Channel", "api")
	if method != http.MethodGet {
		idemSeq++
		r.Header.Set("Idempotency-Key", fmt.Sprintf("idem-%d", idemSeq))
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

const validRule = `{"rule_name":"High Payroll Variance","domain_name":"WORKFORCE","metric_type":"gross_pay_variance","threshold_value":25,"z_score_cutoff":3.5}`

func TestProbes_PublicAndReflectDB(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, allow(), true)

	for _, path := range []string{"/healthz", "/readyz"} {
		r := httptest.NewRequest(http.MethodGet, path, nil) // no identity headers at all
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s without headers: %d %s", path, w.Code, w.Body.String())
		}
	}
	var body map[string]string
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "ok" {
		t.Fatalf("healthz body: %s", w.Body.String())
	}

	st.pingFail = true
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatalf("readyz with DB down: %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	newRouter(st, allow(), false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz without pool: %d", w.Code)
	}
}

func TestMissingIdentity_FailsClosed(t *testing.T) {
	router := newRouter(newFakeStore(), allow(handler.ActionAnomalyRead, handler.ActionRuleCreate), true)

	if w := do(t, router, http.MethodGet, "/v1/anomalies/rules", "", noHeader("X-Tenant-Id")); w.Code != http.StatusUnauthorized {
		t.Fatalf("GET without tenant: %d", w.Code)
	}
	if w := do(t, router, http.MethodGet, "/v1/anomalies/rules", "", noHeader("X-Principal-Id")); w.Code != http.StatusUnauthorized {
		t.Fatalf("GET without principal: %d", w.Code)
	}
	if w := do(t, router, http.MethodPost, "/v1/anomalies/rules", validRule, noHeader("X-Principal-Id")); w.Code != http.StatusUnauthorized {
		t.Fatalf("POST without principal: %d", w.Code)
	}
	if w := do(t, router, http.MethodPost, "/v1/anomalies/rules", validRule, noHeader("Idempotency-Key")); w.Code != http.StatusBadRequest {
		t.Fatalf("POST without Idempotency-Key: %d", w.Code)
	}
	if w := do(t, router, http.MethodPost, "/v1/anomalies/rules", validRule, noHeader("X-Legal-Entity-Id")); w.Code != http.StatusBadRequest {
		t.Fatalf("POST without legal entity: %d", w.Code)
	}
}

func TestCreateRule_AuthzAllowedAndDenied(t *testing.T) {
	st := newFakeStore()
	denied := allow(handler.ActionAnomalyRead) // RULE_CREATE not granted
	w := do(t, newRouter(st, denied, true), http.MethodPost, "/v1/anomalies/rules", validRule)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "permission_denied") {
		t.Fatalf("denied create: %d %s", w.Code, w.Body.String())
	}
	if st.creates != 0 {
		t.Fatalf("a denied request must not reach the store")
	}
	if len(denied.calls) != 1 || denied.calls[0] != "principal-01|entity-001|"+handler.ActionRuleCreate {
		t.Fatalf("authz called with %v", denied.calls)
	}

	allowed := allow(handler.ActionRuleCreate, handler.ActionAnomalyRead)
	router := newRouter(st, allowed, true)
	w = do(t, router, http.MethodPost, "/v1/anomalies/rules", validRule, header("Idempotency-Key", "k-1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("allowed create: %d %s", w.Code, w.Body.String())
	}
	var rule domain.AnomalyRule
	_ = json.Unmarshal(w.Body.Bytes(), &rule)
	if rule.ZScoreCutoff != 3.5 || w.Header().Get("ETag") != `"1"` || w.Header().Get("Location") != "/v1/anomalies/rules/"+rule.RuleID {
		t.Fatalf("create response: %+v headers=%v", rule, w.Header())
	}
	if st.lastCmd.TenantID != tenantA || st.lastCmd.PrincipalID != "principal-01" ||
		st.lastCmd.Idempotency.Key != "k-1" || st.lastCmd.Idempotency.RouteTemplate != "POST /v1/anomalies/rules" {
		t.Fatalf("command context: %+v", st.lastCmd)
	}

	// Read back; another tenant gets 404.
	if w := do(t, router, http.MethodGet, "/v1/anomalies/rules/"+rule.RuleID, ""); w.Code != http.StatusOK {
		t.Fatalf("get rule: %d", w.Code)
	}
	if w := do(t, router, http.MethodGet, "/v1/anomalies/rules/"+rule.RuleID, "", header("X-Tenant-Id", tenantB)); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get rule: %d", w.Code)
	}
}

func TestValidationAndRequestErrors(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, allow(handler.ActionRuleCreate, handler.ActionAnomalyRead, handler.ActionAnomalyAck,
		handler.ActionAnomalyDisposition, handler.ActionGovernedRead), true)

	cases := []struct {
		name, method, path, body string
		want                     int
		opts                     []reqOpt
	}{
		{"missing rule_name", http.MethodPost, "/v1/anomalies/rules", `{"domain_name":"D","metric_type":"m"}`, 422, nil},
		{"malformed json", http.MethodPost, "/v1/anomalies/rules", `{not json`, 400, nil},
		{"unknown field", http.MethodPost, "/v1/anomalies/rules", `{"rule_name":"a","domain_name":"d","metric_type":"m","bogus":1}`, 400, nil},
		{"body tenant mismatch", http.MethodPost, "/v1/anomalies/rules", `{"tenant_id":"` + tenantB + `","rule_name":"a","domain_name":"d","metric_type":"m"}`, 403, nil},
		{"bad id", http.MethodGet, "/v1/anomalies/rules/not-an-id", "", 400, nil},
		{"unknown rule", http.MethodGet, "/v1/anomalies/rules/arule-00000000-0000-0000-0000-0000000000ff", "", 404, nil},
		{"bad limit", http.MethodGet, "/v1/anomalies/rules?limit=0", "", 400, nil},
		{"bad severity filter", http.MethodGet, "/v1/anomaly-governance/signals?severity=HUGE", "", 400, nil},
		{"acknowledge without If-Match", http.MethodPost, "/v1/anomaly-governance/signals/ans_00000000-0000-0000-0000-000000000001:acknowledge", `{}`, 428, nil},
		{"close with protected action", http.MethodPost, "/v1/anomaly-governance/signals/ans_00000000-0000-0000-0000-000000000001:close",
			`{"outcome":"Confirmed","reason_code":"CONFIRMED_EXCEPTION","notes":"n","requested_action":"FREEZE_PAYMENT"}`, 422, []reqOpt{header("If-Match", `"1"`)}},
		{"escalate without reason", http.MethodPost, "/v1/anomaly-governance/signals/ans_00000000-0000-0000-0000-000000000001:escalate",
			`{"reason":""}`, 422, []reqOpt{header("If-Match", `"1"`)}},
		{"run without signals", http.MethodPost, "/v1/anomaly-governance/runs",
			`{"domain_name":"d","model_provider":"p","model_version":"v","feature_set_version":"f","observed_drift_bp":1,"signals":[]}`, 422, nil},
		{"unknown route", http.MethodGet, "/v1/nope", "", 404, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, router, tc.method, tc.path, tc.body, tc.opts...)
			if w.Code != tc.want {
				t.Fatalf("%s %s: want %d got %d %s", tc.method, tc.path, tc.want, w.Code, w.Body.String())
			}
		})
	}
	if st.creates != 0 {
		t.Fatalf("invalid requests must not reach the store (creates=%d)", st.creates)
	}
}

func TestAuthzUnavailable_FailsClosed503(t *testing.T) {
	az := allow(handler.ActionAnomalyRead)
	az.unavailable = true
	w := do(t, newRouter(newFakeStore(), az, true), http.MethodGet, "/v1/anomalies/rules", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("authz unavailable: %d", w.Code)
	}
}
