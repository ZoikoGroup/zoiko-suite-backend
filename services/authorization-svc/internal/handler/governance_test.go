package handler_test

import (
	"bytes"
	"context"
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

const govTenant = "11111111-1111-4111-8111-111111111111"

// govStore is stubStore with assignment filtering and per-role bundles, and it
// captures the audit context the store would install on the transaction.
type govStore struct {
	*stubStore
	roleBundles map[string][]domain.PermissionBundle
	gotReason   string
	gotActor    string
}

func (s *govStore) ListRoleAssignments(_ context.Context, _, principalID, roleID string, _ bool) ([]domain.PrincipalRoleAssignment, error) {
	var out []domain.PrincipalRoleAssignment
	for _, a := range s.listAssignments {
		if (principalID == "" || a.PrincipalID == principalID) && (roleID == "" || a.RoleID == roleID) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *govStore) ListPermissionBundles(_ context.Context, roleID, _ string) ([]domain.PermissionBundle, error) {
	return s.roleBundles[roleID], nil
}

func (s *govStore) SetRoleActive(ctx context.Context, id, tenant string, active bool, v int64) (*domain.Role, error) {
	s.gotActor, _, s.gotReason = domain.AuditFrom(ctx)
	return s.stubStore.SetRoleActive(ctx, id, tenant, active, v)
}

func newGovStore(callerActions ...string) *govStore {
	return &govStore{
		stubStore: &stubStore{
			denyAdmin:     true,
			rbacActions:   callerActions,
			role:          &domain.Role{RoleID: "r-1", TenantID: govTenant, RoleScopeType: "TENANT"},
			setActiveRole: &domain.Role{RoleID: "r-1", TenantID: govTenant},
			assignment:    &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", RoleID: "r-1"},
			bundle:        &domain.PermissionBundle{PermissionBundleID: "b-1", RoleID: "r-1"},
			bundleCreated: true,
		},
		roleBundles: map[string][]domain.PermissionBundle{},
	}
}

func govRouter(s handler.AuthorizationStore, mode handler.CommandContractMode) chi.Router {
	h := handler.New(s, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), platformID, false, zap.NewNop())
	h.SetCommandContract(mode)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)
	return r
}

func govPost(r chi.Router, caller, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", caller)
	req.Header.Set("X-Tenant-Id", govTenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// stubStore grants rbacActions at the tenant scope only when legal entity ==
// tenant; FindGrantedActions answers with rbacActions everywhere else too.
const assignBody = `{"principal_id":"target-1","role_id":"r-1","legal_entity_id":"22222222-2222-4222-8222-222222222222","effective_from":"2026-01-01T00:00:00Z"}`

// ── maker-checker (ZS-IAM-001 §9 / A20, GOV-12) ──────────────────────────────

func TestMakerChecker_PrivilegedRoleByOrdinaryAdminIsPending(t *testing.T) {
	s := newGovStore("iam.assignment.grant")
	s.roleBundles["r-1"] = []domain.PermissionBundle{{RoleID: "r-1", PermittedActions: []string{"iam.role.manage"}, ActiveFlag: true}}
	w := govPost(govRouter(s, handler.CommandContractWarn), "admin-1", "/v1/admin/role-assignments", assignBody)
	if w.Code != http.StatusAccepted || s.gotCreateAssignment.ApprovalStatus != domain.ApprovalPending {
		t.Fatalf("privileged role by an admin without approve_privileged: want 202 PENDING_APPROVAL, got %d %+v", w.Code, s.gotCreateAssignment)
	}
}

func TestMakerChecker_PrivilegedRoleBySecurityApproverIsApproved(t *testing.T) {
	s := newGovStore("iam.assignment.grant", "iam.assignment.approve_privileged")
	s.roleBundles["r-1"] = []domain.PermissionBundle{{RoleID: "r-1", PermittedActions: []string{"iam.role.manage"}, ActiveFlag: true}}
	w := govPost(govRouter(s, handler.CommandContractWarn), "approver-1", "/v1/admin/role-assignments", assignBody)
	if w.Code != http.StatusCreated || s.gotCreateAssignment.ApprovalStatus != domain.ApprovalApproved ||
		s.gotCreateAssignment.ApprovedBy == nil || *s.gotCreateAssignment.ApprovedBy != "approver-1" {
		t.Fatalf("privileged role by the security approver: want 201 APPROVED by approver-1, got %d %+v", w.Code, s.gotCreateAssignment)
	}
}

func TestMakerChecker_OrdinaryRoleUnaffected(t *testing.T) {
	s := newGovStore("iam.assignment.grant")
	s.roleBundles["r-1"] = []domain.PermissionBundle{{RoleID: "r-1", PermittedActions: []string{"invoice.approve"}, ActiveFlag: true}}
	if w := govPost(govRouter(s, handler.CommandContractWarn), "admin-1", "/v1/admin/role-assignments", assignBody); w.Code != http.StatusCreated {
		t.Fatalf("ordinary role: want 201, got %d", w.Code)
	}
	if s.gotCreateAssignment.ApprovalStatus != "" {
		t.Errorf("ordinary role should need no checker: %+v", s.gotCreateAssignment)
	}
}

func pendingAssignment(maker string) *domain.PrincipalRoleAssignment {
	exp := time.Now().Add(time.Hour)
	return &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", PrincipalID: "target-1", RoleID: "r-1",
		AssignedBy: maker, ApprovalStatus: domain.ApprovalPending, ApprovalExpiresAt: &exp}
}

func TestMakerChecker_CheckerMustBeIndependentAndEntitled(t *testing.T) {
	cases := []struct {
		name, caller string
		actions      []string
		want         int
	}{
		{"maker approves own request", "maker-1", []string{"iam.assignment.approve_privileged"}, http.StatusForbidden},
		{"target approves own elevation", "target-1", []string{"iam.assignment.approve_privileged"}, http.StatusForbidden},
		{"checker without approve_privileged", "other-1", []string{"iam.assignment.grant"}, http.StatusForbidden},
		{"independent entitled checker", "checker-1", []string{"iam.assignment.approve_privileged"}, http.StatusOK},
	}
	for _, c := range cases {
		s := newGovStore(c.actions...)
		s.findAssignment = pendingAssignment("maker-1")
		w := govPost(govRouter(s, handler.CommandContractWarn), c.caller, "/v1/admin/role-assignments/a-1/approve", `{"reason_code":"ACCESS_REVIEWED"}`)
		if w.Code != c.want {
			t.Errorf("%s: want %d, got %d: %s", c.name, c.want, w.Code, w.Body.String())
		}
		if c.want == http.StatusOK && (s.gotDecision != domain.ApprovalApproved || s.gotDecider != "checker-1") {
			t.Errorf("%s: decision not recorded: %q by %q", c.name, s.gotDecision, s.gotDecider)
		}
	}
}

func TestMakerChecker_ExpiredOrDecidedIs409(t *testing.T) {
	s := newGovStore("iam.assignment.approve_privileged")
	s.findAssignment = pendingAssignment("maker-1")
	s.decideErr = domain.ErrApprovalExpired
	if w := govPost(govRouter(s, handler.CommandContractWarn), "checker-1", "/v1/admin/role-assignments/a-1/approve", `{}`); w.Code != http.StatusConflict {
		t.Errorf("expired window: want 409, got %d", w.Code)
	}
	s = newGovStore("iam.assignment.approve_privileged")
	done := pendingAssignment("maker-1")
	done.ApprovalStatus = domain.ApprovalApproved
	s.findAssignment = done
	if w := govPost(govRouter(s, handler.CommandContractWarn), "checker-1", "/v1/admin/role-assignments/a-1/approve", `{}`); w.Code != http.StatusConflict {
		t.Errorf("already decided (replay): want 409, got %d", w.Code)
	}
}

// ── §9: protected platform-admin permissions stay out of tenant roles ────────

func TestProtectedPlatformPermission_TenantAdminRefused(t *testing.T) {
	s := newGovStore() // tenant admin via the stub default
	s.denyAdmin = false
	w := govPost(govRouter(s, handler.CommandContractWarn), "admin-1", "/v1/admin/roles/r-1/permission-bundles",
		`{"bundle_code":"X","permitted_actions":["SOD_RULE_MANAGE_GLOBAL"]}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("tenant admin putting a platform-admin permission in a tenant role: want 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProtectedPlatformPermission_PlatformAdminAllowed(t *testing.T) {
	s := newGovStore("iam.permission_bundle.manage") // held at platform scope too
	s.denyAdmin = false
	w := govPost(govRouter(s, handler.CommandContractWarn), "platform-admin", "/v1/admin/roles/r-1/permission-bundles",
		`{"bundle_code":"X","permitted_actions":["SOD_RULE_MANAGE_GLOBAL"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("platform admin: want 201, got %d: %s", w.Code, w.Body.String())
	}
}

// ── GOV-04: a role change cannot hand its holders a toxic combination ───────

func TestToxicCombination_BundleRefusedForHolders(t *testing.T) {
	inner := newGovStore()
	inner.denyAdmin = false
	inner.listAssignments = []domain.PrincipalRoleAssignment{
		{PrincipalRoleAssignmentID: "a-h", PrincipalID: "holder-1", RoleID: "r-1"},
		{PrincipalRoleAssignmentID: "a-p", PrincipalID: "holder-1", RoleID: "r-prep"},
	}
	inner.roleBundles["r-prep"] = []domain.PermissionBundle{{RoleID: "r-prep", PermittedActions: []string{"payment.prepare"}, ActiveFlag: true}}
	s := &toxicStore{govStore: inner, pairs: [][2]string{{"payment.prepare", "payment.release"}}}
	w := govPost(govRouter(s, handler.CommandContractWarn), "admin-1", "/v1/admin/roles/r-1/permission-bundles",
		`{"bundle_code":"REL","permitted_actions":["payment.release"]}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "holder-1") {
		t.Fatalf("bundle giving a holder the prepare/release pair: want 409 naming holder-1, got %d %s", w.Code, w.Body.String())
	}
}

type toxicStore struct {
	*govStore
	pairs [][2]string
}

func (s *toxicStore) CheckSoDConflict(_ context.Context, others []string, candidate, _ string) (string, bool, error) {
	for _, p := range s.pairs {
		for _, o := range others {
			if (p[0] == candidate && p[1] == o) || (p[1] == candidate && p[0] == o) {
				return o, true, nil
			}
		}
	}
	return "", false, nil
}

// ── §16: purpose / reason_code on destructive commands ──────────────────────

func TestReasonCode_EnforceRefusesWithout(t *testing.T) {
	s := newGovStore()
	s.denyAdmin = false
	if w := govPost(govRouter(s, handler.CommandContractEnforce), "admin-1", "/v1/admin/roles/r-1/retire", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("enforce, no reason_code: want 400, got %d", w.Code)
	}
}

func TestReasonCode_WarnAdmitsAndMarks(t *testing.T) {
	s := newGovStore()
	s.denyAdmin = false
	w := govPost(govRouter(s, handler.CommandContractWarn), "admin-1", "/v1/admin/roles/r-1/retire", `{}`)
	if w.Code != http.StatusOK || w.Header().Get(handler.HeaderCommandContract) != "violated" {
		t.Fatalf("warn, no reason_code: want 200 marked violated, got %d %q", w.Code, w.Header().Get(handler.HeaderCommandContract))
	}
}

func TestReasonCode_RecordedWithTheChange(t *testing.T) {
	s := newGovStore()
	s.denyAdmin = false
	w := govPost(govRouter(s, handler.CommandContractEnforce), "admin-1", "/v1/admin/roles/r-1/retire",
		`{"reason_code":"ROLE_DECOMMISSIONED","purpose":"finance restructure"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotReason != "ROLE_DECOMMISSIONED: finance restructure" || s.gotActor != "admin-1" {
		t.Errorf("audit context reaching the store: actor=%q reason=%q", s.gotActor, s.gotReason)
	}
}

// ── §11: delegation rules ───────────────────────────────────────────────────

func delegationBody(extra string) string {
	return `{"delegator_principal_id":"boss-1","delegate_principal_id":"cover-1","scope_type":"FULL","effective_from":"2026-10-01T00:00:00Z"` + extra + `}`
}

func TestDelegation_ProtectedPrivilegeNotDelegable(t *testing.T) {
	s := newGovStore()
	s.denyAdmin = false
	w := govPost(govRouter(s, handler.CommandContractWarn), "boss-1", "/v1/admin/delegated-authorities",
		delegationBody(`,"scope_type":"ACTION_SUBSET","delegated_actions":["iam.assignment.grant"],"effective_to":"2026-10-08T00:00:00Z","reason":"leave"`))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "protected_privilege_not_delegable") {
		t.Fatalf("delegating iam.assignment.grant: want 400, got %d %s", w.Code, w.Body.String())
	}
}

func TestDelegation_FinitePeriod(t *testing.T) {
	s := newGovStore()
	s.denyAdmin = false
	if w := govPost(govRouter(s, handler.CommandContractEnforce), "boss-1", "/v1/admin/delegated-authorities", delegationBody(`,"reason":"leave"`)); w.Code != http.StatusBadRequest {
		t.Errorf("enforce, open-ended delegation: want 400, got %d", w.Code)
	}
	s = newGovStore()
	s.denyAdmin = false
	s.delegation = &domain.DelegatedAuthority{DelegatedAuthorityID: "da-1"}
	w := govPost(govRouter(s, handler.CommandContractWarn), "boss-1", "/v1/admin/delegated-authorities", delegationBody(`,"reason":"leave"`))
	if w.Code != http.StatusCreated {
		t.Fatalf("warn, open-ended delegation: want 201, got %d %s", w.Code, w.Body.String())
	}
	end := s.gotCreateDelegation.EffectiveTo
	if end == nil || !end.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(handler.DefaultDelegationTerm)) {
		t.Errorf("warn mode should bound the delegation to the default term, got %v", end)
	}
	if s.gotCreateDelegation.Reason == nil || *s.gotCreateDelegation.Reason != "leave" {
		t.Errorf("reason not recorded: %+v", s.gotCreateDelegation.Reason)
	}
}
