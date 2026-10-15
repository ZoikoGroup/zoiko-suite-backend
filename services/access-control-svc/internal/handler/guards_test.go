package handler_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/domain"
)

// The pre-provisioning guards (6 Oct 2026 re-audit). Each test here fails
// against the code it replaced:
//
//   - every role create made an SoD call with no actions, which the real
//     authorization-svc answers 400, reported as 403 sod_conflict;
//   - the SoD check saw only the bundle being written, never the role's
//     other bundles, and skipped reactivation;
//   - an SoD outage and a real conflict were the same 403;
//   - an unreadable or empty protected catalogue skipped the protected check;
//   - authorization-svc refusing the caller (403) was reported 503.

func catalogue() *stubProtectedActions {
	return &stubProtectedActions{actions: []string{"PLATFORM_ADMIN", "ROLE_MANAGE", "iam.role.manage", "iam.assignment.grant"}}
}

func errCode(t *testing.T, body string) string {
	t.Helper()
	var e struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("error body is not JSON: %q", body)
	}
	return e.ErrorCode
}

func bundleWith(actions ...string) map[string]any {
	id := uuid.NewString()
	return map[string]any{
		"legal_entity_id":   "le-us",
		"bundle_code":       "B_" + strings.ToUpper(strings.ReplaceAll(id, "-", ""))[:12],
		"permitted_actions": actions,
		"correlation_id":    id,
	}
}

func TestCreateRole_MakesNoSoDCall(t *testing.T) {
	// A conflict verdict on every call: if role creation still asked, it
	// would be refused.
	sod := &stubSoD{err: &domain.SoDConflictError{}}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{}, sod, catalogue())
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("a role has no actions and cannot conflict; want 201, got %d %s", rr.Code, rr.Body.String())
	}
	if len(sod.reqs) != 0 {
		t.Fatalf("role creation made %d SoD call(s)", len(sod.reqs))
	}
}

func TestCreateBundle_SoDSeesTheRolesWholeActionSet(t *testing.T) {
	// The conflict exists only across two bundles of one role.
	sod := &stubSoD{decide: func(req domain.SoDCheckRequest) error {
		if slices.Contains(req.CandidateActions, "PAYMENT_INITIATE") && slices.Contains(req.CandidateActions, "PAYMENT_APPROVE") {
			return &domain.SoDConflictError{Conflicts: []domain.SoDConflict{{CandidateAction: "PAYMENT_APPROVE", ConflictsWith: "PAYMENT_INITIATE", Source: "candidate"}}}
		}
		return nil
	}}
	st := newStubStore()
	r := newRouter(st, &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{}, sod, catalogue())
	role := createRole(t, r)
	path := "/v1/role-definitions/" + role.RoleDefinitionID + "/permission-bundles"

	if rr := doReq(r, http.MethodPost, path, bundleWith("PAYMENT_INITIATE"), "admin-1"); rr.Code != http.StatusCreated {
		t.Fatalf("first bundle: %d %s", rr.Code, rr.Body.String())
	}
	rr := doReq(r, http.MethodPost, path, bundleWith("PAYMENT_APPROVE"), "admin-1")
	if rr.Code != http.StatusForbidden || errCode(t, rr.Body.String()) != "sod_conflict" {
		t.Fatalf("a conflict split across two bundles must be refused 403 sod_conflict, got %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "PAYMENT_APPROVE conflicts with PAYMENT_INITIATE") {
		t.Errorf("the refusal should name the pair, got %s", rr.Body.String())
	}
	last := sod.reqs[len(sod.reqs)-1]
	if last.TenantID != "tenant-abc" || last.CallerID != "admin-1" {
		t.Errorf("SoD request scope = %+v", last)
	}
	if n := len(st.refusals); n == 0 || st.refusals[n-1].RefusalReason != "sod_conflict" {
		t.Errorf("the refusal was not recorded as sod_conflict: %+v", st.refusals)
	}
}

func TestCreateBundle_SoDUnavailableIs503NotAConflict(t *testing.T) {
	sod := &stubSoD{err: fmt.Errorf("%w: authorization-svc returned 400", domain.ErrSoDUnavailable)}
	admin := &stubAuthzAdmin{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin, sod, catalogue())
	role := createRole(t, r)
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", bundleWith("PO_ISSUE"), "admin-1")
	if rr.Code != http.StatusServiceUnavailable || errCode(t, rr.Body.String()) != "sod_unavailable" {
		t.Fatalf("want 503 sod_unavailable, got %d %s", rr.Code, rr.Body.String())
	}
	if len(admin.gotScopes) != 1 { // the role create only
		t.Errorf("nothing may be provisioned without an SoD verdict; admin calls = %d", len(admin.gotScopes))
	}
}

func TestBundleReactivation_RunsTheGuards(t *testing.T) {
	sod := &stubSoD{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{}, sod, catalogue())
	role := createRole(t, r)
	base := "/v1/role-definitions/" + role.RoleDefinitionID + "/permission-bundles"

	rr := doReq(r, http.MethodPost, base, bundleWith("PAYMENT_INITIATE"), "admin-1")
	var first domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&first)
	if rr := doReq(r, http.MethodDelete, base+"/"+first.BundleID+"?legal_entity_id=le-us", nil, "admin-1"); rr.Code != http.StatusOK {
		t.Fatalf("detach: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodPost, base, bundleWith("PAYMENT_APPROVE"), "admin-1"); rr.Code != http.StatusCreated {
		t.Fatalf("second bundle (first is detached, so no conflict): %d %s", rr.Code, rr.Body.String())
	}

	sod.decide = func(req domain.SoDCheckRequest) error {
		if slices.Contains(req.CandidateActions, "PAYMENT_INITIATE") && slices.Contains(req.CandidateActions, "PAYMENT_APPROVE") {
			return &domain.SoDConflictError{}
		}
		return nil
	}
	rr = doReq(r, http.MethodPatch, base+"/"+first.BundleID, map[string]any{"legal_entity_id": "le-us", "active_flag": true}, "admin-1")
	if rr.Code != http.StatusForbidden || errCode(t, rr.Body.String()) != "sod_conflict" {
		t.Fatalf("reactivating into a conflict must be refused, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateBundle_EditReplacesItsOwnActionsInTheCandidateSet(t *testing.T) {
	sod := &stubSoD{}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{}, sod, catalogue())
	role := createRole(t, r)
	base := "/v1/role-definitions/" + role.RoleDefinitionID + "/permission-bundles"
	rr := doReq(r, http.MethodPost, base, bundleWith("A_OLD"), "admin-1")
	var b domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&b)

	rr = doReq(r, http.MethodPatch, base+"/"+b.BundleID, map[string]any{"legal_entity_id": "le-us", "permitted_actions": []string{"A_NEW"}}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rr.Code, rr.Body.String())
	}
	got := sod.reqs[len(sod.reqs)-1].CandidateActions
	if !slices.Equal(got, []string{"A_NEW"}) {
		t.Fatalf("the edited bundle's old actions must not be candidates; got %v", got)
	}
}

func TestProtectedAction_RefusedByName(t *testing.T) {
	for _, action := range []string{"iam.role.manage", " IAM.Assignment.Grant ", "ROLE_MANAGE"} {
		st := newStubStore()
		r := newRouter(st, &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{}, &stubSoD{}, catalogue())
		role := createRole(t, r)
		rr := doReq(r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", bundleWith("PO_ISSUE", action), "admin-1")
		if rr.Code != http.StatusForbidden || errCode(t, rr.Body.String()) != "protected_action" {
			t.Fatalf("%q: want 403 protected_action, got %d %s", action, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), strings.TrimSpace(action)) {
			t.Errorf("%q: the refusal should name the action: %s", action, rr.Body.String())
		}
	}
}

func TestProtectedCatalogue_FailsClosed(t *testing.T) {
	cases := map[string]*stubProtectedActions{
		"unreadable": {err: errors.New("connection refused")},
		"empty":      {actions: nil},
	}
	for name, cat := range cases {
		admin := &stubAuthzAdmin{}
		r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin, &stubSoD{}, cat)
		role := createRole(t, r)
		rr := doReq(r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", bundleWith("PLATFORM_ADMIN"), "admin-1")
		if rr.Code != http.StatusServiceUnavailable || errCode(t, rr.Body.String()) != "protected_catalogue_unavailable" {
			t.Fatalf("%s catalogue: want 503 protected_catalogue_unavailable, got %d %s", name, rr.Code, rr.Body.String())
		}
		if len(admin.gotScopes) != 1 {
			t.Errorf("%s catalogue: the bundle was provisioned anyway", name)
		}
	}
}

func TestAdminRefusal_Is403ProvisioningForbidden(t *testing.T) {
	forbidden := fmt.Errorf("%w: authorization-svc admin API returned 403", domain.ErrProvisioningForbidden)

	st := newStubStore()
	r := newRouter(st, &stubPublisher{}, &stubAuthZ{}, &stubAuthzAdmin{createRoleErr: forbidden}, &stubSoD{}, catalogue())
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "admin-1")
	if rr.Code != http.StatusForbidden || errCode(t, rr.Body.String()) != "provisioning_forbidden" {
		t.Fatalf("role: want 403 provisioning_forbidden, got %d %s", rr.Code, rr.Body.String())
	}
	if n := len(st.refusals); n == 0 || st.refusals[n-1].RefusalReason != "provisioning_forbidden" {
		t.Errorf("refusal not recorded: %+v", st.refusals)
	}

	admin := &stubAuthzAdmin{}
	r = newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, admin, &stubSoD{}, catalogue())
	role := createRole(t, r)
	admin.createBundleErr = forbidden
	rr = doReq(r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", bundleWith("PO_ISSUE"), "admin-1")
	if rr.Code != http.StatusForbidden || errCode(t, rr.Body.String()) != "provisioning_forbidden" {
		t.Fatalf("bundle: want 403 provisioning_forbidden, got %d %s", rr.Code, rr.Body.String())
	}

	// A genuine outage stays 503.
	admin.createBundleErr = errors.New("authorization-svc admin API unreachable")
	rr = doReq(r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", bundleWith("PO_ISSUE"), "admin-1")
	if rr.Code != http.StatusServiceUnavailable || errCode(t, rr.Body.String()) != "authz_admin_unavailable" {
		t.Fatalf("outage: want 503 authz_admin_unavailable, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestRefusalsOnUpdateRoleAndDetachAreRecorded(t *testing.T) {
	st := newStubStore()
	authz := &stubAuthZ{}
	r := newRouter(st, &stubPublisher{}, authz, &stubAuthzAdmin{}, &stubSoD{}, catalogue())
	role := createRole(t, r)
	rr := doReq(r, http.MethodPost, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles", bundleWith("PO_ISSUE"), "admin-1")
	var b domain.PermissionBundleDef
	_ = json.NewDecoder(rr.Body).Decode(&b)

	authz.err = domain.ErrAuthorizationDenied
	doReq(r, http.MethodPatch, "/v1/role-definitions/"+role.RoleDefinitionID, map[string]any{"legal_entity_id": "le-us", "status": "RETIRED"}, "intruder")
	doReq(r, http.MethodDelete, "/v1/role-definitions/"+role.RoleDefinitionID+"/permission-bundles/"+b.BundleID+"?legal_entity_id=le-us", nil, "intruder")

	var kinds []string
	for _, ref := range st.refusals {
		kinds = append(kinds, ref.ActionType+"/"+ref.RefusalReason)
	}
	for _, want := range []string{"UPDATE_ROLE/forbidden", "DETACH_BUNDLE/forbidden"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("missing refusal %s; recorded %v", want, kinds)
		}
	}
}
