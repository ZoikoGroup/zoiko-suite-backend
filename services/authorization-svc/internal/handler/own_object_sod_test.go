package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// TestAuthorize_OwnObjectSoD_Denied_PublishesSoDEvent is the regression
// test for ZS-IAM-001 §10.2's dynamic Segregation-of-Duties layer: a
// preparer cannot approve their own object. Before this fix, EvaluateParams
// (and the wire request) had no resource-attribute input at all, so there
// was no way for /v1/authorize to even learn who prepared the object being
// acted on — this was a mandatory, non-optional layer with nothing to
// evaluate against.
func TestAuthorize_OwnObjectSoD_Denied_PublishesSoDEvent(t *testing.T) {
	store := &stubStore{
		rbacActions:        []string{"AP_INVOICE_APPROVE"},
		rbacBasis:          "rbac:role=AP_APPROVER",
		ownObjectForbidden: true,
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"AP_INVOICE_APPROVE","resource_owner_principal_id":"p-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["decision_outcome"] != "DENIED" {
		t.Fatalf("expected DENIED for a preparer approving their own object, got %s", got["decision_outcome"])
	}
	if got["decision_basis"] != "sod:own_object_forbidden" {
		t.Fatalf("expected basis sod:own_object_forbidden, got %s", got["decision_basis"])
	}
	if pub.deniedCalls != 1 {
		t.Errorf("expected authorization.denied published once, got %d", pub.deniedCalls)
	}
	if pub.sodCalls != 1 {
		t.Errorf("expected sod.violation.detected published once for the own-object denial, got %d", pub.sodCalls)
	}
}

// TestAuthorize_DifferentOwner_NotBlocked proves the check is scoped to
// the SAME principal preparing and acting: a different preparer must not
// trigger the own-object rule, even when one is configured for the action.
func TestAuthorize_DifferentOwner_NotBlocked(t *testing.T) {
	store := &stubStore{
		rbacActions:        []string{"AP_INVOICE_APPROVE"},
		rbacBasis:          "rbac:role=AP_APPROVER",
		ownObjectForbidden: true,
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"AP_INVOICE_APPROVE","resource_owner_principal_id":"p-2"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["decision_outcome"] != "GRANTED" {
		t.Fatalf("expected GRANTED when the preparer differs from the actor, got %s", got["decision_outcome"])
	}
}

// TestAuthorize_NoResourceOwnerSupplied_RequiresApproval: an own-object rule
// applies to the action and the caller did not say who prepared the object.
// This used to skip the check and GRANT — the audit's "a caller that omits it
// skips the check". GOV-04: "Unknown participation/history on material action
// => block or require independent review; never silently assume no conflict."
func TestAuthorize_NoResourceOwnerSupplied_RequiresApproval(t *testing.T) {
	store := &stubStore{
		rbacActions:        []string{"AP_INVOICE_APPROVE"},
		rbacBasis:          "rbac:role=AP_APPROVER",
		ownObjectForbidden: true,
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"AP_INVOICE_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["decision_outcome"] != "DENIED" || got["decision"] != "REQUIRE_APPROVAL" {
		t.Fatalf("own-object rule with unknown preparer: want DENIED / REQUIRE_APPROVAL, got %v / %v", got["decision_outcome"], got["decision"])
	}
	if got["decision_basis"] != "sod:participation_unknown" {
		t.Errorf("basis = %v", got["decision_basis"])
	}
}

// With no own-object rule for the action, an unknown preparer changes nothing.
func TestAuthorize_NoResourceOwnerSupplied_NoRule_Granted(t *testing.T) {
	store := &stubStore{rbacActions: []string{"AP_INVOICE_APPROVE"}, rbacBasis: "rbac:role=AP_APPROVER"}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})
	body := `{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"AP_INVOICE_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["decision_outcome"] != "GRANTED" || got["decision"] != "PERMIT" {
		t.Fatalf("no own-object rule: want GRANTED / PERMIT, got %v / %v", got["decision_outcome"], got["decision"])
	}
}

// TestAuthorize_OwnObject_StoreUnavailable_FailsClosed proves the new
// layer follows the same fail-closed doctrine as every other store call
// on this path: a lookup failure is a 503, never a silent grant.
func TestAuthorize_OwnObject_StoreUnavailable_FailsClosed(t *testing.T) {
	store := &stubStore{
		rbacActions:  []string{"AP_INVOICE_APPROVE"},
		rbacBasis:    "rbac:role=AP_APPROVER",
		ownObjectErr: domain.ErrStoreUnavailable,
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"AP_INVOICE_APPROVE","resource_owner_principal_id":"p-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on own-object store failure, got %d: %s", w.Code, w.Body.String())
	}
}
