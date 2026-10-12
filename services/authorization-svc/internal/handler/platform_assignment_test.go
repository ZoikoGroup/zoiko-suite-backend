package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// An assignment on the platform-scope entity is what requirePlatformAction
// reads, across tenants. Before this, a tenant administrator could bind a role
// of their own tenant to that entity and its holder could author SoD and ABAC
// rules for every tenant. Making or ending one now needs a platform-scope
// grant, and a tenant-scope iam.assignment.grant is not one.

const (
	platformID     = "00000000-0000-0000-0000-00000000f001"
	platformTenant = "11111111-1111-4111-8111-111111111111"
)

func platformRouter(s *stubStore) chi.Router {
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubValidator{},
		siem.New("", "authorization-svc", zap.NewNop()), platformID, false, zap.NewNop()))
	return r
}

func platformStore() *stubStore {
	return &stubStore{
		role:          &domain.Role{RoleID: "r-1", TenantID: platformTenant, RoleScopeType: "LEGAL_ENTITY"},
		assignment:    &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", RoleID: "r-1"},
		revokedAssign: &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", RoleID: "r-1"},
	}
}

func platformPost(s *stubStore, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "tenant-admin")
	req.Header.Set("X-Tenant-Id", platformTenant)
	w := httptest.NewRecorder()
	platformRouter(s).ServeHTTP(w, req)
	return w
}

func assignOn(entity string) string {
	return `{"principal_id":"colleague","role_id":"r-1","legal_entity_id":"` + entity + `","effective_from":"2026-01-01T00:00:00Z"}`
}

// The audit's escalation: a tenant administrator (tenant-scope grants only)
// assigns a tenant role on the platform entity. Every textual form Postgres
// stores as the same UUID is refused, not only the canonical one.
func TestPlatformAssignment_TenantAdminRefused(t *testing.T) {
	for _, entity := range []string{
		platformID,
		"00000000-0000-0000-0000-00000000F001",
		"{00000000-0000-0000-0000-00000000f001}",
		"0000000000000000000000000000f001",
		"urn:uuid:00000000-0000-0000-0000-00000000f001",
	} {
		s := platformStore()
		w := platformPost(s, "/v1/admin/role-assignments", assignOn(entity))
		if w.Code != http.StatusForbidden {
			t.Errorf("legal_entity_id %q by a tenant-only admin: want 403, got %d: %s", entity, w.Code, w.Body.String())
			continue
		}
		if s.recordedParams.LegalEntityID != platformID || s.recordedParams.ActionType != "iam.assignment.grant" || s.recordedParams.Outcome != "DENIED" {
			t.Errorf("%q: the platform check was not the one that refused: %+v", entity, s.recordedParams)
		}
	}
}

// A platform-scope assignment is privileged: it takes effect only with an
// independent approver (GOV-12). A platform admin holding the approval
// permission is that approver; one holding only the grant permission makes a
// PENDING request a second principal must approve.
func TestPlatformAssignment_PlatformApproverAllowed(t *testing.T) {
	s := platformStore()
	s.rbacActions = []string{"iam.assignment.grant", "iam.assignment.approve_privileged"} // held at platform scope
	if w := platformPost(s, "/v1/admin/role-assignments", assignOn(platformID)); w.Code != http.StatusCreated {
		t.Fatalf("platform approver: want 201, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotCreateAssignment.ApprovalStatus != "APPROVED" || s.gotCreateAssignment.ApprovedBy == nil {
		t.Errorf("approved grant not recorded with its approver: %+v", s.gotCreateAssignment)
	}
}

func TestPlatformAssignment_PlatformAdminWithoutApprovalIsPending(t *testing.T) {
	s := platformStore()
	s.rbacActions = []string{"iam.assignment.grant"}
	if w := platformPost(s, "/v1/admin/role-assignments", assignOn(platformID)); w.Code != http.StatusAccepted {
		t.Fatalf("platform admin without approval right: want 202 pending, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotCreateAssignment.ApprovalStatus != "PENDING_APPROVAL" || s.gotCreateAssignment.ApprovalExpiresAt == nil {
		t.Errorf("pending grant not recorded as such: %+v", s.gotCreateAssignment)
	}
}

func TestPlatformAssignment_OrdinaryEntityUnaffected(t *testing.T) {
	if w := platformPost(platformStore(), "/v1/admin/role-assignments", assignOn("22222222-2222-4222-8222-222222222222")); w.Code != http.StatusCreated {
		t.Fatalf("ordinary entity: want 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPlatformAssignment_RevokeByTenantAdminRefused(t *testing.T) {
	s := platformStore()
	pid := platformID
	s.findAssignment = &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", RoleID: "r-1", LegalEntityID: &pid}
	for _, body := range []string{`{}`, `{"effective_to":"2999-01-01T00:00:00Z"}`} {
		if w := platformPost(s, "/v1/admin/role-assignments/a-1/revoke", body); w.Code != http.StatusForbidden {
			t.Errorf("revoke %s of a platform-scope assignment by a tenant admin: want 403, got %d: %s", body, w.Code, w.Body.String())
		}
	}
}

func TestPlatformAssignment_RevokeByPlatformAdminAllowed(t *testing.T) {
	s := platformStore()
	s.rbacActions = []string{"iam.assignment.revoke"}
	pid := platformID
	s.findAssignment = &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", RoleID: "r-1", LegalEntityID: &pid}
	if w := platformPost(s, "/v1/admin/role-assignments/a-1/revoke", `{}`); w.Code != http.StatusOK {
		t.Fatalf("platform admin revoke: want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPlatformAssignment_RevokeUnknown404(t *testing.T) {
	s := platformStore()
	s.findAssignmentErr = domain.ErrRoleAssignmentNotFound
	if w := platformPost(s, "/v1/admin/role-assignments/a-x/revoke", `{}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown assignment: want 404, got %d", w.Code)
	}
}
