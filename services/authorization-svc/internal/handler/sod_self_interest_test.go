package handler_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
)

// The audit's scenario: a principal switches off the SoD rule that blocks
// them. iam.sod_rule.manage gates the route; these tests cover a holder of
// that permission who also holds an action the rule constrains.

func sodStore(heldActions ...string) *stubStore {
	s := &stubStore{
		listSoDRules: []domain.SoDRule{{SoDRuleID: "s-1", ActionA: "PAYMENT_APPROVE", ActionB: "PAYMENT_INITIATE", ActiveFlag: true}},
	}
	if len(heldActions) > 0 {
		s.listAssignments = []domain.PrincipalRoleAssignment{{PrincipalRoleAssignmentID: "a-1", PrincipalID: "admin-1", RoleID: "r-9"}}
		s.bundles = []domain.PermissionBundle{{PermissionBundleID: "b-9", RoleID: "r-9", PermittedActions: heldActions}}
	}
	return s
}

func TestSoDRetire_HolderOfBothActionsRefused(t *testing.T) {
	s := sodStore("PAYMENT_APPROVE", "PAYMENT_INITIATE")
	w := postAsAdmin(s, "/v1/admin/sod-rules/s-1/retire", `{}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("holder of both actions retiring the rule: want 403, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotSoDActiveID != "" {
		t.Error("SetSoDRuleActive was reached despite the refusal")
	}
}

func TestSoDRetire_HolderOfOneActionRefused(t *testing.T) {
	for _, act := range []string{"PAYMENT_APPROVE", "PAYMENT_INITIATE"} {
		if w := postAsAdmin(sodStore(act), "/v1/admin/sod-rules/s-1/retire", `{}`); w.Code != http.StatusForbidden {
			t.Errorf("holder of %s: want 403, got %d", act, w.Code)
		}
	}
}

func TestSoDRetire_EndedAssignmentDoesNotCount(t *testing.T) {
	s := sodStore("PAYMENT_APPROVE")
	ended := time.Now().Add(-time.Hour)
	s.listAssignments[0].EffectiveTo = &ended
	if w := postAsAdmin(s, "/v1/admin/sod-rules/s-1/retire", `{}`); w.Code != http.StatusOK {
		t.Fatalf("ended assignment: want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSoDRetire_DisinterestedAdminAllowed(t *testing.T) {
	s := sodStore("REPORT_VIEW")
	if w := postAsAdmin(s, "/v1/admin/sod-rules/s-1/retire", `{}`); w.Code != http.StatusOK {
		t.Fatalf("admin holding neither action: want 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotSoDActiveID != "s-1" || s.gotSoDActiveValue {
		t.Errorf("SetSoDRuleActive got id=%q active=%v", s.gotSoDActiveID, s.gotSoDActiveValue)
	}
}

// Reactivating only restores a denial, so an interested party may do it.
func TestSoDReactivate_HolderAllowed(t *testing.T) {
	if w := postAsAdmin(sodStore("PAYMENT_APPROVE", "PAYMENT_INITIATE"), "/v1/admin/sod-rules/s-1/reactivate", `{}`); w.Code != http.StatusOK {
		t.Fatalf("reactivate by holder: want 200, got %d", w.Code)
	}
}

func TestSoDRetire_LookupFailureRefuses(t *testing.T) {
	s := sodStore("PAYMENT_APPROVE")
	s.listAssignmentsErr = errors.New("db down")
	if w := postAsAdmin(s, "/v1/admin/sod-rules/s-1/retire", `{}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("lookup failure: want 503, got %d", w.Code)
	}
}
