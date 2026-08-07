package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ── mock authz client ────────────────────────────────────────────────────────

// mockAuthZ returns whatever error is configured — nil means permit.
// Used to exercise the 403 (ErrUnauthorized) and 503 (ErrAuthZUnavailable)
// paths without a real Authorization Service.
type mockAuthZ struct {
	err error

	// captured arguments, so the tests can prove the handler forwards the
	// caller's real identity rather than a hardcoded one.
	principalID string
	scopeID     string
	resource    string
	action      string
	calls       int
}

func (m *mockAuthZ) Authorize(_ context.Context, principalID, scopeID, resource, action string) error {
	m.principalID, m.scopeID, m.resource, m.action = principalID, scopeID, resource, action
	m.calls++
	return m.err
}

func nopLogger() *zap.Logger {
	return zap.NewNop()
}

func permitAll() authz.AuthorizationClient { return authz.NewStubAuthZClient(nopLogger()) }

// ── helpers ──────────────────────────────────────────────────────────────────

// testPrincipal is the value the gateway's ForwardAuth middleware would set
// in X-Principal-Id after verifying the caller's identity envelope.
const testPrincipal = "principal-admin-1"

// postJSON issues an authenticated admin POST — the principal header is
// included by default because every admin route now requires one.
func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return postJSONAs(t, h, path, body, testPrincipal)
}

// postJSONAs is the same, with an explicit principal. An empty principal
// sends no identity header at all.
func postJSONAs(t *testing.T, h http.Handler, path string, body any, principal string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("failed to encode request body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	return executeRequest(h, req)
}

func decodeError(t *testing.T, rr *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	return body
}

// validJurisdictionBody is a complete, valid create request. Tests that
// exercise one specific failure mutate a copy, so they cannot accidentally
// pass for the wrong reason (a missing unrelated field).
func validJurisdictionBody() map[string]any {
	return map[string]any{
		"jurisdiction_code": "GB",
		"jurisdiction_name": "United Kingdom",
		"jurisdiction_type": "COUNTRY",
		"authority_type":    "FEDERAL",
		"effective_from":    time.Now().UTC().Format(time.RFC3339),
	}
}

func validRuleBody() map[string]any {
	return map[string]any{
		"rule_domain":    "PAYROLL",
		"rule_code":      "MIN-WAGE",
		"rule_name":      "Minimum Wage",
		"effective_from": time.Now().UTC().Format(time.RFC3339),
		"rule_payload":   json.RawMessage(`{"applies_to_entity_types":["COMPANY"]}`),
		"rule_status":    "DRAFT",
	}
}

// okStore returns a stub whose every mutation succeeds, for the
// cross-cutting tests that drive all admin routes and care only about the
// gate in front of them.
func okStore() *stubStore {
	return &stubStore{
		createdJurisdiction:     &domain.Jurisdiction{JurisdictionID: "j-1", JurisdictionCode: "GB"},
		jurisdictionWasCreated:  true,
		deactivatedJurisdiction: &domain.Jurisdiction{JurisdictionID: "j-1"},
		createdRule:             &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "DRAFT"},
		ruleWasCreated:          true,
		transitionedRule:        &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "ACTIVE"},
		transitionDidRun:        true,
		driftRule:               &domain.JurisdictionRule{JurisdictionRuleID: "r-1", LegalDriftState: "DRIFTED"},
		driftEvent:              &domain.DriftEvent{DriftEventID: "d-1", FromState: "CURRENT", ToState: "DRIFTED"},
		driftChanged:            true,
	}
}

// adminRoutes is every mutating route, used by the cross-cutting tests below
// so a route added later cannot quietly skip authentication or authorization.
var adminRoutes = []struct {
	name string
	path string
	body map[string]any
}{
	{"create jurisdiction", "/v1/admin/jurisdictions", validJurisdictionBody()},
	{"deactivate jurisdiction", "/v1/admin/jurisdictions/j-1/deactivate", nil},
	{"create rule", "/v1/admin/jurisdictions/j-1/rules", validRuleBody()},
	{"transition rule", "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "ACTIVE"}},
	{"record drift", "/v1/admin/rules/r-1/drift", map[string]any{"drift_state": "DRIFTED"}},
}

// ── cross-cutting: identity and authorization ────────────────────────────────

// TestAdminRoutes_401_WithoutPrincipal covers the audit hole this replaced:
// the actor was read from X-Actor-Principal-ID — a header nothing in the
// platform sets — and fell back to the literal string "system" when absent.
// Every unattributed write was therefore recorded as the platform's own.
func TestAdminRoutes_401_WithoutPrincipal(t *testing.T) {
	for _, route := range adminRoutes {
		t.Run(route.name, func(t *testing.T) {
			st := &stubStore{}
			rr := postJSONAs(t, newTestRouter(st), route.path, route.body, "")

			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 without a principal, got %d — body: %s", rr.Code, rr.Body.String())
			}
			if got := decodeError(t, rr)["error"]; got != "missing_principal" {
				t.Errorf("expected error=missing_principal, got %q", got)
			}
			if st.storeCalled {
				t.Error("FAIL: an unauthenticated request reached the store")
			}
		})
	}
}

// TestAdminRoutes_AuthorizationIsChecked proves the authz gate is actually
// invoked on every mutating route, with the caller's real principal and the
// configured platform scope.
func TestAdminRoutes_AuthorizationIsChecked(t *testing.T) {
	for _, route := range adminRoutes {
		t.Run(route.name, func(t *testing.T) {
			m := &mockAuthZ{}
			st := okStore()
			rr := postJSON(t, newTestRouterWithAuthz(st, m), route.path, route.body)

			if rr.Code >= 400 {
				t.Fatalf("permitted request failed with %d — body: %s", rr.Code, rr.Body.String())
			}
			if m.calls != 1 {
				t.Fatalf("expected exactly 1 authorization check, got %d", m.calls)
			}
			if m.principalID != testPrincipal {
				t.Errorf("authz saw principal %q, want %q", m.principalID, testPrincipal)
			}
			if m.scopeID != testAuthzScopeID {
				t.Errorf("authz saw scope %q, want %q", m.scopeID, testAuthzScopeID)
			}
		})
	}
}

// TestAdminRoutes_403_DeniedNeverReachesStore — a denial must stop the write,
// not merely change the status code after it happened.
func TestAdminRoutes_403_DeniedNeverReachesStore(t *testing.T) {
	for _, route := range adminRoutes {
		t.Run(route.name, func(t *testing.T) {
			st := &stubStore{}
			rr := postJSON(t, newTestRouterWithAuthz(st, &mockAuthZ{err: authz.ErrUnauthorized}), route.path, route.body)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d — body: %s", rr.Code, rr.Body.String())
			}
			if st.storeCalled {
				t.Error("FAIL: a denied request still reached the store")
			}
		})
	}
}

// TestAdminRoutes_503_AuthzUnavailableFailsClosed — an unreachable
// authorization service must block the mutation, not wave it through.
func TestAdminRoutes_503_AuthzUnavailableFailsClosed(t *testing.T) {
	for _, route := range adminRoutes {
		t.Run(route.name, func(t *testing.T) {
			st := &stubStore{}
			rr := postJSON(t, newTestRouterWithAuthz(st, &mockAuthZ{err: authz.ErrAuthZUnavailable}), route.path, route.body)

			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d — body: %s", rr.Code, rr.Body.String())
			}
			if got := decodeError(t, rr)["error"]; got != "authz_unavailable" {
				t.Errorf("expected error=authz_unavailable, got %q", got)
			}
			if st.storeCalled {
				t.Error("FAIL: a request reached the store without an authorization decision")
			}
		})
	}
}

// ── CreateJurisdiction ───────────────────────────────────────────────────────

func TestCreateJurisdiction_201_Created(t *testing.T) {
	st := &stubStore{
		createdJurisdiction:    &domain.Jurisdiction{JurisdictionID: "j-1", JurisdictionCode: "GB"},
		jurisdictionWasCreated: true,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/jurisdictions", validJurisdictionBody())

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if st.createJurisdictionArgs.CreatedByPrincipalID != testPrincipal {
		t.Errorf("created_by_principal_id = %q, want the calling principal %q",
			st.createJurisdictionArgs.CreatedByPrincipalID, testPrincipal)
	}
	if !pub.has("jurisdiction.created") {
		t.Errorf("expected jurisdiction.created to be published, got %v", pub.emitted)
	}
}

// TestCreateJurisdiction_200_IdempotentReplay also asserts the replay stays
// silent — a consumer must not see a second jurisdiction.created for a
// jurisdiction that already existed.
func TestCreateJurisdiction_200_IdempotentReplay(t *testing.T) {
	st := &stubStore{
		createdJurisdiction:    &domain.Jurisdiction{JurisdictionID: "j-1", JurisdictionCode: "GB"},
		jurisdictionWasCreated: false,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/jurisdictions", validJurisdictionBody())

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for idempotent replay, got %d", rr.Code)
	}
	if len(pub.emitted) != 0 {
		t.Errorf("an idempotent replay must not publish, got %v", pub.emitted)
	}
}

func TestCreateJurisdiction_409_Conflict(t *testing.T) {
	st := &stubStore{createJurisdictionErr: domain.ErrConflict}
	h := newTestRouterWithAuthz(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/jurisdictions", validJurisdictionBody())

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "conflict" {
		t.Fatalf("expected error=conflict, got %q", got)
	}
}

// TestCreateJurisdiction_404_ParentNotFound — an unknown parent used to hit
// the self-referential foreign key and surface as 503 store_unavailable, so a
// client mistake was indistinguishable from a database outage.
func TestCreateJurisdiction_404_ParentNotFound(t *testing.T) {
	st := &stubStore{createJurisdictionErr: domain.ErrParentNotFound}
	body := validJurisdictionBody()
	body["parent_jurisdiction_id"] = "11111111-1111-1111-1111-111111111111"

	rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions", body)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if got := decodeError(t, rr)["error"]; got != "parent_jurisdiction_not_found" {
		t.Errorf("expected error=parent_jurisdiction_not_found, got %q", got)
	}
}

func TestCreateJurisdiction_400_MalformedBody(t *testing.T) {
	st := &stubStore{}
	h := newTestRouterWithAuthz(st, permitAll())

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/jurisdictions", bytes.NewBufferString("{not json"))
	req.Header.Set("X-Principal-Id", testPrincipal)
	rr := executeRequest(h, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestCreateJurisdiction_400_MissingRequiredFields — the NOT NULL columns
// accept the empty string, so without handler validation a POST with no body
// fields created a nameless, codeless jurisdiction and returned 201.
func TestCreateJurisdiction_400_MissingRequiredFields(t *testing.T) {
	for _, field := range []string{"jurisdiction_code", "jurisdiction_name", "jurisdiction_type", "authority_type"} {
		t.Run("missing "+field, func(t *testing.T) {
			st := &stubStore{}
			body := validJurisdictionBody()
			delete(body, field)

			rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions", body)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 with %s omitted, got %d — body: %s", field, rr.Code, rr.Body.String())
			}
			got := decodeError(t, rr)
			if got["error"] != "missing_field" || got["field"] != field {
				t.Errorf("expected missing_field/%s, got %v", field, got)
			}
			if st.storeCalled {
				t.Error("an invalid create reached the store")
			}
		})
	}
}

func TestCreateJurisdiction_400_MissingEffectiveFrom(t *testing.T) {
	st := &stubStore{}
	body := validJurisdictionBody()
	delete(body, "effective_from")

	rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions", body)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if got := decodeError(t, rr)["field"]; got != "effective_from" {
		t.Errorf("expected field=effective_from, got %q", got)
	}
}

// TestCreateJurisdiction_400_InvertedEffectivePeriod — a period that ends
// before it starts can never match a point-in-time query, so the record would
// exist and be permanently unreachable.
func TestCreateJurisdiction_400_InvertedEffectivePeriod(t *testing.T) {
	st := &stubStore{}
	body := validJurisdictionBody()
	body["effective_from"] = "2025-01-01T00:00:00Z"
	body["effective_to"] = "2024-01-01T00:00:00Z"

	rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions", body)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if got := decodeError(t, rr)["error"]; got != "invalid_effective_period" {
		t.Errorf("expected error=invalid_effective_period, got %q", got)
	}
	if st.storeCalled {
		t.Error("an invalid effective period reached the store")
	}
}

// TestCreateJurisdiction_400_UnknownField — a misspelled field would
// otherwise be silently dropped and the caller would get a 201 for a record
// missing the value they thought they sent.
func TestCreateJurisdiction_400_UnknownField(t *testing.T) {
	st := &stubStore{}
	body := validJurisdictionBody()
	body["jurisdcition_name"] = "typo"

	rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions", body)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown field, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if st.storeCalled {
		t.Error("a request with an unknown field reached the store")
	}
}

// ── DeactivateJurisdiction ───────────────────────────────────────────────────

func TestDeactivateJurisdiction_200_OK(t *testing.T) {
	st := &stubStore{deactivatedJurisdiction: &domain.Jurisdiction{JurisdictionID: "j-1", ActiveFlag: false}}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/deactivate", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if st.deactivateActorID != testPrincipal {
		t.Errorf("store recorded actor %q, want %q", st.deactivateActorID, testPrincipal)
	}
	if !pub.has("jurisdiction.deactivated") {
		t.Errorf("expected jurisdiction.deactivated to be published, got %v", pub.emitted)
	}
}

func TestDeactivateJurisdiction_404_NotFound(t *testing.T) {
	st := &stubStore{deactivateErr: domain.ErrJurisdictionNotFound}
	h := newTestRouterWithAuthz(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/jurisdictions/unknown/deactivate", nil)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

// ── CreateRule ───────────────────────────────────────────────────────────────

func TestCreateRule_201_Created(t *testing.T) {
	st := &stubStore{
		createdRule:    &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "DRAFT"},
		ruleWasCreated: true,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/rules", validRuleBody())

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if !pub.has("jurisdiction.rule.updated") {
		t.Errorf("expected jurisdiction.rule.updated, got %v", pub.emitted)
	}
	// A DRAFT rule is not in force, so nothing should be told it activated.
	if pub.has("jurisdiction.rule.activated") {
		t.Errorf("a DRAFT rule must not publish rule.activated, got %v", pub.emitted)
	}
}

// TestCreateRule_201_ActiveAlsoPublishesActivated — a rule created directly
// in ACTIVE never passes through the transition endpoint, so activation has
// to be announced at creation or consumers never hear about it.
func TestCreateRule_201_ActiveAlsoPublishesActivated(t *testing.T) {
	st := &stubStore{
		createdRule:    &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "ACTIVE"},
		ruleWasCreated: true,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	body := validRuleBody()
	body["rule_status"] = "ACTIVE"
	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/rules", body)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if !pub.has("jurisdiction.rule.activated") {
		t.Errorf("expected jurisdiction.rule.activated, got %v", pub.emitted)
	}
}

func TestCreateRule_200_ReplayDoesNotPublish(t *testing.T) {
	st := &stubStore{
		createdRule:    &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "ACTIVE"},
		ruleWasCreated: false,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/rules", validRuleBody())

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for replay, got %d", rr.Code)
	}
	if len(pub.emitted) != 0 {
		t.Errorf("an idempotent replay must not publish, got %v", pub.emitted)
	}
}

func TestCreateRule_409_Conflict(t *testing.T) {
	st := &stubStore{createRuleErr: domain.ErrConflict}
	h := newTestRouterWithAuthz(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/rules", validRuleBody())

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rr.Code)
	}
}

// TestCreateRule_409_OverlappingRule — two live rules with the same code and
// overlapping periods make "the effective rule at date X" ambiguous.
func TestCreateRule_409_OverlappingRule(t *testing.T) {
	st := &stubStore{createRuleErr: domain.ErrOverlappingRule}
	rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions/j-1/rules", validRuleBody())

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "overlapping_rule" {
		t.Errorf("expected error=overlapping_rule, got %q", got)
	}
}

// TestCreateRule_404_JurisdictionNotFound — creating a rule against a missing
// or deactivated jurisdiction used to violate the foreign key and read as 503.
func TestCreateRule_404_JurisdictionNotFound(t *testing.T) {
	st := &stubStore{createRuleErr: domain.ErrJurisdictionNotFound}
	rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions/unknown/rules", validRuleBody())

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "jurisdiction_not_found" {
		t.Errorf("expected error=jurisdiction_not_found, got %q", got)
	}
}

func TestCreateRule_400_MissingRequiredFields(t *testing.T) {
	for _, field := range []string{"rule_domain", "rule_code", "rule_name", "effective_from"} {
		t.Run("missing "+field, func(t *testing.T) {
			st := &stubStore{}
			body := validRuleBody()
			delete(body, field)

			rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions/j-1/rules", body)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 with %s omitted, got %d — body: %s", field, rr.Code, rr.Body.String())
			}
			if st.storeCalled {
				t.Error("an invalid create reached the store")
			}
		})
	}
}

// TestCreateRule_400_RejectsNonCreateableStatus is the regression test for a
// state-machine bypass: rule_status was taken verbatim from the body, so a
// caller could POST a rule straight into SUPERSEDED — or into a status that
// does not exist — and skip DRAFT→ACTIVE entirely.
func TestCreateRule_400_RejectsNonCreateableStatus(t *testing.T) {
	for _, status := range []string{"SUPERSEDED", "RETIRED", "BANANAS", "active"} {
		t.Run(status, func(t *testing.T) {
			st := &stubStore{}
			body := validRuleBody()
			body["rule_status"] = status

			rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions/j-1/rules", body)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for rule_status=%q, got %d — body: %s", status, rr.Code, rr.Body.String())
			}
			if got := decodeError(t, rr)["error"]; got != "invalid_rule_status" {
				t.Errorf("expected error=invalid_rule_status, got %q", got)
			}
			if st.storeCalled {
				t.Error("a rule with an uncreateable status reached the store")
			}
		})
	}
}

// TestCreateRule_DefaultsStatusToDraft — an omitted rule_status used to be
// written as the empty string into a NOT NULL column, producing a rule that
// no query ever matches.
func TestCreateRule_DefaultsStatusToDraft(t *testing.T) {
	st := &stubStore{
		createdRule:    &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "DRAFT"},
		ruleWasCreated: true,
	}
	body := validRuleBody()
	delete(body, "rule_status")

	rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions/j-1/rules", body)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if st.createRuleArgs.RuleStatus != "DRAFT" {
		t.Errorf("store received rule_status=%q, want DRAFT", st.createRuleArgs.RuleStatus)
	}
}

// TestCreateRule_RulePayload covers the JSONB column's two failure modes: an
// omitted payload sent zero bytes, which Postgres rejects as invalid JSON
// (surfacing as 503), and a scalar payload was accepted even though every
// documented payload — and every consumer reading applies_to_entity_types
// out of it — assumes an object.
func TestCreateRule_RulePayload(t *testing.T) {
	cases := []struct {
		name       string
		payload    any
		omit       bool
		wantStatus int
		wantStored string
	}{
		{name: "omitted defaults to empty object", omit: true, wantStatus: http.StatusCreated, wantStored: `{}`},
		{name: "explicit null defaults to empty object", payload: nil, wantStatus: http.StatusCreated, wantStored: `{}`},
		{name: "object is kept", payload: json.RawMessage(`{"filing_frequency":"MONTHLY"}`), wantStatus: http.StatusCreated, wantStored: `{"filing_frequency":"MONTHLY"}`},
		{name: "string scalar rejected", payload: json.RawMessage(`"MONTHLY"`), wantStatus: http.StatusBadRequest},
		{name: "number scalar rejected", payload: json.RawMessage(`7`), wantStatus: http.StatusBadRequest},
		{name: "array rejected", payload: json.RawMessage(`[1,2]`), wantStatus: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &stubStore{
				createdRule:    &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "DRAFT"},
				ruleWasCreated: true,
			}
			body := validRuleBody()
			if tc.omit {
				delete(body, "rule_payload")
			} else {
				body["rule_payload"] = tc.payload
			}

			rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions/j-1/rules", body)

			if rr.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d — body: %s", tc.wantStatus, rr.Code, rr.Body.String())
			}
			if tc.wantStored != "" && strings.TrimSpace(string(st.createRuleArgs.RulePayload)) != tc.wantStored {
				t.Errorf("store received payload %q, want %q", st.createRuleArgs.RulePayload, tc.wantStored)
			}
		})
	}
}

// TestCreateRule_413_BodyTooLarge — rule_payload is caller-supplied JSON with
// no size bound in the schema, so an admin POST could otherwise stream an
// arbitrarily large body into memory and into a JSONB column.
func TestCreateRule_413_BodyTooLarge(t *testing.T) {
	st := &stubStore{}
	huge := `{"rule_domain":"PAYROLL","rule_payload":{"x":"` + strings.Repeat("a", 300<<10) + `"}}`

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/jurisdictions/j-1/rules", strings.NewReader(huge))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", testPrincipal)
	rr := executeRequest(newTestRouterWithAuthz(st, permitAll()), req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if st.storeCalled {
		t.Error("an oversized body reached the store")
	}
}

// ── TransitionRuleStatus ─────────────────────────────────────────────────────

func TestTransitionRuleStatus_200_OK(t *testing.T) {
	st := &stubStore{
		transitionedRule: &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "ACTIVE"},
		transitionDidRun: true,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "ACTIVE"})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !pub.has("jurisdiction.rule.activated") {
		t.Errorf("expected jurisdiction.rule.activated, got %v", pub.emitted)
	}
	if st.transitionArgs.ActorID != testPrincipal {
		t.Errorf("store recorded actor %q, want %q", st.transitionArgs.ActorID, testPrincipal)
	}
}

// TestTransitionRuleStatus_ReplayDoesNotPublish — a retried transition
// returns 200 with transitioned=false, and a consumer that saw
// rule.activated twice would double-apply whatever activation triggers.
func TestTransitionRuleStatus_ReplayDoesNotPublish(t *testing.T) {
	st := &stubStore{
		transitionedRule: &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "ACTIVE"},
		transitionDidRun: false,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "ACTIVE"})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if len(pub.emitted) != 0 {
		t.Errorf("an idempotent transition must not publish, got %v", pub.emitted)
	}
}

// TestTransitionRuleStatus_EndDatesClosingTransitions — a rule left with
// effective_to = NULL after being superseded keeps matching every
// point-in-time query alongside the rule that replaced it.
func TestTransitionRuleStatus_EndDatesClosingTransitions(t *testing.T) {
	cases := map[string]bool{
		"ACTIVE":     false,
		"SUPERSEDED": true,
		"RETIRED":    true,
	}
	for status, wantEndDate := range cases {
		t.Run(status, func(t *testing.T) {
			st := &stubStore{
				transitionedRule: &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: status},
				transitionDidRun: true,
			}
			rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()),
				"/v1/admin/rules/r-1/transition", map[string]any{"new_status": status})

			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rr.Code)
			}
			if st.transitionArgs.EndDate != wantEndDate {
				t.Errorf("EndDate = %v for %s, want %v", st.transitionArgs.EndDate, status, wantEndDate)
			}
		})
	}
}

// TestTransitionRuleStatus_ForwardsExplicitEffectiveTo — the operator knows
// when a rule stopped applying; "now" is only the fallback.
func TestTransitionRuleStatus_ForwardsExplicitEffectiveTo(t *testing.T) {
	st := &stubStore{
		transitionedRule: &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "SUPERSEDED"},
		transitionDidRun: true,
	}
	want := time.Date(2025, time.April, 6, 0, 0, 0, 0, time.UTC)

	rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/rules/r-1/transition", map[string]any{
		"new_status":   "SUPERSEDED",
		"effective_to": want.Format(time.RFC3339),
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if st.transitionArgs.EffectiveTo == nil || !st.transitionArgs.EffectiveTo.Equal(want) {
		t.Errorf("store received effective_to %v, want %v", st.transitionArgs.EffectiveTo, want)
	}
}

func TestTransitionRuleStatus_400_UnrecognizedTargetStatus(t *testing.T) {
	st := &stubStore{}
	h := newTestRouterWithAuthz(st, permitAll())

	// "DRAFT" is not a key in ruleStatusAllowedPriors — nothing transitions back to DRAFT.
	rr := postJSON(t, h, "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "DRAFT"})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "invalid_status" {
		t.Fatalf("expected error=invalid_status, got %q", got)
	}
}

func TestTransitionRuleStatus_409_InvalidTransition(t *testing.T) {
	st := &stubStore{transitionErr: domain.ErrInvalidTransition}
	h := newTestRouterWithAuthz(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "RETIRED"})

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "invalid_transition" {
		t.Fatalf("expected error=invalid_transition, got %q", got)
	}
}

func TestTransitionRuleStatus_404_RuleNotFound(t *testing.T) {
	st := &stubStore{transitionErr: domain.ErrRuleNotFound}
	h := newTestRouterWithAuthz(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/rules/unknown/transition", map[string]any{"new_status": "ACTIVE"})

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
	if got := decodeError(t, rr)["error"]; got != "rule_not_found" {
		t.Fatalf("expected error=rule_not_found, got %q", got)
	}
}

// ── RecordDrift ──────────────────────────────────────────────────────────────

// TestRecordDrift_200_PublishesDetected covers §8.2's Critical Enhancement:
// declaring that a stored rule has diverged from legal reality both records
// the transition and tells the platform about it.
func TestRecordDrift_200_PublishesDetected(t *testing.T) {
	reason := "HMRC SI 2025/412 revised the threshold"
	st := &stubStore{
		driftRule:    &domain.JurisdictionRule{JurisdictionRuleID: "r-1", LegalDriftState: "DRIFTED"},
		driftEvent:   &domain.DriftEvent{DriftEventID: "d-1", FromState: "CURRENT", ToState: "DRIFTED"},
		driftChanged: true,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/rules/r-1/drift", map[string]any{
		"drift_state": "DRIFTED",
		"reason":      reason,
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body.String())
	}
	if !pub.has("legal.drift.detected") {
		t.Errorf("expected legal.drift.detected, got %v", pub.emitted)
	}
	if st.driftArgs.Reason == nil || *st.driftArgs.Reason != reason {
		t.Errorf("store received reason %v, want %q", st.driftArgs.Reason, reason)
	}
	if st.driftArgs.RecordedByPrincipalID != testPrincipal {
		t.Errorf("store recorded principal %q, want %q", st.driftArgs.RecordedByPrincipalID, testPrincipal)
	}
}

// TestRecordDrift_ResolutionDoesNotPublishDetected — returning to CURRENT is
// a resolution, and announcing it as "drift detected" would have consumers
// react to the opposite of what happened.
func TestRecordDrift_ResolutionDoesNotPublishDetected(t *testing.T) {
	st := &stubStore{
		driftRule:    &domain.JurisdictionRule{JurisdictionRuleID: "r-1", LegalDriftState: "CURRENT"},
		driftEvent:   &domain.DriftEvent{DriftEventID: "d-2", FromState: "UNDER_REVIEW", ToState: "CURRENT"},
		driftChanged: true,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/rules/r-1/drift", map[string]any{"drift_state": "CURRENT"})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if pub.has("legal.drift.detected") {
		t.Errorf("resolving drift must not publish legal.drift.detected, got %v", pub.emitted)
	}
}

func TestRecordDrift_ReplayDoesNotPublish(t *testing.T) {
	st := &stubStore{
		driftRule:    &domain.JurisdictionRule{JurisdictionRuleID: "r-1", LegalDriftState: "DRIFTED"},
		driftChanged: false,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())

	rr := postJSON(t, h, "/v1/admin/rules/r-1/drift", map[string]any{"drift_state": "DRIFTED"})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if len(pub.emitted) != 0 {
		t.Errorf("an unchanged drift state must not publish, got %v", pub.emitted)
	}
}

func TestRecordDrift_400_InvalidState(t *testing.T) {
	for _, state := range []string{"", "DRIFTING", "current", "OK"} {
		t.Run("state="+state, func(t *testing.T) {
			st := &stubStore{}
			rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/rules/r-1/drift",
				map[string]any{"drift_state": state})

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for drift_state=%q, got %d", state, rr.Code)
			}
			if got := decodeError(t, rr)["error"]; got != "invalid_drift_state" {
				t.Errorf("expected error=invalid_drift_state, got %q", got)
			}
			if st.storeCalled {
				t.Error("an invalid drift state reached the store")
			}
		})
	}
}

func TestRecordDrift_404_RuleNotFound(t *testing.T) {
	st := &stubStore{driftErr: domain.ErrRuleNotFound}
	rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/rules/nope/drift",
		map[string]any{"drift_state": "DRIFTED"})

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

// TestRecordDrift_PublishFailureDoesNotFailRequest — the state change is
// already committed; refusing the response would tell the caller nothing
// happened when something did.
func TestRecordDrift_PublishFailureDoesNotFailRequest(t *testing.T) {
	st := &stubStore{
		driftRule:    &domain.JurisdictionRule{JurisdictionRuleID: "r-1", LegalDriftState: "DRIFTED"},
		driftEvent:   &domain.DriftEvent{DriftEventID: "d-1"},
		driftChanged: true,
	}
	h, pub := newTestRouterWithPublisher(st, permitAll())
	pub.err = context.DeadlineExceeded

	rr := postJSON(t, h, "/v1/admin/rules/r-1/drift", map[string]any{"drift_state": "DRIFTED"})

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 despite a broker failure, got %d", rr.Code)
	}
}
