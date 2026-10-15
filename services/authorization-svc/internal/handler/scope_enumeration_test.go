package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The audit's scenario: any principal in the tenant could enumerate any other
// principal's permitted_actions through /v1/entity-scope/validate. Asking about
// yourself stays open; asking about somebody else needs iam.assignment.read.

func scopeAsk(s *stubStore, caller, subject string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/entity-scope/validate",
		bytes.NewBufferString(`{"principal_id":"`+subject+`","legal_entity_ids":["22222222-2222-4222-8222-222222222222"]}`))
	req.Header.Set("X-Principal-Id", caller)
	req.Header.Set("X-Tenant-Id", "11111111-1111-4111-8111-111111111111")
	w := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(w, req)
	return w
}

func TestScopeEnumeration_OtherPrincipalWithoutPermissionRefused(t *testing.T) {
	s := &stubStore{denyAdmin: true, rbacActions: []string{"PAYMENT_RELEASE"}}
	w := scopeAsk(s, "clerk-1", "cfo-1")
	if w.Code != http.StatusForbidden {
		t.Fatalf("clerk asking for the CFO's grant map: want 403, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("PAYMENT_RELEASE")) {
		t.Error("the refused response still disclosed the subject's actions")
	}
}

func TestScopeEnumeration_SelfAlwaysAllowed(t *testing.T) {
	s := &stubStore{denyAdmin: true, rbacActions: []string{"PAYMENT_RELEASE"}, rbacBasis: "rbac:role=AP"}
	w := scopeAsk(s, "clerk-1", "clerk-1")
	if w.Code != http.StatusOK {
		t.Fatalf("asking about yourself: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Results []struct {
			PermittedActions []string `json:"permitted_actions"`
		} `json:"results"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Results) != 1 || len(got.Results[0].PermittedActions) == 0 {
		t.Fatalf("self query returned no grant map: %s", w.Body.String())
	}
}

func TestScopeEnumeration_AssignmentReaderAllowed(t *testing.T) {
	s := &stubStore{rbacActions: []string{"PAYMENT_RELEASE"}} // default stub caller holds iam.assignment.read
	if w := scopeAsk(s, "iam-admin", "cfo-1"); w.Code != http.StatusOK {
		t.Fatalf("holder of iam.assignment.read: want 200, got %d: %s", w.Code, w.Body.String())
	}
}
