package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
)

// Layer 0 — the principal-status gate.
//
// A principal identity-context-svc has SUSPENDED or DISABLED may execute
// nothing, and until migration 000013 this service had no way to know that.
// Session eviction does not cover it: /v1/authorize is called east-west by 111
// services on envelopes resolved before the suspension, and by queued work
// carrying a principal and no session at all.
//
// The two properties these tests exist to hold:
//
//  1. ABSENT MEANS ACTIVE. The projection ships empty, so a deployment that
//     has seen no status event must behave exactly as it did before the layer
//     existed. This is the one fail-OPEN default in the service and the
//     alternative is denying every principal on the platform on first deploy.
//  2. A layer-0 denial IS RECORDED. It is not a short-circuit — it goes
//     through recordAndAnswer like every other outcome, so the artifact, the
//     authorization.denied event and the SIEM signal all still happen. A
//     suspended principal being refused is precisely the evidence §8.3 wants.

func authorizeAs(t *testing.T, store *stubStore, pub *stubPublisher) *httptest.ResponseRecorder {
	t.Helper()
	r := newTestRouterFull(store, pub, &stubValidator{})
	body := `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE","tenant_id":"t-1"}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeDecision(t *testing.T, w *httptest.ResponseRecorder) (outcome, basis string) {
	t.Helper()
	var resp struct {
		DecisionOutcome string `json:"decision_outcome"`
		DecisionBasis   string `json:"decision_basis"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, w.Body.String())
	}
	return resp.DecisionOutcome, resp.DecisionBasis
}

// Property 1. The stub's default is ACTIVE, which models the empty projection.
func TestAuthorize_NoProjectedStatusGrantsAsBefore(t *testing.T) {
	store := &stubStore{
		rbacActions: []string{"PAYMENT_APPROVE"},
		rbacBasis:   "rbac:role=FINANCE_APPROVER",
	}
	w := authorizeAs(t, store, &stubPublisher{})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	outcome, basis := decodeDecision(t, w)
	if outcome != "GRANTED" {
		t.Fatalf("outcome = %s basis = %s — an empty projection must change no outcome", outcome, basis)
	}
	if store.principalStatusCallCount != 1 {
		t.Errorf("FindPrincipalStatus called %d times, want exactly 1", store.principalStatusCallCount)
	}
}

func TestAuthorize_SuspendedPrincipalIsDeniedEverything(t *testing.T) {
	store := &stubStore{
		principalStatus: "SUSPENDED",
		// Holds the action outright. The point is that it does not matter.
		rbacActions: []string{"PAYMENT_APPROVE"},
		rbacBasis:   "rbac:role=FINANCE_APPROVER",
	}
	w := authorizeAs(t, store, &stubPublisher{})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (the evaluation succeeded), got %d: %s", w.Code, w.Body.String())
	}
	outcome, basis := decodeDecision(t, w)
	if outcome != "DENIED" {
		t.Fatalf("outcome = %s, want DENIED for a suspended principal holding the action", outcome)
	}
	// The STATUS is in the basis, not just the fact of denial: suspended is
	// reversible and usually deliberate, disabled is usually terminal, and a
	// bare "principal_not_active" would send an operator to ask
	// identity-context-svc which it was.
	if basis != "principal_status:SUSPENDED" {
		t.Fatalf("basis = %q, want principal_status:SUSPENDED", basis)
	}
}

func TestAuthorize_DisabledPrincipalIsDeniedWithItsOwnStatus(t *testing.T) {
	store := &stubStore{principalStatus: "DISABLED", rbacActions: []string{"PAYMENT_APPROVE"}}
	w := authorizeAs(t, store, &stubPublisher{})

	_, basis := decodeDecision(t, w)
	if basis != "principal_status:DISABLED" {
		t.Fatalf("basis = %q, want principal_status:DISABLED", basis)
	}
}

// A status this build has never seen denies. There is no safe interpretation of
// an unknown standing on an authorization plane, and identity-context-svc may
// add a fourth value — a deny-list of SUSPENDED/DISABLED would have silently
// admitted it.
func TestAuthorize_UnrecognisedStatusDenies(t *testing.T) {
	store := &stubStore{principalStatus: "PENDING_REVIEW", rbacActions: []string{"PAYMENT_APPROVE"}}
	w := authorizeAs(t, store, &stubPublisher{})

	outcome, basis := decodeDecision(t, w)
	if outcome != "DENIED" {
		t.Fatalf("outcome = %s, want DENIED for a status this build cannot interpret", outcome)
	}
	if basis != "principal_status:PENDING_REVIEW" {
		t.Fatalf("basis = %q — the unrecognised status has to be named or nobody can diagnose the denial", basis)
	}
}

// Property 2, and the reason recordAndAnswer was extracted rather than
// duplicated: the layer-0 denial writes the artifact and publishes, exactly as
// every other outcome does.
func TestAuthorize_LayerZeroDenialIsRecordedAndPublished(t *testing.T) {
	store := &stubStore{principalStatus: "SUSPENDED", rbacActions: []string{"PAYMENT_APPROVE"}}
	pub := &stubPublisher{}
	if w := authorizeAs(t, store, pub); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if store.recordedParams.Outcome != "DENIED" {
		t.Fatalf("recorded outcome = %q — a suspended principal being refused is exactly the evidence the critical constraint requires",
			store.recordedParams.Outcome)
	}
	if store.recordedParams.Basis != "principal_status:SUSPENDED" {
		t.Errorf("recorded basis = %q", store.recordedParams.Basis)
	}
	if store.recordedParams.PrincipalID != "p-1" {
		t.Errorf("recorded principal = %q", store.recordedParams.PrincipalID)
	}
	if store.recordedParams.TenantID != "t-1" {
		t.Errorf("recorded tenant = %q — the resolved scope, not empty", store.recordedParams.TenantID)
	}
	if pub.deniedCalls != 1 {
		t.Errorf("authorization.denied published %d times, want 1", pub.deniedCalls)
	}
	// NOT an SoD violation. The basis is deliberately not prefixed "sod:",
	// because that prefix is what makes recordAndAnswer publish
	// sod.violation.detected — and a suspended principal is not a duty
	// conflict.
	if pub.sodCalls != 0 {
		t.Errorf("sod.violation.detected published %d times, want 0", pub.sodCalls)
	}
	if pub.grantedCalls != 0 {
		t.Errorf("authorization.granted published %d times, want 0", pub.grantedCalls)
	}
}

// The gate runs BEFORE the grant lookups, so a suspended principal costs one
// read rather than five. Asserted on the argument capture rather than on
// timing.
func TestAuthorize_LayerZeroShortCircuitsTheGrantLookups(t *testing.T) {
	store := &stubStore{principalStatus: "SUSPENDED", rbacActions: []string{"PAYMENT_APPROVE"}}
	if w := authorizeAs(t, store, &stubPublisher{}); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if store.grantedTenantArg != "" {
		t.Error("FindGrantedActions was called for a principal already known to be suspended — no grant can be exercised, so the basis would only be a computation nobody is entitled to")
	}
	if store.delegatedTenantArg != "" {
		t.Error("FindDelegatedActions was called for a suspended principal")
	}
}

// The gate is asked with the RESOLVED tenant scope, not the raw body tenant.
// This is the exact defect the own-object SoD check carried and that the fifth
// pass fixed there: a caller that correctly forwards X-Tenant-Id and omits the
// body field would otherwise have this read run with an empty tenant, which
// takes the tenantless most-restrictive branch instead of its own tenant's row.
func TestAuthorize_LayerZeroUsesTheResolvedTenantScope(t *testing.T) {
	store := &stubStore{rbacActions: []string{"PAYMENT_APPROVE"}, rbacBasis: "rbac:role=X"}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	// Header only, no tenant_id in the body — the convention
	// resolveTenantScope exists to encourage.
	body := `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", "t-header")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(store.gotPrincipalStatusArgs) != 2 {
		t.Fatal("FindPrincipalStatus was not called")
	}
	if got := store.gotPrincipalStatusArgs[1]; got != "t-header" {
		t.Fatalf("FindPrincipalStatus tenant = %q, want t-header — the resolved scope, not the body's (empty) one", got)
	}
	if got := store.gotPrincipalStatusArgs[0]; got != "p-1" {
		t.Fatalf("FindPrincipalStatus principal = %q, want p-1", got)
	}
}

// The store being unreachable on layer 0 is a 503, not a denial — the same
// fail-closed-but-distinguishable posture every other layer takes. "Cannot
// evaluate" and "evaluated and denied" must not be the same answer to a caller.
func TestAuthorize_LayerZeroStoreErrorIs503NotDenial(t *testing.T) {
	store := &stubStore{principalStatusErr: domain.ErrStoreUnavailable, rbacActions: []string{"PAYMENT_APPROVE"}}
	w := authorizeAs(t, store, &stubPublisher{})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if store.recordedParams.Outcome != "" {
		t.Error("a decision was recorded despite the store being unreachable — no decision was made, so there is nothing to record")
	}
}

// An explicitly ACTIVE projected status behaves identically to no row.
func TestAuthorize_ActiveStatusGrantsNormally(t *testing.T) {
	store := &stubStore{
		principalStatus: domain.PrincipalStatusActive,
		rbacActions:     []string{"PAYMENT_APPROVE"},
		rbacBasis:       "rbac:role=FINANCE_APPROVER",
	}
	w := authorizeAs(t, store, &stubPublisher{})

	outcome, basis := decodeDecision(t, w)
	if outcome != "GRANTED" {
		t.Fatalf("outcome = %s basis = %s, want GRANTED", outcome, basis)
	}
	if basis != "rbac:role=FINANCE_APPROVER" {
		t.Errorf("basis = %q — an ACTIVE status must not overwrite the granting layer's basis", basis)
	}
}

// The three validation routes deliberately do NOT carry the layer-0 gate. They
// answer questions about the shape of the grant graph — "what does this
// principal hold", "would this be grantable" — and those answers are unchanged
// by whether the principal is currently suspended. Gating them would tell an
// operator that a suspended employee holds nothing, which is false and is
// exactly the wrong thing to show somebody deciding what to revoke.
func TestValidationRoutesDoNotConsultPrincipalStatus(t *testing.T) {
	cases := []struct{ path, body string }{
		{handler.EntityScopeValidatePath, `{"principal_id":"p-1","legal_entity_ids":["le-1"]}`},
		{handler.SoDValidatePath, `{"principal_id":"p-1","legal_entity_id":"le-1","candidate_actions":["PAYMENT_APPROVE"]}`},
		{handler.DelegatedAccessEvaluatePath, `{"principal_id":"p-1","legal_entity_id":"le-1"}`},
	}
	for _, c := range cases {
		store := &stubStore{principalStatus: "SUSPENDED", rbacActions: []string{"PAYMENT_APPROVE"}}
		r := newTestRouter(store)
		w := postJSON(t, r, c.path, c.body, adminHeaders())
		if w.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d: %s", c.path, w.Code, w.Body.String())
		}
		if store.principalStatusCallCount != 0 {
			t.Errorf("%s: consulted principal status %d times — these routes report what the grant graph holds, not what may be exercised right now",
				c.path, store.principalStatusCallCount)
		}
	}
}
