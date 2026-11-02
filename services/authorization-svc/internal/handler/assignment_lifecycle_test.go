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

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// The ORG-06 role half (IAM-13): SuspendAssignment and its resume,
// GetAssignmentsAsOf, and If-Match honoured — not yet required — on the
// assignment mutations (000038).

const lifecycleTenant = "11111111-1111-4111-8111-111111111111"

// lifecycleStore adds suspension and history to the shared stub.
type lifecycleStore struct {
	*stubStore
	suspended, resumed bool
	gotSuspendBy       string
	gotSuspendReason   string
	transitionErr      error
	gotAsOf            time.Time
	gotAsOfPrincipal   string
	asOf               []domain.PrincipalRoleAssignment
}

func (s *lifecycleStore) SuspendRoleAssignment(_ context.Context, id, _, by, reason string) (*domain.PrincipalRoleAssignment, error) {
	if s.transitionErr != nil {
		return nil, s.transitionErr
	}
	s.suspended, s.gotSuspendBy, s.gotSuspendReason = true, by, reason
	now := time.Now()
	return &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: id, SuspendedAt: &now, Version: 3}, nil
}

func (s *lifecycleStore) ResumeRoleAssignment(_ context.Context, id, _ string) (*domain.PrincipalRoleAssignment, error) {
	if s.transitionErr != nil {
		return nil, s.transitionErr
	}
	s.resumed = true
	return &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: id, Version: 4}, nil
}

func (s *lifecycleStore) FindRoleAssignmentsAsOf(_ context.Context, _, principalID string, asOf time.Time) ([]domain.PrincipalRoleAssignment, error) {
	s.gotAsOf, s.gotAsOfPrincipal = asOf, principalID
	return s.asOf, nil
}

func newLifecycleStore(existing domain.PrincipalRoleAssignment) *lifecycleStore {
	return &lifecycleStore{stubStore: &stubStore{
		role:           &domain.Role{RoleID: "r-1", TenantID: lifecycleTenant, RoleScopeType: "LEGAL_ENTITY"},
		findAssignment: &existing,
		revokedAssign:  &existing,
	}}
}

func lifecycleDo(t *testing.T, s handler.AuthorizationStore, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", lifecycleTenant)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", false, zap.NewNop()))
	r.ServeHTTP(w, req)
	return w
}

func suspendedAssignment() domain.PrincipalRoleAssignment {
	at := time.Now().Add(-time.Hour)
	return domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", PrincipalID: "p-1", RoleID: "r-1",
		ApprovalStatus: domain.ApprovalApproved, SuspendedAt: &at, Version: 2}
}

func TestSuspendRoleAssignment_SuspendsWithReasonAndETag(t *testing.T) {
	s := newLifecycleStore(domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", PrincipalID: "p-1", RoleID: "r-1",
		ApprovalStatus: domain.ApprovalApproved, Version: 2})
	w := lifecycleDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/suspend",
		`{"reason_code":"LEAVE","purpose":"extended absence"}`, map[string]string{"If-Match": `"v2"`})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !s.suspended || s.gotSuspendBy != "admin-1" || !strings.Contains(s.gotSuspendReason, "LEAVE") {
		t.Fatalf("store got suspended=%v by=%q reason=%q", s.suspended, s.gotSuspendBy, s.gotSuspendReason)
	}
	if got := w.Header().Get("ETag"); got != `"v3"` {
		t.Fatalf("expected ETag of the new version, got %q", got)
	}
}

func TestSuspendRoleAssignment_StaleIfMatchChangesNothing(t *testing.T) {
	s := newLifecycleStore(domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", PrincipalID: "p-1", RoleID: "r-1",
		ApprovalStatus: domain.ApprovalApproved, Version: 5})
	w := lifecycleDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/suspend", `{"reason_code":"LEAVE"}`,
		map[string]string{"If-Match": `"v4"`})
	if w.Code != http.StatusPreconditionFailed || s.suspended {
		t.Fatalf("expected 412 and no suspension, got %d suspended=%v: %s", w.Code, s.suspended, w.Body.String())
	}
}

func TestSuspendRoleAssignment_WrongStateIsConflict(t *testing.T) {
	s := newLifecycleStore(domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", PrincipalID: "p-1", RoleID: "r-1", Version: 1})
	s.transitionErr = domain.ErrInvalidTransition
	w := lifecycleDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/suspend", `{"reason_code":"LEAVE"}`, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "invalid_transition") {
		t.Fatalf("expected 409 invalid_transition, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSuspendRoleAssignment_RequiresRevokePermission(t *testing.T) {
	s := newLifecycleStore(domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", Version: 1})
	s.denyAdmin = true
	w := lifecycleDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/suspend", `{"reason_code":"LEAVE"}`, nil)
	if w.Code != http.StatusForbidden || s.suspended {
		t.Fatalf("expected 403 and no suspension, got %d suspended=%v", w.Code, s.suspended)
	}
}

func TestResumeRoleAssignment_Resumes(t *testing.T) {
	s := newLifecycleStore(suspendedAssignment())
	w := lifecycleDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/resume", `{"reason_code":"RETURNED"}`, nil)
	if w.Code != http.StatusOK || !s.resumed {
		t.Fatalf("expected 200 and a resume, got %d resumed=%v: %s", w.Code, s.resumed, w.Body.String())
	}
}

func TestResumeRoleAssignment_NotOnOwnAssignment(t *testing.T) {
	a := suspendedAssignment()
	a.PrincipalID = "admin-1"
	s := newLifecycleStore(a)
	w := lifecycleDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/resume", `{"reason_code":"RETURNED"}`, nil)
	if w.Code != http.StatusForbidden || s.resumed {
		t.Fatalf("expected 403 self_grant_not_allowed, got %d resumed=%v: %s", w.Code, s.resumed, w.Body.String())
	}
}

func TestResumeRoleAssignment_NotSuspendedIsConflict(t *testing.T) {
	a := suspendedAssignment()
	a.SuspendedAt = nil
	s := newLifecycleStore(a)
	w := lifecycleDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/resume", `{"reason_code":"RETURNED"}`, nil)
	if w.Code != http.StatusConflict || s.resumed {
		t.Fatalf("expected 409, got %d resumed=%v: %s", w.Code, s.resumed, w.Body.String())
	}
}

// A privileged role (IAM administration) comes back only with the
// independent-approval permission, as its grant needed it.
func TestResumeRoleAssignment_PrivilegedRoleNeedsApprovePrivileged(t *testing.T) {
	s := newLifecycleStore(suspendedAssignment())
	s.bundles = []domain.PermissionBundle{{PermissionBundleID: "b-1", RoleID: "r-1", ActiveFlag: true, PermittedActions: []string{"iam.role.manage"}}}
	w := lifecycleDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/resume", `{"reason_code":"RETURNED"}`, nil)
	if w.Code != http.StatusForbidden || s.resumed {
		t.Fatalf("expected 403 without %s, got %d resumed=%v: %s", handler.PermissionApprovePrivileged, w.Code, s.resumed, w.Body.String())
	}
}

func TestListRoleAssignments_AsOfReadsHistory(t *testing.T) {
	s := newLifecycleStore(domain.PrincipalRoleAssignment{})
	s.asOf = []domain.PrincipalRoleAssignment{
		{PrincipalRoleAssignmentID: "a-1", PrincipalID: "p-1", RoleID: "r-1"},
		{PrincipalRoleAssignmentID: "a-2", PrincipalID: "p-1", RoleID: "r-2"},
	}
	w := lifecycleDo(t, s, http.MethodGet, "/v1/admin/role-assignments?as_of=2026-03-01T00:00:00Z&principal_id=p-1&role_id=r-2", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC); !s.gotAsOf.Equal(want) || s.gotAsOfPrincipal != "p-1" {
		t.Fatalf("store asked as_of=%v principal=%q", s.gotAsOf, s.gotAsOfPrincipal)
	}
	var got []domain.PrincipalRoleAssignment
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].PrincipalRoleAssignmentID != "a-2" {
		t.Fatalf("expected only a-2 (role filter), got %s", w.Body.String())
	}
}

func TestListRoleAssignments_AsOfRefusesBadInstants(t *testing.T) {
	for name, v := range map[string]string{
		"malformed": "yesterday",
		"future":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	} {
		t.Run(name, func(t *testing.T) {
			s := newLifecycleStore(domain.PrincipalRoleAssignment{})
			w := lifecycleDo(t, s, http.MethodGet, "/v1/admin/role-assignments?as_of="+v, "", nil)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_as_of") {
				t.Fatalf("expected 400 invalid_as_of, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// Revoke and approve take If-Match when it is sent, and still work without
// it: the console and access-control-svc do not send one yet.
func TestRevokeRoleAssignment_IfMatchHonouredNotRequired(t *testing.T) {
	existing := domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", PrincipalID: "p-1", RoleID: "r-1", Version: 7}
	if w := lifecycleDo(t, newLifecycleStore(existing), http.MethodPost, "/v1/admin/role-assignments/a-1/revoke",
		`{"reason_code":"LEAVER"}`, map[string]string{"If-Match": `"v6"`}); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: expected 412, got %d: %s", w.Code, w.Body.String())
	}
	if w := lifecycleDo(t, newLifecycleStore(existing), http.MethodPost, "/v1/admin/role-assignments/a-1/revoke",
		`{"reason_code":"LEAVER","expected_version":6}`, nil); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale expected_version: expected 412, got %d: %s", w.Code, w.Body.String())
	}
	w := lifecycleDo(t, newLifecycleStore(existing), http.MethodPost, "/v1/admin/role-assignments/a-1/revoke", `{"reason_code":"LEAVER"}`, nil)
	if w.Code != http.StatusOK || w.Header().Get("ETag") != `"v7"` {
		t.Fatalf("no If-Match: expected 200 with ETag, got %d %q: %s", w.Code, w.Header().Get("ETag"), w.Body.String())
	}
}

func TestApproveRoleAssignment_StaleIfMatchIsRefused(t *testing.T) {
	s := newLifecycleStore(domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", PrincipalID: "p-1", RoleID: "r-1",
		AssignedBy: "maker-1", ApprovalStatus: domain.ApprovalPending, Version: 2})
	w := lifecycleDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/approve", `{"reason_code":"CLEARED"}`,
		map[string]string{"If-Match": `"v1"`})
	if w.Code != http.StatusPreconditionFailed || s.gotDecision != "" {
		t.Fatalf("expected 412 before any decision, got %d decision=%q: %s", w.Code, s.gotDecision, w.Body.String())
	}
}
