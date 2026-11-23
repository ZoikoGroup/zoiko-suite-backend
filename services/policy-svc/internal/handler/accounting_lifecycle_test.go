package handler_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
)

// REF-07 lifecycle at the HTTP layer: each command asks for its own permission
// as the verified caller, replacing an active policy is stated, and an
// emergency suspension is honoured only under a live break-glass session. The
// transitions themselves, and the events, are proven on a real database in
// internal/store.

const (
	sessionID  = "99999999-9999-9999-9999-999999999999"
	otherSess  = "88888888-8888-8888-8888-888888888888"
	activeID   = "cccccccc-0000-0000-0000-0000000000aa"
	versionURL = "/v1/accounting-policies/versions/" + versionID + "/"
)

// emergencyAuthz is an authorization client that also answers the session-bound
// check.
type emergencyAuthz struct {
	*stubAuthz

	basis string
	err   error

	calls     int
	principal string
	scope     string
	action    string
	tenant    string
	session   string
}

func (a *emergencyAuthz) CheckEmergency(_ context.Context, principalID, legalEntityID, actionType, tenantID, sessionID string) (string, error) {
	a.calls++
	a.principal, a.scope, a.action, a.tenant, a.session = principalID, legalEntityID, actionType, tenantID, sessionID
	return a.basis, a.err
}

func acctVersionStub() *acctStub {
	v := draftedVersion()
	v.CreatedByPrincipalID = "the-author"
	v.LegalEntityID = strPtr(testEntity)
	v.PolicyVersion.VersionStatus = domain.StatusActive
	return &acctStub{
		version: v,
		stubStore: &stubStore{transitionResult: &domain.TransitionResult{
			Version: &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: policyID, RowVersion: 2}, Transitioned: true}},
	}
}

func ifMatchOne() reqOpt { return header("If-Match", `"1"`) }

// ── each command is its own act ─────────────────────────────────────────────

func TestAccountingCommands_EachAsksForItsOwnPermissionAsTheCaller(t *testing.T) {
	for _, c := range []struct {
		route, command, action, body string
	}{
		{"submit-review", domain.CommandSubmitReview, authz.ActionAccountingPolicyDraft, `{}`},
		{"approve", domain.CommandApprove, authz.ActionAccountingPolicyApprove, `{"approval_reference":"CHG-7"}`},
		{"activate", domain.CommandActivate, authz.ActionAccountingPolicyActivate, `{}`},
		{"supersede", domain.CommandActivate, authz.ActionAccountingPolicyActivate, `{"supersedes_version_id":"` + activeID + `"}`},
		{"review-suspension", domain.CommandReviewSuspension, authz.ActionAccountingPolicyApprove, `{"reason_code":"RC","outcome":"upheld"}`},
	} {
		st := acctVersionStub()
		g := newAcctRig(st, nil)
		w := g.do(http.MethodPost, versionURL+c.route, c.body, ifMatchOne())
		expect(t, w, http.StatusOK, "")
		if st.transitionCalls != 1 || st.transitionCmd.Command != c.command || st.transitionCmd.ActorID != testPrincipal ||
			st.transitionCmd.PolicyVersionID != versionID || st.transitionCmd.ExpectedVersion == nil || *st.transitionCmd.ExpectedVersion != 1 {
			t.Errorf("%s: command = %+v", c.route, st.transitionCmd)
		}
		if g.az.actionType != c.action || g.az.scope != testEntity || g.az.principal != testPrincipal || g.az.tenantID != testTenant {
			t.Errorf("%s: asked %s at %s as %s, want %s in the entity's scope as the caller", c.route, g.az.actionType, g.az.scope, g.az.principal, c.action)
		}
		if w.Header().Get("ETag") == "" {
			t.Errorf("%s: no ETag", c.route)
		}
	}
}

func TestAccountingCommands_SupersedeNamesTheVersionItReplaces(t *testing.T) {
	st := acctVersionStub()
	g := newAcctRig(st, nil)
	expect(t, g.do(http.MethodPost, versionURL+"supersede", `{"supersedes_version_id":"`+activeID+`"}`, ifMatchOne()), http.StatusOK, "")
	if st.transitionCmd.SupersedesVersionID != activeID {
		t.Errorf("supersedes = %q", st.transitionCmd.SupersedesVersionID)
	}

	for name, body := range map[string]string{"nothing named": `{}`, "not a uuid": `{"supersedes_version_id":"v1"}`} {
		st := acctVersionStub()
		g := newAcctRig(st, nil)
		expect(t, g.do(http.MethodPost, versionURL+"supersede", body, ifMatchOne()), http.StatusBadRequest, "INVALID_REQUEST")
		if st.transitionCalls != 0 {
			t.Errorf("%s: reached the store", name)
		}
	}
	// The other commands do not take one: it would be silently ignored.
	for _, route := range []string{"approve", "activate", "submit-review"} {
		st := acctVersionStub()
		g := newAcctRig(st, nil)
		expect(t, g.do(http.MethodPost, versionURL+route, `{"supersedes_version_id":"`+activeID+`"}`, ifMatchOne()), http.StatusBadRequest, "INVALID_REQUEST")
		if st.transitionCalls != 0 {
			t.Errorf("%s accepted a supersedes_version_id", route)
		}
	}
}

func TestAccountingCommands_InputRefusals(t *testing.T) {
	st := acctVersionStub()
	g := newAcctRig(st, nil)
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{}`), http.StatusPreconditionRequired, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{}`, header("If-Match", "x")), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{"expected_version":2}`, ifMatchOne()), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, versionURL+"review-suspension", `{"outcome":"UPHELD"}`, ifMatchOne()), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, versionURL+"review-suspension", `{"reason_code":"RC","outcome":"MAYBE"}`, ifMatchOne()), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, "/v1/accounting-policies/versions/not-a-uuid/approve", `{}`, ifMatchOne()), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{not json`, ifMatchOne()), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{}`, ifMatchOne(), noPrincipal), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{}`, ifMatchOne(), noTenant), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	if st.transitionCalls != 0 {
		t.Error("an invalid request reached the store")
	}
	// expected_version in the body works as If-Match does.
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{"expected_version":1}`), http.StatusOK, "")
}

func TestAccountingCommands_OnlyAccountingVersionsAreServed(t *testing.T) {
	st := acctVersionStub()
	st.versionErr = domain.ErrPolicyVersionNotFound
	g := newAcctRig(st, nil)
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{}`, ifMatchOne()), http.StatusNotFound, "NOT_FOUND")
	st.versionErr = domain.ErrStoreUnavailable
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{}`, ifMatchOne()), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	if st.transitionCalls != 0 {
		t.Error("a command reached the store for a version that is not an accounting version")
	}
}

func TestAccountingCommands_DeniedOrUnavailableAuthorizationStopsTheCommand(t *testing.T) {
	st := acctVersionStub()
	g := newAcctRig(st, &stubAuthz{byAction: map[string]error{authz.ActionAccountingPolicyActivate: domain.ErrAuthorizationDenied}})
	expect(t, g.do(http.MethodPost, versionURL+"activate", `{}`, ifMatchOne()), http.StatusForbidden, "AUTHORIZATION_DENIED")
	// Holding the approve permission is not holding the activate permission.
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{}`, ifMatchOne()), http.StatusOK, "")
	st2 := acctVersionStub()
	g = newAcctRig(st2, &stubAuthz{err: domain.ErrAuthorizationServiceUnavailable})
	expect(t, g.do(http.MethodPost, versionURL+"approve", `{}`, ifMatchOne()), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	if st2.transitionCalls != 0 {
		t.Error("a command ran without an authorization decision")
	}
}

func TestAccountingCommands_TransitionRefusalsAreMapped(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"the author as checker":   {domain.ErrSoDConflict, http.StatusForbidden, "SOD_CONFLICT"},
		"an unapproved version":   {domain.ErrApprovalRequired, http.StatusConflict, "APPROVAL_REQUIRED"},
		"silent replacement":      {domain.ErrSupersessionRequired, http.StatusConflict, "SUPERSESSION_REQUIRED"},
		"naming the wrong one":    {domain.ErrInvalidSupersedes, http.StatusConflict, "SUPERSESSION_REQUIRED"},
		"an ambiguous activation": {domain.ErrPolicyConflicted, http.StatusConflict, "POLICY_CONFLICTED"},
		"a stale version":         {domain.ErrConcurrencyConflict, http.StatusConflict, "CONCURRENCY_CONFLICT"},
		"a wrong state":           {domain.ErrInvalidTransition, http.StatusConflict, "CONCURRENCY_CONFLICT"},
		"store down":              {domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
	} {
		st := acctVersionStub()
		st.transitionErr = c.err
		g := newAcctRig(st, nil)
		w := g.do(http.MethodPost, versionURL+"activate", `{}`, ifMatchOne())
		if w.Code != c.status || decode(t, w)["code"] != c.code {
			t.Errorf("%s: %d %v, want %d %s", name, w.Code, decode(t, w)["code"], c.status, c.code)
		}
	}
}

// ── EmergencySuspendPolicy ──────────────────────────────────────────────────

func emergencyBody() string {
	return `{"reason_code":"WRONG_RATE","reason":"mis-stated rounding rule"}`
}

func TestEmergencySuspend_IsHonouredOnlyUnderTheLiveSession(t *testing.T) {
	st := acctVersionStub()
	az := &emergencyAuthz{stubAuthz: &stubAuthz{}, basis: "break_glass:session=" + sessionID + ":incident=INC-9"}
	g := newAcctRigClient(st, az)
	w := g.do(http.MethodPost, versionURL+"emergency-suspend", emergencyBody(), ifMatchOne(), header(handlerSessionHeader, sessionID))
	expect(t, w, http.StatusOK, "")

	if az.calls != 1 || az.action != authz.ActionAccountingPolicyEmergencySuspend || az.session != sessionID || az.principal != testPrincipal ||
		az.scope != testEntity || az.tenant != testTenant {
		t.Errorf("emergency check = %+v, want the suspend permission, the session and the caller in the entity's scope", az)
	}
	if az.stubAuthz.calls != 0 {
		t.Error("the ordinary check was used for an emergency act")
	}
	c := st.transitionCmd
	if st.transitionCalls != 1 || c.Command != domain.CommandSuspend || c.BreakGlassSessionID != sessionID ||
		!strings.Contains(c.Reason, "WRONG_RATE") || !strings.Contains(c.Reason, "mis-stated rounding rule") || !strings.Contains(c.Reason, "break_glass_session="+sessionID) {
		t.Errorf("command = %+v, want the reason and the session recorded", c)
	}
}

func TestEmergencySuspend_NeedsASessionNotJustThePermission(t *testing.T) {
	const header_ = handlerSessionHeader
	for name, c := range map[string]struct {
		az      *emergencyAuthz
		session string
		status  int
		code    string
	}{
		"no session at all":            {&emergencyAuthz{stubAuthz: &stubAuthz{}, basis: "break_glass:session=" + sessionID}, "", http.StatusForbidden, "EMERGENCY_SESSION_REQUIRED"},
		"a session id that is no uuid": {&emergencyAuthz{stubAuthz: &stubAuthz{}, basis: "break_glass:session=x"}, "x", http.StatusForbidden, "EMERGENCY_SESSION_REQUIRED"},
		// The principal holds the permission through a role: granted, but not
		// through the session, so it is not an emergency act.
		"a standing grant":          {&emergencyAuthz{stubAuthz: &stubAuthz{}, basis: "rbac:role=ACCOUNTING_AUTHORITY"}, sessionID, http.StatusForbidden, "EMERGENCY_SESSION_REQUIRED"},
		"another session's grant":   {&emergencyAuthz{stubAuthz: &stubAuthz{}, basis: "break_glass:session=" + otherSess}, sessionID, http.StatusForbidden, "EMERGENCY_SESSION_REQUIRED"},
		"a pam session's grant":     {&emergencyAuthz{stubAuthz: &stubAuthz{}, basis: "pam:session=" + sessionID + ":ticket=T-1"}, sessionID, http.StatusForbidden, "EMERGENCY_SESSION_REQUIRED"},
		"no basis":                  {&emergencyAuthz{stubAuthz: &stubAuthz{}, basis: ""}, sessionID, http.StatusForbidden, "EMERGENCY_SESSION_REQUIRED"},
		"denied (an ended session)": {&emergencyAuthz{stubAuthz: &stubAuthz{}, err: domain.ErrAuthorizationDenied}, sessionID, http.StatusForbidden, "AUTHORIZATION_DENIED"},
		"authorization down":        {&emergencyAuthz{stubAuthz: &stubAuthz{}, err: domain.ErrAuthorizationServiceUnavailable}, sessionID, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
	} {
		st := acctVersionStub()
		g := newAcctRigClient(st, c.az)
		var opts []reqOpt
		opts = append(opts, ifMatchOne())
		if c.session != "" {
			opts = append(opts, header(header_, c.session))
		}
		w := g.do(http.MethodPost, versionURL+"emergency-suspend", emergencyBody(), opts[0], opts[len(opts)-1])
		if w.Code != c.status || decode(t, w)["code"] != c.code {
			t.Errorf("%s: %d %v, want %d %s", name, w.Code, decode(t, w)["code"], c.status, c.code)
		}
		if st.transitionCalls != 0 {
			t.Errorf("%s: the policy was suspended", name)
		}
	}
}

// A client that cannot check sessions cannot honour an emergency act.
func TestEmergencySuspend_FailsClosedWithoutASessionCapableClient(t *testing.T) {
	st := acctVersionStub()
	g := newAcctRig(st, nil) // an ordinary stub: no CheckEmergency
	w := g.do(http.MethodPost, versionURL+"emergency-suspend", emergencyBody(), ifMatchOne(), header(handlerSessionHeader, sessionID))
	expect(t, w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	if st.transitionCalls != 0 {
		t.Error("an emergency suspension was made with no way to check the session")
	}
}

func TestEmergencySuspend_NeedsAReasonAndTheOrdinaryRules(t *testing.T) {
	st := acctVersionStub()
	az := &emergencyAuthz{stubAuthz: &stubAuthz{}, basis: "break_glass:session=" + sessionID}
	g := newAcctRigClient(st, az)
	expect(t, g.do(http.MethodPost, versionURL+"emergency-suspend", `{}`, ifMatchOne(), header(handlerSessionHeader, sessionID)), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodPost, versionURL+"emergency-suspend", emergencyBody(), header(handlerSessionHeader, sessionID)), http.StatusPreconditionRequired, "INVALID_REQUEST")
	if az.calls != 0 || st.transitionCalls != 0 {
		t.Error("an invalid emergency request reached authorization or the store")
	}
	// The ordinary commands never send the session anywhere.
	g2 := newAcctRigClient(acctVersionStub(), az)
	expect(t, g2.do(http.MethodPost, versionURL+"approve", `{}`, ifMatchOne(), header(handlerSessionHeader, sessionID)), http.StatusOK, "")
	if az.calls != 0 {
		t.Error("an ordinary command used the emergency check")
	}

	st = acctVersionStub()
	st.transitionErr = domain.ErrInvalidTransition
	g = newAcctRigClient(st, az)
	expect(t, g.do(http.MethodPost, versionURL+"emergency-suspend", emergencyBody(), ifMatchOne(), header(handlerSessionHeader, sessionID)), http.StatusConflict, "CONCURRENCY_CONFLICT")
}

const handlerSessionHeader = "X-Break-Glass-Session-Id"

// ── impact ──────────────────────────────────────────────────────────────────

func TestAccountingImpact(t *testing.T) {
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	st := &acctStub{impact: &domain.AccountingImpact{
		Version: draftedVersion(), Retroactive: true, PredecessorGovernsUntil: &until,
		WouldSupersede: &domain.ImpactVersionRef{PolicyVersionID: activeID}, AmbiguousWith: []domain.ImpactVersionRef{}, DependedOnBy: []domain.ImpactVersionRef{},
		ActivationBlockers: []string{"version x is active: activate by superseding it, naming it"},
	}}
	g := newAcctRig(st, nil)
	w := g.do(http.MethodGet, "/v1/accounting-policies/versions/"+versionID+"/impact", "")
	body := expect(t, w, http.StatusOK, "")
	if body["retroactive"] != true || body["would_supersede"].(map[string]any)["policy_version_id"] != activeID || len(body["activation_blockers"].([]any)) != 1 {
		t.Errorf("body = %v", body)
	}
	if g.az.actionType != authz.ActionPolicyRead {
		t.Errorf("asked %s, want POLICY_READ", g.az.actionType)
	}

	expect(t, g.do(http.MethodGet, "/v1/accounting-policies/versions/not-a-uuid/impact", ""), http.StatusBadRequest, "INVALID_REQUEST")
	g = newAcctRig(&acctStub{impactErr: domain.ErrPolicyVersionNotFound}, nil)
	expect(t, g.do(http.MethodGet, "/v1/accounting-policies/versions/"+versionID+"/impact", ""), http.StatusNotFound, "NOT_FOUND")
	g = newAcctRig(&acctStub{impactErr: domain.ErrStoreUnavailable}, nil)
	expect(t, g.do(http.MethodGet, "/v1/accounting-policies/versions/"+versionID+"/impact", ""), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	g = newAcctRig(&acctStub{impact: &domain.AccountingImpact{}}, &stubAuthz{err: domain.ErrAuthorizationDenied})
	expect(t, g.do(http.MethodGet, "/v1/accounting-policies/versions/"+versionID+"/impact", ""), http.StatusForbidden, "AUTHORIZATION_DENIED")
}
