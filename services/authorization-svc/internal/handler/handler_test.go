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
)

// ── stub store ────────────────────────────────────────────────────────────────

type stubStore struct {
	role        *domain.Role
	roleCreated bool
	roleErr     error

	bundle    *domain.PermissionBundle
	bundleErr error

	assignment      *domain.PrincipalRoleAssignment
	assignmentErr   error
	revokedAssign   *domain.PrincipalRoleAssignment
	revokeAssignErr error

	delegation          *domain.DelegatedAuthority
	delegationErr       error
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
func (s *stubStore) CreatePermissionBundle(_ context.Context, _ domain.CreatePermissionBundleParams) (*domain.PermissionBundle, error) {
	return s.bundle, s.bundleErr
}
func (s *stubStore) CreateRoleAssignment(_ context.Context, _ domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error) {
	return s.assignment, s.assignmentErr
}
func (s *stubStore) RevokeRoleAssignment(_ context.Context, _ string) (*domain.PrincipalRoleAssignment, error) {
	return s.revokedAssign, s.revokeAssignErr
}
func (s *stubStore) CreateDelegatedAuthority(_ context.Context, _ domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error) {
	return s.delegation, s.delegationErr
}
func (s *stubStore) RevokeDelegatedAuthority(_ context.Context, _ string) (*domain.DelegatedAuthority, error) {
	return s.revokedDelegation, s.revokeDelegationErr
}
func (s *stubStore) CreateSoDRule(_ context.Context, _ domain.CreateSoDRuleParams) (*domain.SoDRule, error) {
	return s.sodRule, s.sodRuleErr
}
func (s *stubStore) FindGrantedActions(_ context.Context, _, _ string) ([]string, string, error) {
	return s.rbacActions, s.rbacBasis, s.rbacErr
}
func (s *stubStore) FindDelegatedActions(_ context.Context, _, _ string) ([]string, string, error) {
	return s.delegatedActions, s.delegatedBasis, s.delegatedErr
}
func (s *stubStore) CheckSoDConflict(_ context.Context, _ []string, _ string) (string, bool, error) {
	return s.sodConflictAction, s.sodHasConflict, s.sodErr
}
func (s *stubStore) RecordAccessDecision(_ context.Context, _, _, _, outcome, basis, _ string) (*domain.AccessDecisionLog, error) {
	if s.decision != nil {
		return s.decision, s.recordErr
	}
	return &domain.AccessDecisionLog{AccessDecisionID: "d-1", DecisionOutcome: outcome, DecisionBasis: basis}, s.recordErr
}
func (s *stubStore) FindAccessDecisionByID(_ context.Context, _ string) (*domain.AccessDecisionLog, error) {
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
	h := handler.New(s, p, v, zap.NewNop())
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

	body := `{"tenant_id":"t-1","role_code":"FINANCE_APPROVER","role_name":"Finance Approver","role_scope_type":"LEGAL_ENTITY","created_by_principal_id":"admin-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRole_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── CreateSoDRule with jurisdiction validation ──────────────────────────────

func TestCreateSoDRule_JurisdictionNotFound(t *testing.T) {
	r := newTestRouterFull(&stubStore{}, &stubPublisher{}, &stubValidator{err: domain.ErrJurisdictionNotFound})

	body := `{"domain_code":"FINANCE","action_a":"PAYMENT_INITIATE","action_b":"PAYMENT_APPROVE","conflict_type":"MUTUALLY_EXCLUSIVE","jurisdiction_id":"jur-missing"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSoDRule_NoJurisdiction_Created(t *testing.T) {
	store := &stubStore{sodRule: &domain.SoDRule{SoDRuleID: "sod-1"}}
	r := newTestRouter(store)

	body := `{"domain_code":"FINANCE","action_a":"PAYMENT_INITIATE","action_b":"PAYMENT_APPROVE","conflict_type":"MUTUALLY_EXCLUSIVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sod-rules", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// ── RevokeDelegatedAuthority ─────────────────────────────────────────────────

func TestRevokeDelegatedAuthority_AlreadyRevoked(t *testing.T) {
	store := &stubStore{revokeDelegationErr: domain.ErrInvalidTransition}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/delegated-authorities/d-1/revoke", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// ── GetAccessDecision ────────────────────────────────────────────────────────

func TestGetAccessDecision_NotFound(t *testing.T) {
	store := &stubStore{findDecisionErr: domain.ErrAccessDecisionNotFound}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/access-decisions/missing", nil)
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
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}
