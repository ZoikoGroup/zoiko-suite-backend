package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
)

// GOV-03 commands, GOV-04 exceptions and §16 error classes.

// cmdStore adds the optional capabilities these routes use to stubStore.
type cmdStore struct {
	*stubStore
	emitted     []domain.OutboxMessage
	invalidated []string

	exceptions map[string]*domain.SoDException
	active     *domain.SoDException // FindActiveSoDException's answer
	pairs      [][2]string
}

func (s *cmdStore) EmitEvent(_ context.Context, m domain.OutboxMessage) error {
	s.emitted = append(s.emitted, m)
	return nil
}
func (s *cmdStore) InvalidateTenant(t string) { s.invalidated = append(s.invalidated, t) }

func (s *cmdStore) CreateSoDException(_ context.Context, p domain.CreateSoDExceptionParams) (*domain.SoDException, error) {
	e := &domain.SoDException{SoDExceptionID: "55555555-5555-4555-8555-555555555555", TenantID: p.TenantID, SoDRuleID: p.SoDRuleID,
		PrincipalID: p.PrincipalID, CompensatingControl: p.CompensatingControl, Reason: p.Reason, Status: domain.SoDExceptionRequested,
		RequestedBy: p.RequestedBy, ExpiresAt: p.ExpiresAt}
	s.exceptions[e.SoDExceptionID] = e
	return e, nil
}
func (s *cmdStore) FindSoDException(_ context.Context, id, _ string) (*domain.SoDException, error) {
	if e, ok := s.exceptions[id]; ok {
		return e, nil
	}
	return nil, domain.ErrSoDExceptionNotFound
}
func (s *cmdStore) ListSoDExceptions(context.Context, string, string, string) ([]domain.SoDException, error) {
	return nil, nil
}
func (s *cmdStore) TransitionSoDException(_ context.Context, id, _, from, to, actor string) (*domain.SoDException, error) {
	e := s.exceptions[id]
	if e.Status != from {
		return nil, domain.ErrSoDExceptionState
	}
	e.Status = to
	e.ApprovedBy = &actor
	return e, nil
}
func (s *cmdStore) FindActiveSoDException(_ context.Context, principalID, _, a, b string) (*domain.SoDException, error) {
	if s.active != nil && s.active.PrincipalID == principalID {
		for _, p := range s.pairs {
			if (p[0] == a && p[1] == b) || (p[0] == b && p[1] == a) {
				return s.active, nil
			}
		}
	}
	return nil, nil
}
func (s *cmdStore) CheckSoDConflict(_ context.Context, others []string, candidate, _ string) (string, bool, error) {
	for _, o := range others {
		for _, rule := range [][2]string{{"payment.prepare", "payment.release"}, {"supplier.bank_details.edit", "payment.release"}} {
			if (rule[0] == candidate && rule[1] == o) || (rule[1] == candidate && rule[0] == o) {
				return o, true, nil
			}
		}
	}
	return "", false, nil
}

func newCmdStore(s *stubStore) *cmdStore {
	return &cmdStore{stubStore: s, exceptions: map[string]*domain.SoDException{}}
}

func cmdDo(s handler.AuthorizationStore, method, caller, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Principal-Id", caller)
	req.Header.Set("X-Tenant-Id", govTenant)
	w := httptest.NewRecorder()
	govRouter(s, handler.CommandContractWarn).ServeHTTP(w, req)
	return w
}

// ── GOV-03 InvalidateAuthorizationCache ─────────────────────────────────────

func TestGOV03_InvalidateCache(t *testing.T) {
	denied := newCmdStore(&stubStore{denyAdmin: true})
	if w := cmdDo(denied, http.MethodPost, "clerk-1", "/v1/admin/authorization-cache/invalidate", `{}`); w.Code != http.StatusForbidden {
		t.Errorf("without iam.policy.publish: want 403, got %d", w.Code)
	}
	s := newCmdStore(&stubStore{denyAdmin: true, rbacActions: []string{"iam.policy.publish"}})
	w := cmdDo(s, http.MethodPost, "sec-1", "/internal/v1/gov03/commands/invalidateAuthorizationCache", `{"reason_code":"INCIDENT_42"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.invalidated) != 1 || s.invalidated[0] != govTenant {
		t.Errorf("local cache not invalidated for the tenant: %v", s.invalidated)
	}
	if len(s.emitted) != 1 || s.emitted[0].EventType != "authorization.cache.invalidated" || s.emitted[0].TenantID != govTenant {
		t.Fatalf("invalidation not announced to the other replicas: %+v", s.emitted)
	}
	var env map[string]any
	_ = json.Unmarshal(s.emitted[0].Value, &env)
	if p, _ := env["payload"].(map[string]any); p["reason"] != "INCIDENT_42" || p["invalidated_by"] != "sec-1" {
		t.Errorf("event payload = %v", env["payload"])
	}
}

// ── GOV-03 RecomputeSubjectEffectiveAccess ──────────────────────────────────

func TestGOV03_RecomputeEffectiveAccess(t *testing.T) {
	inner := newGovStore("iam.assignment.read")
	past := time.Now().Add(-time.Hour)
	inner.listAssignments = []domain.PrincipalRoleAssignment{
		{PrincipalRoleAssignmentID: "a-live", PrincipalID: "cfo-1", RoleID: "r-1", EffectiveFrom: past, ApprovalStatus: domain.ApprovalApproved},
		{PrincipalRoleAssignmentID: "a-pending", PrincipalID: "cfo-1", RoleID: "r-2", EffectiveFrom: past, ApprovalStatus: domain.ApprovalPending},
	}
	inner.role = &domain.Role{RoleID: "r-1", TenantID: govTenant, ActiveFlag: true}
	inner.roleBundles["r-1"] = []domain.PermissionBundle{{RoleID: "r-1", PermittedActions: []string{"payment.approve"}, ActiveFlag: true}}
	inner.roleBundles["r-2"] = []domain.PermissionBundle{{RoleID: "r-2", PermittedActions: []string{"iam.role.manage"}, ActiveFlag: true}}
	w := cmdDo(inner, http.MethodPost, "auditor-1", "/v1/admin/subjects/cfo-1/effective-access/recompute", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		EffectiveActions []string `json:"effective_actions"`
		Assignments      []struct {
			ID       string `json:"principal_role_assignment_id"`
			Granting bool   `json:"granting"`
		} `json:"assignments"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.EffectiveActions) != 1 || resp.EffectiveActions[0] != "payment.approve" {
		t.Errorf("a pending assignment must not count as effective access: %v", resp.EffectiveActions)
	}
	if len(resp.Assignments) != 2 || resp.Assignments[1].Granting {
		t.Errorf("assignments = %+v", resp.Assignments)
	}

	noRead := newGovStore()
	if w := cmdDo(noRead, http.MethodPost, "clerk-1", "/v1/admin/subjects/cfo-1/effective-access/recompute", `{}`); w.Code != http.StatusForbidden {
		t.Errorf("another subject without iam.assignment.read: want 403, got %d", w.Code)
	}
}

// ── GOV-04 compensating-control exceptions ──────────────────────────────────

const ruleID = "66666666-6666-4666-8666-666666666666"

func TestGOV04_ExceptionMustExpire(t *testing.T) {
	s := newCmdStore(&stubStore{denyAdmin: true, rbacActions: []string{"iam.sod_rule.manage"}})
	w := cmdDo(s, http.MethodPost, "ctl-1", "/v1/admin/sod-exceptions",
		`{"sod_rule_id":"`+ruleID+`","principal_id":"p-1","compensating_control":"daily bank rec review","reason":"single-person entity"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "expires_at") {
		t.Fatalf("exception without expiry: want 400 naming expires_at, got %d %s", w.Code, w.Body.String())
	}
}

func TestGOV04_ExceptionCannotBeSelfApproved(t *testing.T) {
	s := newCmdStore(&stubStore{denyAdmin: true, rbacActions: []string{"iam.sod_rule.manage", "iam.sod_rule.publish"}})
	exp := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	w := cmdDo(s, http.MethodPost, "ctl-1", "/v1/admin/sod-exceptions",
		`{"sod_rule_id":"`+ruleID+`","principal_id":"p-1","compensating_control":"daily bank rec review","reason":"single-person entity","expires_at":"`+exp+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("request: want 201, got %d %s", w.Code, w.Body.String())
	}
	id := "55555555-5555-4555-8555-555555555555"
	for _, who := range []string{"ctl-1", "p-1"} {
		if w := cmdDo(s, http.MethodPost, who, "/v1/admin/sod-exceptions/"+id+"/approve", `{"reason_code":"OK"}`); w.Code != http.StatusForbidden {
			t.Errorf("approval by %s (requester or subject): want 403, got %d", who, w.Code)
		}
	}
	if w := cmdDo(s, http.MethodPost, "cro-1", "/v1/admin/sod-exceptions/"+id+"/approve", `{"reason_code":"OK"}`); w.Code != http.StatusOK {
		t.Fatalf("independent approval: want 200, got %d %s", w.Code, w.Body.String())
	}
	if w := cmdDo(s, http.MethodPost, "cro-1", "/v1/admin/sod-exceptions/"+id+"/approve", `{}`); w.Code != http.StatusConflict {
		t.Errorf("second approval of a decided request: want 409, got %d", w.Code)
	}
}

func TestGOV04_ActiveExceptionPermitsWithObligation(t *testing.T) {
	s := newCmdStore(&stubStore{rbacActions: []string{"payment.prepare", "payment.release"}})
	s.active = &domain.SoDException{SoDExceptionID: "ex-1", PrincipalID: "p-1", CompensatingControl: "independent daily review"}
	s.pairs = [][2]string{{"payment.prepare", "payment.release"}}
	w := cmdDo(s, http.MethodPost, "p-1", "/v1/authorize", `{"principal_id":"p-1","legal_entity_id":"`+evEntity+`","action_type":"payment.release"}`)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	obl, _ := got["obligations"].([]any)
	if got["decision"] != "PERMIT" || len(obl) == 0 || obl[0] != "COMPENSATING_CONTROL:independent daily review" {
		t.Fatalf("excepted conflict: want PERMIT with the compensating control, got %v", got)
	}
	if !strings.Contains(w.Body.String(), "SOD_EXCEPTION_APPLIED") {
		t.Errorf("reason codes must say the conflict was excepted, not clear: %v", got["reason_codes"])
	}
}

// An exception for one pair must not hide another conflicting pair.
func TestGOV04_ExceptionDoesNotHideAnotherConflict(t *testing.T) {
	s := newCmdStore(&stubStore{rbacActions: []string{"payment.prepare", "supplier.bank_details.edit", "payment.release"}})
	s.active = &domain.SoDException{SoDExceptionID: "ex-1", PrincipalID: "p-1", CompensatingControl: "review"}
	s.pairs = [][2]string{{"payment.prepare", "payment.release"}}
	w := cmdDo(s, http.MethodPost, "p-1", "/v1/authorize", `{"principal_id":"p-1","legal_entity_id":"`+evEntity+`","action_type":"payment.release"}`)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["decision_outcome"] != "DENIED" || got["decision_basis"] != "sod:conflict_with=supplier.bank_details.edit" {
		t.Fatalf("the un-excepted bank-detail conflict must still deny: %v", got)
	}
}

func TestGOV04_ListConflictingPermissions(t *testing.T) {
	s := newCmdStore(&stubStore{listSoDRules: []domain.SoDRule{
		{SoDRuleID: "r1", ActionA: "payment.prepare", ActionB: "payment.release", ActiveFlag: true},
		{SoDRuleID: "r2", ActionA: "supplier.bank_details.edit", ActionB: "payment.release", ActiveFlag: true},
		{SoDRuleID: "r3", ActionA: "journal.create", ActionB: "payment.release", ActiveFlag: false},
	}})
	w := cmdDo(s, http.MethodGet, "p-1", "/v1/sod/conflicting-permissions?action_type=payment.release", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "payment.prepare") ||
		!strings.Contains(w.Body.String(), "supplier.bank_details.edit") || strings.Contains(w.Body.String(), "journal.create") {
		t.Fatalf("want the two active conflicts only, got %d %s", w.Code, w.Body.String())
	}
}

// ── §16 stable error classes ────────────────────────────────────────────────

func TestErrorClasses(t *testing.T) {
	s := newCmdStore(&stubStore{denyAdmin: true})
	cases := []struct {
		caller, path, body, wantClass string
		wantCode                      int
	}{
		{"clerk-1", "/v1/admin/authorization-cache/invalidate", `{}`, "AUTHORIZATION_DENIED", 403},
		{"", "/v1/admin/authorization-cache/invalidate", `{}`, "CONTEXT_UNRESOLVED", 401},
	}
	for _, c := range cases {
		w := cmdDo(s, http.MethodPost, c.caller, c.path, c.body)
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != c.wantCode || got["error_class"] != c.wantClass || got["error"] == nil {
			t.Errorf("%s as %q: want %d %s beside the code, got %d %v", c.path, c.caller, c.wantCode, c.wantClass, w.Code, got)
		}
	}
	// A validation error has no §16 class and gets none.
	w := cmdDo(newCmdStore(&stubStore{}), http.MethodPost, "p-1", "/v1/authorize", `{"legal_entity_id":"x"}`)
	if strings.Contains(w.Body.String(), "error_class") {
		t.Errorf("validation error given a forced class: %s", w.Body.String())
	}
}
