package handler_test

import (
	"bytes"
	"context"
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

// ── stub store ────────────────────────────────────────────────────────────────

type stubStore struct {
	role        *domain.Role
	roleCreated bool
	roleErr     error
	findRoleErr error // FindRoleByID reuses `role` for its result; set this to override with a not-found/other error

	// setActive records what SetRoleActive was asked for, so a test can assert
	// that /retire asks for false and /reactivate for true rather than only
	// that the call happened.
	setActiveRole   *domain.Role
	setActiveErr    error
	setActiveCalls  int
	setActiveWanted []bool

	bundle    *domain.PermissionBundle
	bundleErr error

	assignment      *domain.PrincipalRoleAssignment
	assignmentErr   error
	revokedAssign   *domain.PrincipalRoleAssignment
	revokeAssignErr error

	delegation          *domain.DelegatedAuthority
	delegationErr       error
	findDelegation      *domain.DelegatedAuthority // pre-revoke fetch result for RevokeDelegatedAuthority's ownership check
	findDelegationErr   error
	revokedDelegation   *domain.DelegatedAuthority
	revokeDelegationErr error

	sodRule    *domain.SoDRule
	sodRuleErr error

	rbacActions []string
	rbacBasis   string
	rbacErr     error

	delegatedActions []string
	delegatedBasis   string
	delegatedErr     error

	// The tenant each lookup was actually called with. The whole point of
	// scoping FindGrantedActions is that the handler forwards its VERIFIED
	// tenant scope rather than leaving the store to evaluate platform-wide,
	// and that is only observable from the argument.
	grantedTenantArg   string
	delegatedTenantArg string

	sodConflictAction string
	sodHasConflict    bool
	sodErr            error

	decision        *domain.AccessDecisionLog
	recordErr       error
	findDecision    *domain.AccessDecisionLog
	findDecisionErr error
}

func (s *stubStore) CreateRole(_ context.Context, _ domain.CreateRoleParams) (*domain.Role, bool, error) {
	return s.role, s.roleCreated, s.roleErr
}
func (s *stubStore) FindRoleByID(_ context.Context, _ string) (*domain.Role, error) {
	return s.role, s.findRoleErr
}
func (s *stubStore) SetRoleActive(_ context.Context, _, _ string, active bool) (*domain.Role, error) {
	s.setActiveCalls++
	s.setActiveWanted = append(s.setActiveWanted, active)
	return s.setActiveRole, s.setActiveErr
}
func (s *stubStore) CreatePermissionBundle(_ context.Context, _ domain.CreatePermissionBundleParams) (*domain.PermissionBundle, error) {
	return s.bundle, s.bundleErr
}
func (s *stubStore) CreateRoleAssignment(_ context.Context, _ domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error) {
	return s.assignment, s.assignmentErr
}
func (s *stubStore) RevokeRoleAssignment(_ context.Context, _, _ string) (*domain.PrincipalRoleAssignment, error) {
	return s.revokedAssign, s.revokeAssignErr
}
func (s *stubStore) CreateDelegatedAuthority(_ context.Context, _ domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error) {
	return s.delegation, s.delegationErr
}
func (s *stubStore) FindDelegatedAuthorityByID(_ context.Context, _, _ string) (*domain.DelegatedAuthority, error) {
	return s.findDelegation, s.findDelegationErr
}
func (s *stubStore) RevokeDelegatedAuthority(_ context.Context, _, _ string) (*domain.DelegatedAuthority, error) {
	return s.revokedDelegation, s.revokeDelegationErr
}
func (s *stubStore) CreateSoDRule(_ context.Context, _ domain.CreateSoDRuleParams) (*domain.SoDRule, error) {
	return s.sodRule, s.sodRuleErr
}
func (s *stubStore) FindGrantedActions(_ context.Context, _, _, tenantID string) ([]string, string, error) {
	s.grantedTenantArg = tenantID
	return s.rbacActions, s.rbacBasis, s.rbacErr
}
func (s *stubStore) FindDelegatedActions(_ context.Context, _, _, tenantID string) ([]string, string, error) {
	s.delegatedTenantArg = tenantID
	return s.delegatedActions, s.delegatedBasis, s.delegatedErr
}
func (s *stubStore) CheckSoDConflict(_ context.Context, _ []string, _, _ string) (string, bool, error) {
	return s.sodConflictAction, s.sodHasConflict, s.sodErr
}
func (s *stubStore) RecordAccessDecision(_ context.Context, _ domain.RecordAccessDecisionParams) (*domain.AccessDecisionLog, error) {
	if s.decision != nil {
		return s.decision, s.recordErr
	}
	return &domain.AccessDecisionLog{AccessDecisionID: "d-1", DecisionOutcome: "GRANTED", DecisionBasis: "test"}, s.recordErr
}
func (s *stubStore) FindAccessDecisionByID(_ context.Context, _, _ string) (*domain.AccessDecisionLog, error) {
	return s.findDecision, s.findDecisionErr
}

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct {
	grantedCalls int
	deniedCalls  int
	sodCalls     int
}

func (p *stubPublisher) PublishAuthorizationGranted(_ context.Context, _ domain.AccessDecisionLog) error {
	p.grantedCalls++
	return nil
}
func (p *stubPublisher) PublishAuthorizationDenied(_ context.Context, _ domain.AccessDecisionLog) error {
	p.deniedCalls++
	return nil
}
func (p *stubPublisher) PublishSoDViolationDetected(_ context.Context, _ domain.AccessDecisionLog, _ string) error {
	p.sodCalls++
	return nil
}

// ── stub jurisdiction validator ──────────────────────────────────────────────

type stubValidator struct{ err error }

func (v *stubValidator) ValidateExists(_ context.Context, _ string) error { return v.err }

func newTestRouter(s *stubStore) chi.Router {
	return newTestRouterFull(s, &stubPublisher{}, &stubValidator{})
}

func newTestRouterFull(s *stubStore, p *stubPublisher, v *stubValidator) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, p, v, siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// ── Authorize ────────────────────────────────────────────────────────────────

func TestAuthorize_RBACGrant_NoConflict_Granted(t *testing.T) {
	store := &stubStore{
		rbacActions:    []string{"PAYMENT_APPROVE"},
		rbacBasis:      "rbac:role=FINANCE_APPROVER",
		sodHasConflict: false,
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["decision_outcome"] != "GRANTED" {
		t.Errorf("expected GRANTED, got %s", got["decision_outcome"])
	}
	if pub.grantedCalls != 1 {
		t.Errorf("expected authorization.granted published once, got %d", pub.grantedCalls)
	}
}

func TestAuthorize_NoGrant_Denied(t *testing.T) {
	store := &stubStore{rbacActions: nil, delegatedActions: nil}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["decision_outcome"] != "DENIED" {
		t.Errorf("expected DENIED, got %s", got["decision_outcome"])
	}
	if got["decision_basis"] != "no_grant" {
		t.Errorf("expected basis no_grant, got %s", got["decision_basis"])
	}
	if pub.deniedCalls != 1 {
		t.Errorf("expected authorization.denied published once, got %d", pub.deniedCalls)
	}
}

func TestAuthorize_SoDConflict_Denied_PublishesSoDEvent(t *testing.T) {
	store := &stubStore{
		rbacActions:       []string{"PAYMENT_INITIATE", "PAYMENT_APPROVE"},
		rbacBasis:         "rbac:role=SUPER_ROLE",
		sodConflictAction: "PAYMENT_INITIATE",
		sodHasConflict:    true,
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["decision_outcome"] != "DENIED" {
		t.Errorf("expected DENIED due to SoD conflict, got %s", got["decision_outcome"])
	}
	if pub.deniedCalls != 1 {
		t.Errorf("expected authorization.denied published once, got %d", pub.deniedCalls)
	}
	if pub.sodCalls != 1 {
		t.Errorf("expected sod.violation.detected published once, got %d", pub.sodCalls)
	}
}

func TestAuthorize_DelegatedGrant_Granted(t *testing.T) {
	store := &stubStore{
		rbacActions:      nil,
		delegatedActions: []string{"PAYMENT_APPROVE"},
		delegatedBasis:   "delegated:from=p-boss",
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["decision_outcome"] != "GRANTED" {
		t.Errorf("expected GRANTED via delegation, got %s", got["decision_outcome"])
	}
	if got["decision_basis"] != "delegated:from=p-boss" {
		t.Errorf("expected delegated basis, got %s", got["decision_basis"])
	}
}

func TestAuthorize_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"principal_id":"p-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestAuthorize_StoreUnavailable_FailsClosed(t *testing.T) {
	store := &stubStore{rbacErr: domain.ErrStoreUnavailable}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (fail-closed), got %d", w.Code)
	}
	if pub.grantedCalls != 0 || pub.deniedCalls != 0 {
		t.Errorf("expected no publish when evaluation could not complete, got granted=%d denied=%d", pub.grantedCalls, pub.deniedCalls)
	}
}

// ── CreateRole ───────────────────────────────────────────────────────────────

func TestCreateRole_Created(t *testing.T) {
	store := &stubStore{role: &domain.Role{RoleID: "r-1", RoleCode: "FINANCE_APPROVER"}, roleCreated: true}
	r := newTestRouter(store)

	body := `{"tenant_id":"t-1","role_code":"FINANCE_APPROVER","role_name":"Finance Approver","role_scope_type":"LEGAL_ENTITY"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRole_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestCreateRole_NoPrincipal_Refused proves the fix: before it, this
// route trusted created_by_principal_id from the request body entirely.
func TestCreateRole_NoPrincipal_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"tenant_id":"t-1","role_code":"FINANCE_APPROVER","role_name":"Finance Approver","role_scope_type":"LEGAL_ENTITY"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Principal-Id, got %d", w.Code)
	}
}

// TestCreateRole_ForeignTenantBody_Refused proves the fix: before it,
// any caller could create a role in any tenant just by naming it in the
// body.
func TestCreateRole_ForeignTenantBody_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"tenant_id":"other-tenant","role_code":"FINANCE_APPROVER","role_name":"Finance Approver","role_scope_type":"LEGAL_ENTITY"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 creating a role in another tenant, got %d: %s", w.Code, w.Body.String())
	}
}

// ── CreatePermissionBundle ───────────────────────────────────────────────────
// Had zero test coverage of any kind before this fix, and zero ownership
// check: any caller could grant a permission bundle onto any tenant's
// role just by naming its role_id.

func TestCreatePermissionBundle_Created(t *testing.T) {
	store := &stubStore{
		role:   &domain.Role{RoleID: "r-1", TenantID: "t-1"},
		bundle: &domain.PermissionBundle{PermissionBundleID: "b-1", RoleID: "r-1"},
	}
	r := newTestRouter(store)

	body := `{"bundle_code":"default","permitted_actions":["PAYMENT_APPROVE"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles/r-1/permission-bundles", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreatePermissionBundle_ForeignTenantRole_Refused(t *testing.T) {
	store := &stubStore{role: &domain.Role{RoleID: "r-1", TenantID: "other-tenant"}}
	r := newTestRouter(store)

	body := `{"bundle_code":"default","permitted_actions":["PAYMENT_APPROVE"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles/r-1/permission-bundles", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 404, not 403 — must not confirm another tenant's role exists.
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 granting a bundle onto another tenant's role, got %d: %s", w.Code, w.Body.String())
	}
}

// ── CreateRoleAssignment / RevokeRoleAssignment ─────────────────────────────
// Had zero test coverage of any kind before this fix, and zero ownership
// check: any caller could assign any tenant's role to any principal.

func TestCreateRoleAssignment_Created(t *testing.T) {
	store := &stubStore{
		role:       &domain.Role{RoleID: "r-1", TenantID: "t-1", RoleScopeType: "LEGAL_ENTITY"},
		assignment: &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1"},
	}
	r := newTestRouter(store)

	body := `{"principal_id":"p-1","role_id":"r-1","legal_entity_id":"e-1","effective_from":"2026-01-01T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/role-assignments", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRoleAssignment_ForeignTenantRole_Refused(t *testing.T) {
	store := &stubStore{role: &domain.Role{RoleID: "r-1", TenantID: "other-tenant", RoleScopeType: "LEGAL_ENTITY"}}
	r := newTestRouter(store)

	body := `{"principal_id":"p-1","role_id":"r-1","legal_entity_id":"e-1","effective_from":"2026-01-01T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/role-assignments", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 assigning another tenant's role, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRevokeRoleAssignment_Revoked(t *testing.T) {
	store := &stubStore{revokedAssign: &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1"}}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/role-assignments/a-1/revoke", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRevokeRoleAssignment_NoTenantScope_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/role-assignments/a-1/revoke", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d", w.Code)
	}
}

// ── CreateDelegatedAuthority ─────────────────────────────────────────────────
// Had zero test coverage of any kind before this fix, and zero check
// that the caller IS the delegator: any caller could delegate any
// principal's authority to any other principal.

func TestCreateDelegatedAuthority_Created(t *testing.T) {
	store := &stubStore{delegation: &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1"}}
	r := newTestRouter(store)

	body := `{"delegator_principal_id":"admin-1","delegate_principal_id":"p-2","scope_type":"FULL","effective_from":"2026-01-01T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/delegated-authorities", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateDelegatedAuthority_NotOwnAuthority_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})

	// The caller (admin-1) tries to delegate SOMEONE ELSE's authority
	// (someone-else) to a third principal — the exact escalation this
	// fix closes.
	body := `{"delegator_principal_id":"someone-else","delegate_principal_id":"p-2","scope_type":"FULL","effective_from":"2026-01-01T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/delegated-authorities", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 delegating someone else's authority, got %d: %s", w.Code, w.Body.String())
	}
}

// ── RetireRole / ReactivateRole ───────────────────────────────────────────────
//
// active_flag is the only thing that stops a role granting anything --
// FindGrantedActions joins through it -- so what these assert is not "the
// handler returned 200" but "the handler asked for the flag the route name
// promises". A /retire that set the flag true would still answer 200.

func TestRetireRole_SetsActiveFalse(t *testing.T) {
	store := &stubStore{
		role:          &domain.Role{RoleID: "r-1", RoleCode: "FINANCE_APPROVER", TenantID: "tenant-1", ActiveFlag: true},
		setActiveRole: &domain.Role{RoleID: "r-1", RoleCode: "FINANCE_APPROVER", ActiveFlag: false},
	}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles/r-1/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.setActiveCalls != 1 {
		t.Fatalf("expected exactly one SetRoleActive call, got %d", store.setActiveCalls)
	}
	if store.setActiveWanted[0] != false {
		t.Fatalf("/retire asked for active_flag=%v; retiring a role must set it false, or the role keeps granting every action it grants today", store.setActiveWanted[0])
	}
}

func TestReactivateRole_SetsActiveTrue(t *testing.T) {
	store := &stubStore{
		role:          &domain.Role{RoleID: "r-1", RoleCode: "FINANCE_APPROVER", TenantID: "tenant-1", ActiveFlag: false},
		setActiveRole: &domain.Role{RoleID: "r-1", RoleCode: "FINANCE_APPROVER", ActiveFlag: true},
	}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles/r-1/reactivate", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.setActiveWanted[0] != true {
		t.Fatalf("/reactivate asked for active_flag=%v, expected true", store.setActiveWanted[0])
	}
}

func TestRetireRole_UnknownRoleIs404(t *testing.T) {
	// 404 and not 503: the store reached the database and answered. Collapsing
	// the two would make a typo'd role id look like an outage.
	store := &stubStore{
		role:         &domain.Role{RoleID: "r-1", RoleCode: "FINANCE_APPROVER", TenantID: "tenant-1", ActiveFlag: true},
		setActiveErr: domain.ErrRoleNotFound,
	}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles/does-not-exist/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRetireRole_StoreDownIs503(t *testing.T) {
	store := &stubStore{
		role:         &domain.Role{RoleID: "r-1", RoleCode: "FINANCE_APPROVER", TenantID: "tenant-1", ActiveFlag: true},
		setActiveErr: domain.ErrStoreUnavailable,
	}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles/r-1/retire", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// ── CreateSoDRule with jurisdiction validation ──────────────────────────────

func TestCreateSoDRule_JurisdictionNotFound(t *testing.T) {
	r := newTestRouterFull(&stubStore{}, &stubPublisher{}, &stubValidator{err: domain.ErrJurisdictionNotFound})

	body := `{"domain_code":"FINANCE","action_a":"PAYMENT_INITIATE","action_b":"PAYMENT_APPROVE","conflict_type":"MUTUALLY_EXCLUSIVE","jurisdiction_id":"jur-missing","tenant_id":"tenant-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSoDRule_NoJurisdiction_Created(t *testing.T) {
	store := &stubStore{sodRule: &domain.SoDRule{SoDRuleID: "sod-1"}}
	r := newTestRouter(store)

	body := `{"domain_code":"FINANCE","action_a":"PAYMENT_INITIATE","action_b":"PAYMENT_APPROVE","conflict_type":"MUTUALLY_EXCLUSIVE","tenant_id":"tenant-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCreateSoDRule_ForeignTenantBody_Refused proves the fix: before it,
// any caller could write a SoD rule into any tenant just by naming it.
func TestCreateSoDRule_ForeignTenantBody_Refused(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"domain_code":"FINANCE","action_a":"PAYMENT_INITIATE","action_b":"PAYMENT_APPROVE","conflict_type":"MUTUALLY_EXCLUSIVE","tenant_id":"other-tenant"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules", bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "t-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 writing a SoD rule into another tenant, got %d: %s", w.Code, w.Body.String())
	}
}

// ── RevokeDelegatedAuthority ─────────────────────────────────────────────────

func TestRevokeDelegatedAuthority_AlreadyRevoked(t *testing.T) {
	store := &stubStore{
		findDelegation:      &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1", DelegatorPrincipalID: "admin-1"},
		revokeDelegationErr: domain.ErrInvalidTransition,
	}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/delegated-authorities/d-1/revoke", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// TestRevokeDelegatedAuthority_NotDelegator_Refused proves the fix:
// before it, any caller could revoke any principal's delegation.
func TestRevokeDelegatedAuthority_NotDelegator_Refused(t *testing.T) {
	store := &stubStore{
		findDelegation: &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1", DelegatorPrincipalID: "someone-else"},
	}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/delegated-authorities/d-1/revoke", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 revoking someone else's delegation, got %d: %s", w.Code, w.Body.String())
	}
}

// ── GetAccessDecision ────────────────────────────────────────────────────────

func TestGetAccessDecision_NotFound(t *testing.T) {
	store := &stubStore{findDecisionErr: domain.ErrAccessDecisionNotFound}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/access-decisions/missing", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetAccessDecision_Found(t *testing.T) {
	store := &stubStore{findDecision: &domain.AccessDecisionLog{AccessDecisionID: "d-1", DecisionOutcome: "GRANTED"}}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/access-decisions/d-1", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestAuthorize_ForwardsVerifiedTenantScopeToStore is the handler-side guard
// for the cross-tenant grant leak.
//
// The store can only scope the role join to a tenant if the handler gives it
// one. resolveTenantScope already produced a verified scope immediately above
// the lookup — it simply was not passed, so every evaluation ran platform-wide.
// Asserting the ARGUMENT is the only way to catch that regressing: the decision
// outcome looks identical either way.
func TestAuthorize_ForwardsVerifiedTenantScopeToStore(t *testing.T) {
	const tenant = "tenant-a"

	store := &stubStore{
		rbacActions: []string{"PAYMENT_APPROVE"},
		rbacBasis:   "rbac:role=FINANCE_APPROVER",
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.grantedTenantArg != tenant {
		t.Fatalf("expected the verified tenant %q to reach FindGrantedActions, got %q — "+
			"an empty value means the evaluation ran platform-wide", tenant, store.grantedTenantArg)
	}
}

// TestAuthorize_NoTenantHeader_FallsBackToPlatformScope pins the other half of
// the contract. ~60 services call /v1/authorize and most do not forward
// X-Tenant-Id yet; scoping their evaluations to an empty tenant would match no
// role at all and deny every one of them. The empty argument is what selects
// the platform-scope fallback in the store.
func TestAuthorize_NoTenantHeader_FallsBackToPlatformScope(t *testing.T) {
	store := &stubStore{
		rbacActions: []string{"PAYMENT_APPROVE"},
		rbacBasis:   "rbac:role=FINANCE_APPROVER",
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubValidator{})

	body := `{"principal_id":"p-1","legal_entity_id":"le-1","action_type":"PAYMENT_APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.grantedTenantArg != "" {
		t.Fatalf("expected an empty tenant to select platform scope, got %q", store.grantedTenantArg)
	}
}

// TestDelegationRoutes_RequireTenantScope pins the refusal 000006 makes
// necessary.
//
// delegated_authorities.tenant_id is NOT NULL, and both delegation reads carry
// a tenant predicate. A caller with no verified scope could therefore only
// either write a row no policy can match, or ask a question that has no
// tenant-scoped answer. CreateDelegatedAuthority was the one /v1/admin/* route
// that never resolved a tenant at all, so this is the guard against it drifting
// back.
func TestDelegationRoutes_RequireTenantScope(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			name:   "create",
			method: http.MethodPost,
			path:   "/v1/admin/delegated-authorities",
			body:   `{"delegator_principal_id":"admin-1","delegate_principal_id":"p-2","scope_type":"FULL","effective_from":"2026-01-01T00:00:00Z"}`,
		},
		{
			name:   "revoke",
			method: http.MethodPost,
			path:   "/v1/admin/delegated-authorities/d-1/revoke",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{
				delegation:     &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1"},
				findDelegation: &domain.DelegatedAuthority{DelegatedAuthorityID: "d-1", DelegatorPrincipalID: "admin-1"},
			}
			r := newTestRouter(store)

			var req *http.Request
			if tc.body != "" {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			// A verified principal, but no verified tenant.
			req.Header.Set("X-Principal-Id", "admin-1")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 without X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
