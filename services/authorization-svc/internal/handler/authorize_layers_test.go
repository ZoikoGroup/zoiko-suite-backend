package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

const testTenant = "00000000-0000-0000-0000-0000000000a1"

func authorize(t *testing.T, r chi.Router, body string, headers map[string]string) (int, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got
}

// ── the own-object SoD tenant scope ──────────────────────────────────────────

// TestAuthorize_OwnObjectSoD_UsesTheResolvedTenantScope is the regression test
// for a bug found while fixing the delegation layer.
//
// Every layer of /v1/authorize took its tenant from resolveTenantScope, which
// prefers the verified X-Tenant-Id header — except the own-object SoD read,
// which passed the raw BODY tenant_id. CheckOwnObjectSoD's predicate is
// `tenant_id IS NULL OR tenant_id = NULLIF($2, the empty string)`, so an empty tenant narrows
// it to platform-wide rules only. A caller doing exactly what
// resolveTenantScope exists to encourage — forwarding the header and leaving
// tenant_id out of the body — therefore had its tenant's own-object rules
// silently skipped.
//
// The same class of bug resolveTenantScope was written to fix, one layer down,
// and it punished the best-behaved callers.
func TestAuthorize_OwnObjectSoD_UsesTheResolvedTenantScope(t *testing.T) {
	store := &stubStore{
		rbacActions: []string{"AP_INVOICE_APPROVE"},
		rbacBasis:   "rbac:role=AP_APPROVER",
	}
	r := newTestRouter(store)

	// Header only — no tenant_id in the body, which is the convention.
	code, _ := authorize(t,
		r,
		`{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"AP_INVOICE_APPROVE","resource_owner_principal_id":"p-1"}`,
		map[string]string{"X-Tenant-Id": testTenant},
	)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if store.ownObjectTenantArg != testTenant {
		t.Fatalf("own-object SoD check ran with tenant %q, want the verified header tenant %q — "+
			"this tenant's own-object rules would never have fired", store.ownObjectTenantArg, testTenant)
	}
}

// ── platform scope (tracker item 67) ─────────────────────────────────────────

// TestAuthorize_PlatformScopeSentinelResolvesToTheConfiguredEntity: services
// with a platform-wide act had nowhere to put a scope, so each invented its own
// synthetic legal_entity_id and a grant seeded against one was invisible to a
// check made against another — silently, and fail-closed, so it read as "no
// grant" rather than as a mismatch.
func TestAuthorize_PlatformScopeSentinelResolvesToTheConfiguredEntity(t *testing.T) {
	store := &stubStore{
		rbacActions: []string{"KILL_SWITCH_ENGAGE"},
		rbacBasis:   "rbac:role=PLATFORM_OPERATOR",
	}
	r := newTestRouter(store) // configured with platformScopeEntityID "platform-scope-entity"

	code, got := authorize(t, r,
		`{"principal_id":"p-1","legal_entity_id":"PLATFORM","action_type":"KILL_SWITCH_ENGAGE"}`,
		map[string]string{"X-Tenant-Id": testTenant})

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if got["decision_outcome"] != "GRANTED" {
		t.Fatalf("outcome = %s, want GRANTED", got["decision_outcome"])
	}
	// The DECISION ARTIFACT has to name the scope actually evaluated, not the
	// sentinel: legal_entity_id is UUID NOT NULL in access_decision_log, so
	// "PLATFORM" would fail the insert outright, and a later audit of a
	// platform-wide act needs the real scope.
	if store.recordedParams.LegalEntityID != "platform-scope-entity" {
		t.Fatalf("decision recorded against %q, want the configured platform-scope entity",
			store.recordedParams.LegalEntityID)
	}
}

// TestAuthorize_PlatformScopeRefusedWhenUnconfigured — fail closed, and in the
// same direction requirePlatformAction already fails: a deployment that has
// not provisioned the platform-scope entity cannot have platform-wide
// decisions made for it, rather than having one invented.
func TestAuthorize_PlatformScopeRefusedWhenUnconfigured(t *testing.T) {
	store := &stubStore{rbacActions: []string{"KILL_SWITCH_ENGAGE"}}
	r := chi.NewRouter()
	// Empty platform-scope entity id — the default.
	h := handler.New(store, &stubPublisher{}, &stubValidator{},
		siem.New("", "authorization-svc", zap.NewNop()), "", zap.NewNop())
	handler.RegisterRoutes(r, h)

	code, got := authorize(t, r,
		`{"principal_id":"p-1","legal_entity_id":"PLATFORM","action_type":"KILL_SWITCH_ENGAGE"}`,
		map[string]string{"X-Tenant-Id": testTenant})

	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", code)
	}
	if got["error"] != "platform_scope_not_configured" {
		t.Fatalf("error = %q, want platform_scope_not_configured", got["error"])
	}
}

// TestAuthorize_OmittedEntityIsStillABadRequest — the sentinel is explicit for
// a reason. An omitted legal_entity_id is far more often a caller bug than a
// platform-scope request, and promoting it to platform scope would evaluate
// that bug against the wrong scope instead of reporting it.
func TestAuthorize_OmittedEntityIsStillABadRequest(t *testing.T) {
	r := newTestRouter(&stubStore{})
	code, got := authorize(t, r,
		`{"principal_id":"p-1","action_type":"KILL_SWITCH_ENGAGE"}`,
		map[string]string{"X-Tenant-Id": testTenant})

	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an omitted legal_entity_id, got %d", code)
	}
	if got["field"] != "legal_entity_id" {
		t.Fatalf("field = %q, want legal_entity_id", got["field"])
	}
}

// ── ABAC, layer 5 ────────────────────────────────────────────────────────────

func abacRule(code, effect, key, op, value string) domain.ABACRule {
	r := domain.ABACRule{
		RuleCode: code, ActionType: "PAYMENT_APPROVE", Effect: effect,
		AttributeKey: key, Operator: op, ActiveFlag: true,
	}
	if value != "" {
		r.AttributeValue = &value
	}
	return r
}

// TestAuthorize_ABACWithNoRulesChangesNothing is the property that makes
// shipping layer 5 with an empty abac_rules table safe.
func TestAuthorize_ABACWithNoRulesChangesNothing(t *testing.T) {
	store := &stubStore{
		rbacActions: []string{"PAYMENT_APPROVE"},
		rbacBasis:   "rbac:role=FINANCE_APPROVER",
		abacRules:   nil,
	}
	r := newTestRouter(store)

	code, got := authorize(t, r,
		`{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE","attributes":{"amount":"999999"}}`,
		map[string]string{"X-Tenant-Id": testTenant})

	if code != http.StatusOK || got["decision_outcome"] != "GRANTED" {
		t.Fatalf("code=%d outcome=%s, want 200/GRANTED", code, got["decision_outcome"])
	}
	if got["decision_basis"] != "rbac:role=FINANCE_APPROVER" {
		t.Fatalf("basis = %q, want the RBAC basis untouched", got["decision_basis"])
	}
}

func TestAuthorize_ABACDenies(t *testing.T) {
	tests := []struct {
		name      string
		rules     []domain.ABACRule
		body      string
		wantBasis string
	}{
		{
			name:      "FORBID matched",
			rules:     []domain.ABACRule{abacRule("NO_LARGE_SELF_SERVICE", domain.EffectForbid, "channel", "eq", "SELF_SERVICE")},
			body:      `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE","attributes":{"channel":"SELF_SERVICE"}}`,
			wantBasis: "abac:forbidden=NO_LARGE_SELF_SERVICE",
		},
		{
			name:      "REQUIRE unsatisfied",
			rules:     []domain.ABACRule{abacRule("DUAL_APPROVAL", domain.EffectRequire, "dual_approved", "eq", "true")},
			body:      `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE","attributes":{"dual_approved":"false"}}`,
			wantBasis: "abac:require_failed=DUAL_APPROVAL",
		},
		{
			// The bypass this closes: a REQUIRE rule must not be evadable by
			// simply omitting the attribute it requires.
			name:      "REQUIRE with no attributes sent at all",
			rules:     []domain.ABACRule{abacRule("DUAL_APPROVAL", domain.EffectRequire, "dual_approved", "eq", "true")},
			body:      `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`,
			wantBasis: "abac:require_failed=DUAL_APPROVAL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{
				rbacActions: []string{"PAYMENT_APPROVE"},
				rbacBasis:   "rbac:role=FINANCE_APPROVER",
				abacRules:   tc.rules,
			}
			r := newTestRouter(store)

			code, got := authorize(t, r, tc.body, map[string]string{"X-Tenant-Id": testTenant})
			if code != http.StatusOK {
				t.Fatalf("expected 200, got %d", code)
			}
			if got["decision_outcome"] != "DENIED" {
				t.Fatalf("outcome = %s, want DENIED", got["decision_outcome"])
			}
			if got["decision_basis"] != tc.wantBasis {
				t.Fatalf("basis = %q, want %q", got["decision_basis"], tc.wantBasis)
			}
			// The denial is still recorded — the artifact is the evidence, and
			// an ABAC denial is as material as any other.
			if store.recordedParams.Outcome != "DENIED" || store.recordedParams.Basis != tc.wantBasis {
				t.Fatalf("decision recorded as %s/%q", store.recordedParams.Outcome, store.recordedParams.Basis)
			}
		})
	}
}

// TestAuthorize_ABACIsDenyOnly — an ABAC rule cannot rescue an action the
// earlier layers refused. Layer 5 is only reachable from GRANTED, so a
// no_grant denial must stay no_grant and the ABAC store must not even be
// consulted.
func TestAuthorize_ABACIsDenyOnly(t *testing.T) {
	store := &stubStore{
		rbacActions: nil, // holds nothing
		abacRules:   []domain.ABACRule{abacRule("ALWAYS_TRUE", domain.EffectRequire, "channel", "exists", "")},
	}
	r := newTestRouter(store)

	code, got := authorize(t, r,
		`{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE","attributes":{"channel":"BRANCH"}}`,
		map[string]string{"X-Tenant-Id": testTenant})

	if code != http.StatusOK || got["decision_outcome"] != "DENIED" {
		t.Fatalf("code=%d outcome=%s, want 200/DENIED", code, got["decision_outcome"])
	}
	if got["decision_basis"] != "no_grant" {
		t.Fatalf("basis = %q, want no_grant — ABAC must not grant what RBAC did not", got["decision_basis"])
	}
	if store.abacActionArg != "" {
		t.Errorf("the ABAC lookup ran on an already-denied request (action %q) — wasted work on the hottest path", store.abacActionArg)
	}
}

// TestAuthorize_ABACLookupIsTenantScoped — the rules that deny a request must
// be the tenant's own plus the platform-wide ones, resolved through the same
// verified scope every other layer uses.
func TestAuthorize_ABACLookupIsTenantScoped(t *testing.T) {
	store := &stubStore{
		rbacActions: []string{"PAYMENT_APPROVE"},
		rbacBasis:   "rbac:role=FINANCE_APPROVER",
	}
	r := newTestRouter(store)

	_, _ = authorize(t, r,
		`{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`,
		map[string]string{"X-Tenant-Id": testTenant})

	if store.abacTenantArg != testTenant {
		t.Fatalf("ABAC lookup ran with tenant %q, want %q", store.abacTenantArg, testTenant)
	}
	if store.abacActionArg != "PAYMENT_APPROVE" {
		t.Fatalf("ABAC lookup ran for action %q, want PAYMENT_APPROVE", store.abacActionArg)
	}
}

// TestAuthorize_ABACStoreOutageFailsClosed — a layer that cannot be evaluated
// must not be skipped. 503, not a GRANTED that silently omitted a control.
func TestAuthorize_ABACStoreOutageFailsClosed(t *testing.T) {
	store := &stubStore{
		rbacActions: []string{"PAYMENT_APPROVE"},
		rbacBasis:   "rbac:role=FINANCE_APPROVER",
		abacErr:     domain.ErrStoreUnavailable,
	}
	r := newTestRouter(store)

	code, got := authorize(t, r,
		`{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`,
		map[string]string{"X-Tenant-Id": testTenant})

	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (%v)", code, got)
	}
	if store.recordedParams.Outcome != "" {
		t.Errorf("a decision was recorded for an evaluation that could not complete: %v", store.recordedParams)
	}
}

// TestAuthorize_ABACUnevaluableRuleDenies — an operator or effect this build
// cannot execute is a defect in the RULE, and a condition nobody can evaluate
// has not been met. Denies, with a basis naming the rule so it is fixable from
// the log rather than from an incident.
func TestAuthorize_ABACUnevaluableRuleDenies(t *testing.T) {
	store := &stubStore{
		rbacActions: []string{"PAYMENT_APPROVE"},
		rbacBasis:   "rbac:role=FINANCE_APPROVER",
		abacRules:   []domain.ABACRule{abacRule("TYPO_RULE", domain.EffectRequire, "amount", "approximately", "10000")},
	}
	r := newTestRouter(store)

	code, got := authorize(t, r,
		`{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE","attributes":{"amount":"10000"}}`,
		map[string]string{"X-Tenant-Id": testTenant})

	if code != http.StatusOK || got["decision_outcome"] != "DENIED" {
		t.Fatalf("code=%d outcome=%s, want 200/DENIED", code, got["decision_outcome"])
	}
	if got["decision_basis"] != "abac:rule_unevaluable=TYPO_RULE" {
		t.Fatalf("basis = %q, want abac:rule_unevaluable=TYPO_RULE", got["decision_basis"])
	}
}

// ── ABAC admin surface ───────────────────────────────────────────────────────

func TestCreateABACRule_RejectsUnsupportedOperatorAndEffect(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError string
	}{
		{
			name:      "unknown operator",
			body:      `{"rule_code":"R1","action_type":"PAYMENT_APPROVE","effect":"REQUIRE","attribute_key":"amount","operator":"approximately","attribute_value":"1","tenant_id":"` + testTenant + `"}`,
			wantError: "unsupported_operator",
		},
		{
			name:      "unknown effect",
			body:      `{"rule_code":"R1","action_type":"PAYMENT_APPROVE","effect":"MAYBE","attribute_key":"amount","operator":"eq","attribute_value":"1","tenant_id":"` + testTenant + `"}`,
			wantError: "unsupported_effect",
		},
		{
			// A comparison with nothing to compare against denies every
			// request for the action under REQUIRE and permits every one under
			// FORBID. Both are silent, so it is refused at authoring time.
			name:      "comparison operator with no operand",
			body:      `{"rule_code":"R1","action_type":"PAYMENT_APPROVE","effect":"REQUIRE","attribute_key":"amount","operator":"gt","tenant_id":"` + testTenant + `"}`,
			wantError: "missing_field",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{}
			r := newTestRouter(store)

			req := httptest.NewRequest(http.MethodPost, "/v1/admin/abac-rules", bytes.NewBufferString(tc.body))
			req.Header.Set("X-Principal-Id", "admin-1")
			req.Header.Set("X-Tenant-Id", testTenant)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			var got map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if got["error"] != tc.wantError {
				t.Fatalf("error = %q, want %q", got["error"], tc.wantError)
			}
			if store.gotCreateABACRule.RuleCode != "" {
				t.Errorf("an invalid rule reached the store: %+v", store.gotCreateABACRule)
			}
		})
	}
}

// TestCreateABACRule_PlatformWideRequiresThePlatformGrant. A rule with no
// tenant_id denies its action for EVERY tenant on the estate, so authoring one
// is a distinct grant rather than a side effect of omitting a field — exactly
// the posture CreateSoDRule already takes, and for the same reason.
func TestCreateABACRule_PlatformWideRequiresThePlatformGrant(t *testing.T) {
	// The caller holds nothing at platform scope, so requirePlatformAction
	// refuses.
	store := &stubStore{rbacActions: nil}
	r := newTestRouter(store)

	body := `{"rule_code":"GLOBAL_RULE","action_type":"PAYMENT_APPROVE","effect":"FORBID","attribute_key":"channel","operator":"eq","attribute_value":"SELF_SERVICE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/abac-rules", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", testTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a platform-wide rule with no platform grant, got %d: %s", w.Code, w.Body.String())
	}
	if store.gotCreateABACRule.RuleCode != "" {
		t.Errorf("the platform-wide rule was written anyway: %+v", store.gotCreateABACRule)
	}
}

// TestCreateABACRule_RequiresPrincipalAndTenant. Reaching this route at all
// requires both, whether or not the body names a tenant — the same hole
// CreateSoDRule closed: with no tenant required, one request carrying nothing
// but a principal header could store a rule denying an action platform-wide.
func TestCreateABACRule_RequiresPrincipalAndTenant(t *testing.T) {
	body := `{"rule_code":"R1","action_type":"PAYMENT_APPROVE","effect":"FORBID","attribute_key":"channel","operator":"eq","attribute_value":"X","tenant_id":"` + testTenant + `"}`

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{name: "no principal", headers: map[string]string{"X-Tenant-Id": testTenant}},
		{name: "no tenant", headers: map[string]string{"X-Principal-Id": "admin-1"}},
		{name: "neither", headers: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{}
			r := newTestRouter(store)
			req := httptest.NewRequest(http.MethodPost, "/v1/admin/abac-rules", bytes.NewBufferString(body))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
			}
			if store.gotCreateABACRule.RuleCode != "" {
				t.Errorf("an unauthenticated request wrote a rule: %+v", store.gotCreateABACRule)
			}
		})
	}
}

// TestCreateABACRule_RefusesAForeignTenant — the body must not be able to name
// someone else's tenant, the same check every other admin route carries.
func TestCreateABACRule_RefusesAForeignTenant(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	body := `{"rule_code":"R1","action_type":"PAYMENT_APPROVE","effect":"FORBID","attribute_key":"channel","operator":"eq","attribute_value":"X","tenant_id":"00000000-0000-0000-0000-0000000000ff"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/abac-rules", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", testTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if store.gotCreateABACRule.RuleCode != "" {
		t.Errorf("a rule was written into a foreign tenant: %+v", store.gotCreateABACRule)
	}
}

func TestListABACRules_IsTenantScopedAndForwardsTheFilter(t *testing.T) {
	store := &stubStore{listABACRules: []domain.ABACRule{abacRule("R1", domain.EffectForbid, "channel", "eq", "X")}}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/abac-rules?action_type=PAYMENT_APPROVE", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", testTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.gotABACListTenant != testTenant {
		t.Errorf("listed with tenant %q, want %q", store.gotABACListTenant, testTenant)
	}
	if store.gotABACListAction != "PAYMENT_APPROVE" {
		t.Errorf("listed with action %q, want PAYMENT_APPROVE", store.gotABACListAction)
	}

	var rules []domain.ABACRule
	if err := json.Unmarshal(w.Body.Bytes(), &rules); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
}

func TestRetireAndReactivateABACRule(t *testing.T) {
	store := &stubStore{setABACActiveRule: &domain.ABACRule{ABACRuleID: "r-1", RuleCode: "R1"}}
	r := newTestRouter(store)

	for _, path := range []string{"retire", "reactivate"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/abac-rules/r-1/"+path, nil)
		req.Header.Set("X-Principal-Id", "admin-1")
		req.Header.Set("X-Tenant-Id", testTenant)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", path, w.Code, w.Body.String())
		}
	}

	if len(store.setABACActiveWant) != 2 || store.setABACActiveWant[0] != false || store.setABACActiveWant[1] != true {
		t.Fatalf("SetABACRuleActive called with %v, want [false true]", store.setABACActiveWant)
	}
}

func TestRetireABACRule_NotFoundIs404(t *testing.T) {
	store := &stubStore{setABACActiveErr: domain.ErrABACRuleNotFound}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/abac-rules/r-missing/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", testTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ── the delegation admin API must be able to EXPRESS a subset ────────────────

// TestCreateDelegatedAuthority_ForwardsTheActionSubset is the regression test
// for a gap found by driving the real service end-to-end: the store and the
// schema gained `delegated_actions` (migration 000008) and the HTTP request
// struct did not, so a body naming a subset was silently dropped and the row
// was written with SQL NULL — which means the delegator's FULL authority.
//
// The end-to-end symptom was exact: a delegation of PAYMENT_APPROVE alone
// still granted PAYMENT_INITIATE. Every store-level test passed, because they
// call the store directly and bypass the request struct entirely.
func TestCreateDelegatedAuthority_ForwardsTheActionSubset(t *testing.T) {
	store := &stubStore{delegation: &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1"}}
	r := newTestRouter(store)

	body := `{"delegator_principal_id":"boss-1","delegate_principal_id":"assistant-1",` +
		`"scope_type":"ACTION_SUBSET","legal_entity_id":"le-1",` +
		`"delegated_actions":["PAYMENT_APPROVE"],"effective_from":"2026-01-01T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/delegated-authorities", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "boss-1")
	req.Header.Set("X-Tenant-Id", testTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	got := store.gotCreateDelegation.DelegatedActions
	if len(got) != 1 || got[0] != "PAYMENT_APPROVE" {
		t.Fatalf("store received delegated_actions = %v, want [PAYMENT_APPROVE] — "+
			"a dropped subset is stored as NULL, which means the delegator's FULL authority", got)
	}
}

// TestCreateDelegatedAuthority_ActionSubsetWithoutASubsetIsRefused. That exact
// combination is what shipped before 000008: a row that reads as restricted in
// the register and confers everything. Accepting it would recreate the
// over-grant through the API rather than through the schema.
func TestCreateDelegatedAuthority_ActionSubsetWithoutASubsetIsRefused(t *testing.T) {
	store := &stubStore{delegation: &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1"}}
	r := newTestRouter(store)

	body := `{"delegator_principal_id":"boss-1","delegate_principal_id":"assistant-1",` +
		`"scope_type":"ACTION_SUBSET","legal_entity_id":"le-1","effective_from":"2026-01-01T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/delegated-authorities", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "boss-1")
	req.Header.Set("X-Tenant-Id", testTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["field"] != "delegated_actions" {
		t.Fatalf("field = %q, want delegated_actions", got["field"])
	}
	if store.gotCreateDelegation.DelegatePrincipalID != "" {
		t.Errorf("the delegation was written anyway: %+v", store.gotCreateDelegation)
	}
}

// TestCreateDelegatedAuthority_FullScopeNeedsNoSubset — omitting
// delegated_actions is the pre-000008 meaning and stays valid: the delegator's
// full authority.
func TestCreateDelegatedAuthority_FullScopeNeedsNoSubset(t *testing.T) {
	store := &stubStore{delegation: &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1"}}
	r := newTestRouter(store)

	body := `{"delegator_principal_id":"boss-1","delegate_principal_id":"assistant-1",` +
		`"scope_type":"FULL","legal_entity_id":"le-1","effective_from":"2026-01-01T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/delegated-authorities", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "boss-1")
	req.Header.Set("X-Tenant-Id", testTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(store.gotCreateDelegation.DelegatedActions) != 0 {
		t.Fatalf("a FULL delegation carried a subset: %v", store.gotCreateDelegation.DelegatedActions)
	}
}
