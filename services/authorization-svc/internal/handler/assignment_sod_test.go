package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// Static SoD at assignment time. The audit's scenario: a conflicting pair could
// be assigned and was caught only later, when the action was evaluated.

// sodRuleStore is stubStore with real pair rules and per-role bundles, so the
// conflict check sees the arguments it is actually given.
type sodRuleStore struct {
	*stubStore
	pairs       [][2]string
	roleBundles map[string][]domain.PermissionBundle
	sodErr      error
}

func (s *sodRuleStore) CheckSoDConflict(_ context.Context, others []string, candidate, _ string) (string, bool, error) {
	if s.sodErr != nil {
		return "", false, s.sodErr
	}
	for _, p := range s.pairs {
		for _, o := range others {
			if (p[0] == candidate && p[1] == o) || (p[1] == candidate && p[0] == o) {
				return o, true, nil
			}
		}
	}
	return "", false, nil
}

func (s *sodRuleStore) ListPermissionBundles(_ context.Context, roleID, _ string) ([]domain.PermissionBundle, error) {
	return s.roleBundles[roleID], nil
}

func newSoDAssignStore(assigneeHolds []string, newRoleActions ...string) *sodRuleStore {
	return &sodRuleStore{
		stubStore: &stubStore{
			role:        &domain.Role{RoleID: "r-new", TenantID: ownRoleTenant, RoleScopeType: "TENANT"},
			assignment:  &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", RoleID: "r-new"},
			rbacActions: assigneeHolds,
		},
		pairs: [][2]string{{"payment.prepare", "payment.release"}},
		roleBundles: map[string][]domain.PermissionBundle{
			"r-new": {{PermissionBundleID: "b-new", RoleID: "r-new", PermittedActions: newRoleActions, ActiveFlag: true}},
		},
	}
}

func assignTo(s *sodRuleStore, entity string) (int, map[string]any) {
	body := `{"principal_id":"preparer-1","role_id":"r-new","effective_from":"2026-01-01T00:00:00Z"` +
		map[bool]string{true: `,"legal_entity_id":"` + entity + `"`, false: ``}[entity != ""] + `}`
	w := postAsAdminTo(s, "/v1/admin/role-assignments", body)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got
}

func TestAssignmentSoD_ConflictWithHeldRefused(t *testing.T) {
	s := newSoDAssignStore([]string{"payment.prepare"}, "payment.release")
	code, got := assignTo(s, "22222222-2222-4222-8222-222222222222")
	if code != http.StatusConflict || got["error"] != "sod_conflict" {
		t.Fatalf("preparer given the release role: want 409 sod_conflict, got %d %v", code, got)
	}
	c := got["conflicts"].([]any)[0].(map[string]any)
	if c["candidate_action"] != "payment.release" || c["conflicts_with"] != "payment.prepare" || c["source"] != "held" {
		t.Errorf("conflict = %v", c)
	}
}

func TestAssignmentSoD_ToxicRoleRefused(t *testing.T) {
	s := newSoDAssignStore(nil, "payment.prepare", "payment.release")
	if code, got := assignTo(s, "22222222-2222-4222-8222-222222222222"); code != http.StatusConflict {
		t.Fatalf("role holding both sides of a rule: want 409, got %d %v", code, got)
	}
}

func TestAssignmentSoD_NoConflictAllowed(t *testing.T) {
	s := newSoDAssignStore([]string{"report.view"}, "payment.release")
	if code, got := assignTo(s, "22222222-2222-4222-8222-222222222222"); code != http.StatusCreated {
		t.Fatalf("no conflicting holding: want 201, got %d %v", code, got)
	}
}

// A retired bundle grants nothing, so it cannot create a conflict.
func TestAssignmentSoD_RetiredBundleIgnored(t *testing.T) {
	s := newSoDAssignStore([]string{"payment.prepare"}, "payment.release")
	s.roleBundles["r-new"][0].ActiveFlag = false
	if code, _ := assignTo(s, "22222222-2222-4222-8222-222222222222"); code != http.StatusCreated {
		t.Fatalf("retired conflicting bundle: want 201, got %d", code)
	}
}

// A tenant-wide assignment applies in every entity, so a conflicting action
// held through any role in the tenant counts.
func TestAssignmentSoD_TenantWideSeesEveryHolding(t *testing.T) {
	s := newSoDAssignStore(nil, "payment.release")
	s.listAssignments = []domain.PrincipalRoleAssignment{{PrincipalRoleAssignmentID: "a-0", PrincipalID: "preparer-1", RoleID: "r-held"}}
	s.roleBundles["r-held"] = []domain.PermissionBundle{{RoleID: "r-held", PermittedActions: []string{"payment.prepare"}, ActiveFlag: true}}
	if code, got := assignTo(s, ""); code != http.StatusConflict {
		t.Fatalf("tenant-wide release role for a preparer elsewhere in the tenant: want 409, got %d %v", code, got)
	}
}

func TestAssignmentSoD_CheckFailureRefuses(t *testing.T) {
	s := newSoDAssignStore(nil, "payment.release")
	s.sodErr = errors.New("db down")
	if code, _ := assignTo(s, "22222222-2222-4222-8222-222222222222"); code != http.StatusServiceUnavailable {
		t.Fatalf("SoD check failure: want 503, got %d", code)
	}
}

func postAsAdminTo(s handler.AuthorizationStore, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubValidator{},
		siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", false, zap.NewNop()))
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", ownRoleTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
