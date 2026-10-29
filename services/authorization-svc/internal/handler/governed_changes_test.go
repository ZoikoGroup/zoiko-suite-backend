package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
)

// AU-02, AU-03, AU-13, AU-17 at the handler: who may decide, what reaches the
// store, and what a decision records. The database half (state, CHECKs,
// triggers, events) is governed_changes_integration_test.go in store.

// governedCall is what a stub decide method was asked to do.
type governedCall struct {
	ID, Scope, Decider string
	Platform, Approve  bool
}

// ── stubStore: the governedStore capability ──────────────────────────────────

func (s *stubStore) ApproveBreakGlassSession(_ context.Context, sessionID, tenantID, approver string, stepUp *domain.StepUpEvidence) (*domain.BreakGlassSession, error) {
	s.gotStepUp = stepUp
	s.gotGoverned = &governedCall{ID: sessionID, Scope: tenantID, Decider: approver, Approve: true}
	return s.bgDecided, s.governedErr
}

func (s *stubStore) RejectBreakGlassSession(_ context.Context, sessionID, tenantID, decider string) (*domain.BreakGlassSession, error) {
	s.gotGoverned = &governedCall{ID: sessionID, Scope: tenantID, Decider: decider}
	return s.bgDecided, s.governedErr
}

func (s *stubStore) FindSoDRule(_ context.Context, _, _ string) (*domain.SoDRule, error) {
	if s.sodFound == nil {
		return nil, domain.ErrSoDRuleNotFound
	}
	return s.sodFound, nil
}

// RequestSoDRuleChange records through the SetSoDRuleActive fields the
// lifecycle tests already assert on: RETIRE is active=false.
func (s *stubStore) RequestSoDRuleChange(ctx context.Context, sodRuleID, tenantID, change, _, _ string, expectedVersion int64) (*domain.SoDRule, bool, error) {
	r, err := s.SetSoDRuleActive(ctx, sodRuleID, tenantID, change == domain.RuleChangeReactivate, expectedVersion)
	return r, err == nil, err
}

func (s *stubStore) DecideSoDRuleChange(_ context.Context, sodRuleID, tenantID string, platformWide bool, decider string, approve bool) (*domain.SoDRule, error) {
	s.gotGoverned = &governedCall{ID: sodRuleID, Scope: tenantID, Decider: decider, Platform: platformWide, Approve: approve}
	if s.governedErr != nil {
		return nil, s.governedErr
	}
	return s.sodFound, nil
}

func (s *stubStore) FindABACRule(_ context.Context, _, _ string) (*domain.ABACRule, error) {
	if s.abacFound == nil {
		return nil, domain.ErrABACRuleNotFound
	}
	return s.abacFound, nil
}

func (s *stubStore) RequestABACRuleChange(ctx context.Context, abacRuleID, tenantID, change, _, _ string, expectedVersion int64) (*domain.ABACRule, bool, error) {
	r, err := s.SetABACRuleActive(ctx, abacRuleID, tenantID, change == domain.RuleChangeReactivate, expectedVersion)
	return r, err == nil, err
}

func (s *stubStore) DecideABACRuleChange(_ context.Context, abacRuleID, tenantID string, platformWide bool, decider string, approve bool) (*domain.ABACRule, error) {
	s.gotGoverned = &governedCall{ID: abacRuleID, Scope: tenantID, Decider: decider, Platform: platformWide, Approve: approve}
	if s.governedErr != nil {
		return nil, s.governedErr
	}
	return s.abacFound, nil
}

func (s *stubStore) RequestAuthorityLimitRetire(_ context.Context, limitID, tenantID, requester, _ string, _ int64) (*domain.AuthorityLimit, error) {
	s.gotGoverned = &governedCall{ID: limitID, Scope: tenantID, Decider: requester}
	if s.governedErr != nil {
		return nil, s.governedErr
	}
	return s.authorityLimit, nil
}

func (s *stubStore) DecideAuthorityLimit(_ context.Context, limitID, tenantID, decider string, approve bool) (*domain.AuthorityLimit, error) {
	s.gotGoverned = &governedCall{ID: limitID, Scope: tenantID, Decider: decider, Approve: approve}
	if s.governedErr != nil {
		return nil, s.governedErr
	}
	return s.authorityLimit, nil
}

func postReq(store *stubStore, configure func(*handler.Handler), caller, path, body string, stepUp bool) *httptest.ResponseRecorder {
	r := scRouterWith(store, configure)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", scTenant)
	req.Header.Set("X-Principal-Id", caller)
	if stepUp {
		req = withStepUp(req, caller)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func noConfig(*handler.Handler) {}

// ── AU-02: break-glass approval ──────────────────────────────────────────────

func TestGoverned_BreakGlassRequestCarriesTheQuorum(t *testing.T) {
	s := &stubStore{}
	body := `{"principal_id":"` + scSubject + `","incident_id":"INC-1","reason":"r","requested_actions":["gateway.failover"]}`
	w := postReq(s, func(h *handler.Handler) { h.SetBreakGlassApprovals(2) }, scAdmin, "/admin/v1/break-glass-sessions", body, true)
	if w.Code != http.StatusAccepted {
		t.Fatalf("break-glass request: want 202, got %d: %s", w.Code, w.Body.String())
	}
	if s.createdBreakGlassParam.ApprovalsRequired != 2 {
		t.Errorf("quorum reaching the store = %d, want 2", s.createdBreakGlassParam.ApprovalsRequired)
	}

	// Unset or nonsense is one approver, never zero.
	s = &stubStore{}
	postReq(s, noConfig, scAdmin, "/admin/v1/break-glass-sessions", body, true)
	if s.createdBreakGlassParam.ApprovalsRequired != 1 {
		t.Errorf("default quorum = %d, want 1", s.createdBreakGlassParam.ApprovalsRequired)
	}
}

func pendingBG() *domain.BreakGlassSession {
	requester := scAdmin
	return &domain.BreakGlassSession{SessionID: "bg-1", TenantID: scTenant, PrincipalID: scSubject, IncidentID: "INC-1",
		Status: domain.BreakGlassSessionStatusPendingApproval, RequestedBy: &requester, ApprovalsRequired: 1}
}

func TestGoverned_BreakGlassApproverIsIndependent(t *testing.T) {
	for _, who := range []string{scSubject, scAdmin} { // the subject, the requester
		s := &stubStore{breakGlassSession: pendingBG()}
		w := postReq(s, noConfig, who, "/admin/v1/break-glass-sessions/bg-1/approve", `{"reason_code":"MEETS_POLICY"}`, true)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s approving: want 403, got %d: %s", who, w.Code, w.Body.String())
		}
		if s.gotGoverned != nil {
			t.Fatalf("%s: a self-approval reached the store", who)
		}
	}
}

func TestGoverned_BreakGlassApprovalNeedsStepUpRejectionDoesNot(t *testing.T) {
	s := &stubStore{breakGlassSession: pendingBG()}
	w := postReq(s, noConfig, scThird, "/admin/v1/break-glass-sessions/bg-1/approve", `{"reason_code":"MEETS_POLICY"}`, false)
	assertStepUpRequired(t, "break-glass approval", w)
	if s.gotGoverned != nil {
		t.Fatal("an approval without a step-up reached the store")
	}

	active := pendingBG()
	active.Status = domain.BreakGlassSessionStatusActive
	s = &stubStore{breakGlassSession: pendingBG(), bgDecided: active}
	w = postReq(s, noConfig, scThird, "/admin/v1/break-glass-sessions/bg-1/approve", `{"reason_code":"MEETS_POLICY"}`, true)
	if w.Code != http.StatusOK || s.gotGoverned == nil || !s.gotGoverned.Approve || s.gotGoverned.Decider != scThird {
		t.Fatalf("stepped-up approval: %d %s %+v", w.Code, w.Body.String(), s.gotGoverned)
	}
	if s.gotStepUp == nil || s.gotStepUp.SessionContextID != "sc-"+scThird {
		t.Errorf("approver's step-up evidence not passed to the store: %+v", s.gotStepUp)
	}

	rejected := pendingBG()
	rejected.Status = domain.BreakGlassSessionStatusRejected
	s = &stubStore{breakGlassSession: pendingBG(), bgDecided: rejected}
	w = postReq(s, noConfig, scThird, "/v1/admin/break-glass-sessions/bg-1/reject", `{"reason_code":"NOT_JUSTIFIED"}`, false)
	if w.Code != http.StatusOK || s.gotGoverned == nil || s.gotGoverned.Approve {
		t.Fatalf("rejection: %d %s %+v", w.Code, w.Body.String(), s.gotGoverned)
	}
}

func TestGoverned_BreakGlassApprovalNeedsPermissionAndReason(t *testing.T) {
	s := &stubStore{breakGlassSession: pendingBG(), denyAdmin: true}
	if w := postReq(s, noConfig, scThird, "/admin/v1/break-glass-sessions/bg-1/approve", `{"reason_code":"MEETS_POLICY"}`, true); w.Code != http.StatusForbidden {
		t.Fatalf("without iam.break_glass.approve: want 403, got %d", w.Code)
	}
	s = &stubStore{breakGlassSession: pendingBG()}
	enforce := func(h *handler.Handler) { h.SetCommandContract(handler.CommandContractEnforce) }
	if w := postReq(s, enforce, scThird, "/admin/v1/break-glass-sessions/bg-1/approve", `{}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("approval without a reason under enforce: want 400, got %d", w.Code)
	}
}

func TestGoverned_BreakGlassStoreRefusalsMapToConflicts(t *testing.T) {
	for err, want := range map[error]int{
		domain.ErrSessionNotPending:     http.StatusConflict,
		domain.ErrAlreadyApproved:       http.StatusConflict,
		domain.ErrCheckerNotIndependent: http.StatusForbidden,
	} {
		s := &stubStore{breakGlassSession: pendingBG(), governedErr: err}
		if w := postReq(s, noConfig, scThird, "/admin/v1/break-glass-sessions/bg-1/approve", `{"reason_code":"x"}`, true); w.Code != want {
			t.Errorf("%v: want %d, got %d", err, want, w.Code)
		}
	}
}

// A pending break-glass request grants nothing; an active one grants its
// actions, and every decision made in it — granted or denied — names it and
// carries the BREAK_GLASS tag (AU-13).
func TestGoverned_DecisionsInABreakGlassSessionAreTagged(t *testing.T) {
	authorize := func(s *stubStore, action string) *httptest.ResponseRecorder {
		r := newTestRouterFull(s, &stubPublisher{}, &stubValidator{})
		body := `{"principal_id":"` + testBGElevatedUser + `","legal_entity_id":"` + testBGLegalEntityID +
			`","action_type":"` + action + `","break_glass_session_id":"bg-session-99"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
		req.Header.Set("X-Tenant-Id", testBGTenantID)
		req.Header.Set("X-Principal-Id", testBGElevatedUser)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	session := func(status string) *domain.BreakGlassSession {
		return &domain.BreakGlassSession{SessionID: "bg-session-99", TenantID: testBGTenantID, PrincipalID: testBGElevatedUser,
			IncidentID: "INC-9", RequestedActions: []string{"emergency.restore"}, Status: status,
			ExpiresAt: time.Now().Add(30 * time.Minute)}
	}

	s := &stubStore{breakGlassSession: session(domain.BreakGlassSessionStatusPendingApproval)}
	if outcome, _ := decodeDecision(t, authorize(s, "emergency.restore")); outcome != "DENIED" {
		t.Fatalf("a pending break-glass request granted %s", outcome)
	}
	if s.recordedParams.SessionID != "" {
		t.Errorf("a pending request was recorded as the decision's session: %q", s.recordedParams.SessionID)
	}

	s = &stubStore{breakGlassSession: session(domain.BreakGlassSessionStatusActive)}
	if outcome, _ := decodeDecision(t, authorize(s, "emergency.restore")); outcome != "GRANTED" {
		t.Fatalf("active session: %s", outcome)
	}
	p := s.recordedParams
	if p.SessionKind != domain.SessionKindBreakGlass || p.SessionID != "bg-session-99" {
		t.Errorf("granted decision recorded session %q/%q", p.SessionKind, p.SessionID)
	}
	if !containsStr(p.ReasonCodes, "BREAK_GLASS") || !containsStr(p.Obligations, "EVIDENCE_TAG:BREAK_GLASS:incident=INC-9") {
		t.Errorf("BREAK_GLASS tag missing: reasons %v obligations %v", p.ReasonCodes, p.Obligations)
	}

	// Denied in the session: still the session's activity.
	s = &stubStore{breakGlassSession: session(domain.BreakGlassSessionStatusActive)}
	if outcome, _ := decodeDecision(t, authorize(s, "payments.wipe")); outcome != "DENIED" {
		t.Fatalf("unrequested action: %s", outcome)
	}
	if s.recordedParams.SessionID != "bg-session-99" {
		t.Errorf("a denial inside the session was not attributed to it")
	}
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func TestGoverned_SessionActivitySummarises(t *testing.T) {
	decisions := []domain.AccessDecisionLog{
		{AccessDecisionID: "d1", ActionType: "emergency.restore", DecisionOutcome: domain.OutcomeGranted, ResourceType: "db", ResourceID: "core"},
		{AccessDecisionID: "d2", ActionType: "emergency.restore", DecisionOutcome: domain.OutcomeGranted, ResourceType: "db", ResourceID: "core"},
		{AccessDecisionID: "d3", ActionType: "payments.wipe", DecisionOutcome: domain.OutcomeDenied},
	}
	get := func(s *stubStore, path string) *httptest.ResponseRecorder {
		r := scRouterWith(s, noConfig)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-Id", scTenant)
		req.Header.Set("X-Principal-Id", scThird)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	s := &stubStore{breakGlassSession: pendingBG(), listDecisions: &domain.AccessDecisionPage{Decisions: decisions}}
	w := get(s, "/v1/admin/break-glass-sessions/bg-1/activity")
	if w.Code != http.StatusOK {
		t.Fatalf("activity: %d %s", w.Code, w.Body.String())
	}
	var act domain.SessionActivity
	_ = json.Unmarshal(w.Body.Bytes(), &act)
	if act.Granted != 2 || act.Denied != 1 || len(act.Actions) != 2 || len(act.Resources) != 1 || act.Resources[0] != "db:core" {
		t.Errorf("summary = %+v", act)
	}
	if s.gotListDecisionsParams.SessionID != "bg-1" || s.gotListDecisionsTenant != scTenant {
		t.Errorf("decision read not scoped to the session and tenant: %+v %s", s.gotListDecisionsParams, s.gotListDecisionsTenant)
	}

	if w := get(&stubStore{}, "/v1/admin/break-glass-sessions/missing/activity"); w.Code != http.StatusNotFound {
		t.Errorf("unknown session: want 404, got %d", w.Code)
	}
	if w := get(&stubStore{breakGlassSession: pendingBG(), denyAdmin: true}, "/v1/admin/break-glass-sessions/bg-1/activity"); w.Code != http.StatusForbidden {
		t.Errorf("no review or manage permission: want 403, got %d", w.Code)
	}
}

// ── AU-17: rule publication ──────────────────────────────────────────────────

func pendingSoD(tenant *string, change, requester string) *domain.SoDRule {
	r := &domain.SoDRule{SoDRuleID: "s-1", ActionA: "PAYMENT_APPROVE", ActionB: "PAYMENT_INITIATE", TenantID: tenant}
	r.PendingChange, r.ChangeRequestedBy = &change, &requester
	r.PublicationStatus = domain.PublicationPending
	return r
}

func TestGoverned_RuleIsPublishedBySomeoneElse(t *testing.T) {
	tenant := scTenant
	s := &stubStore{sodFound: pendingSoD(&tenant, domain.RuleChangePublish, scAdmin)}
	if w := postReq(s, noConfig, scAdmin, "/v1/admin/sod-rules/s-1/approve", `{"reason_code":"x"}`, false); w.Code != http.StatusForbidden {
		t.Fatalf("maker publishing own rule: want 403, got %d", w.Code)
	}
	if s.gotGoverned != nil {
		t.Fatal("a self-publication reached the store")
	}
	w := postReq(s, noConfig, scThird, "/v1/admin/sod-rules/s-1/approve", `{"reason_code":"x"}`, false)
	if w.Code != http.StatusOK || s.gotGoverned == nil || !s.gotGoverned.Approve || s.gotGoverned.Scope != scTenant || s.gotGoverned.Platform {
		t.Fatalf("independent publication: %d %+v", w.Code, s.gotGoverned)
	}

	s = &stubStore{sodFound: pendingSoD(&tenant, domain.RuleChangePublish, scAdmin), denyAdmin: true}
	if w := postReq(s, noConfig, scThird, "/v1/admin/sod-rules/s-1/approve", `{"reason_code":"x"}`, false); w.Code != http.StatusForbidden {
		t.Fatalf("without iam.sod_rule.publish: want 403, got %d", w.Code)
	}
}

// A platform-wide rule binds every tenant: publishing it takes the
// platform-scope grant, which a tenant administrator does not hold.
func TestGoverned_PlatformWideRuleNeedsThePlatformGrant(t *testing.T) {
	s := &stubStore{sodFound: pendingSoD(nil, domain.RuleChangePublish, scAdmin)}
	if w := postReq(s, noConfig, scThird, "/v1/admin/sod-rules/s-1/approve", `{"reason_code":"x"}`, false); w.Code != http.StatusForbidden {
		t.Fatalf("tenant admin publishing a platform-wide rule: want 403, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotGoverned != nil {
		t.Fatal("reached the store")
	}
}

// Approving a retirement is switching the control off: an approver who holds
// either action may not, exactly as they may not ask for it.
func TestGoverned_InterestedPartyCannotApproveARetirement(t *testing.T) {
	tenant := ownRoleTenant
	s := sodStore("PAYMENT_APPROVE")
	s.sodFound = pendingSoD(&tenant, domain.RuleChangeRetire, "someone-else")
	s.sodFound.PublicationStatus = domain.PublicationPublished
	w := postAsAdmin(s, "/v1/admin/sod-rules/s-1/approve", `{"reason_code":"x"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("holder approving retirement: want 403, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotGoverned != nil {
		t.Fatal("reached the store")
	}
	// Rejecting a retirement keeps the control; a holder may.
	if w := postAsAdmin(s, "/v1/admin/sod-rules/s-1/reject", `{"reason_code":"x"}`); w.Code != http.StatusOK {
		t.Fatalf("holder rejecting retirement: want 200, got %d", w.Code)
	}
}

func TestGoverned_ABACRulePublication(t *testing.T) {
	tenant := scTenant
	change, maker := domain.RuleChangePublish, scAdmin
	rule := &domain.ABACRule{ABACRuleID: "a-1", TenantID: &tenant}
	rule.PendingChange, rule.ChangeRequestedBy = &change, &maker
	s := &stubStore{abacFound: rule}
	if w := postReq(s, noConfig, scAdmin, "/v1/admin/abac-rules/a-1/approve", `{"reason_code":"x"}`, false); w.Code != http.StatusForbidden {
		t.Fatalf("maker: want 403, got %d", w.Code)
	}
	if w := postReq(s, noConfig, scThird, "/v1/admin/abac-rules/a-1/reject", `{"reason_code":"x"}`, false); w.Code != http.StatusOK || s.gotGoverned.Approve {
		t.Fatalf("reject: %d %+v", w.Code, s.gotGoverned)
	}
	s = &stubStore{abacFound: rule, governedErr: domain.ErrNoPendingChange}
	if w := postReq(s, noConfig, scThird, "/v1/admin/abac-rules/a-1/approve", `{"reason_code":"x"}`, false); w.Code != http.StatusConflict {
		t.Fatalf("nothing pending: want 409, got %d", w.Code)
	}
}

// ── AU-03: authority limits ──────────────────────────────────────────────────

func TestGoverned_AuthorityLimitRequestIsValidatedAndPending(t *testing.T) {
	for body, field := range map[string]string{
		`{"authority_type":"invoice_approval","upper_limit":"100"}`:                                            "principal_id",
		`{"principal_id":"p-1","upper_limit":"100"}`:                                                           "authority_type",
		`{"principal_id":"p-1","authority_type":"x","upper_limit":"100","currency":"XYZ"}`:                     "currency",
		`{"principal_id":"p-1","authority_type":"x","upper_limit":"10","lower_limit":"20"}`:                    "upper_limit",
		`{"principal_id":"p-1","authority_type":"x","upper_limit":"ten"}`:                                      "upper_limit",
		`{"principal_id":"p-1","authority_type":"x","upper_limit":"10","effective_from":"2026-11-01"}`:         "effective_from",
		`{"principal_id":"p-1","authority_type":"x","upper_limit":"10","effective_to":"2020-01-01T00:00:00Z"}`: "effective_to",
	} {
		s := &stubStore{}
		w := postReq(s, noConfig, scAdmin, "/v1/admin/authority-limits", body, false)
		var got map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != http.StatusBadRequest || got["field"] != field {
			t.Errorf("%s: want 400 on %s, got %d %s", body, field, w.Code, w.Body.String())
		}
		if s.createdLimit != nil {
			t.Errorf("%s: an invalid limit reached the store", body)
		}
	}

	s := &stubStore{}
	w := postReq(s, noConfig, scAdmin, "/v1/admin/authority-limits",
		`{"principal_id":"p-1","authority_type":"invoice_approval","currency":"usd","upper_limit":"25000.00","reason_code":"NEW_ROLE"}`, false)
	if w.Code != http.StatusAccepted {
		t.Fatalf("valid limit: want 202, got %d: %s", w.Code, w.Body.String())
	}
	if p := s.createdLimit; p.RequestedBy != scAdmin || p.Currency != "USD" || p.LowerLimit != "0" || p.RequestReason != "NEW_ROLE" {
		t.Errorf("store got %+v", p)
	}
}

func activeLimit(principal string) *domain.AuthorityLimit {
	requester := "maker-1"
	return &domain.AuthorityLimit{AuthorityLimitID: "al-1", TenantID: scTenant, PrincipalID: &principal, AuthorityType: "invoice_approval",
		Currency: "GBP", LowerLimit: "0", UpperLimit: "1000", Status: domain.AuthorityLimitActive, RequestedBy: &requester, Version: 3}
}

func TestGoverned_AuthorityLimitCheckerIsNeitherMakerNorHolder(t *testing.T) {
	pending := activeLimit("holder-1")
	pending.Status = domain.AuthorityLimitPending
	for _, who := range []string{"maker-1", "holder-1"} {
		s := &stubStore{authorityLimit: pending}
		if w := postReq(s, noConfig, who, "/v1/admin/authority-limits/al-1/approve", `{"reason_code":"x"}`, false); w.Code != http.StatusForbidden {
			t.Fatalf("%s approving: want 403, got %d", who, w.Code)
		}
		if s.gotGoverned != nil {
			t.Fatalf("%s: reached the store", who)
		}
	}
	s := &stubStore{authorityLimit: pending}
	if w := postReq(s, noConfig, scThird, "/v1/admin/authority-limits/al-1/approve", `{"reason_code":"x"}`, false); w.Code != http.StatusOK || !s.gotGoverned.Approve {
		t.Fatalf("independent checker: %d %+v", w.Code, s.gotGoverned)
	}
	s = &stubStore{authorityLimit: pending, denyAdmin: true}
	if w := postReq(s, noConfig, scThird, "/v1/admin/authority-limits/al-1/approve", `{"reason_code":"x"}`, false); w.Code != http.StatusForbidden {
		t.Fatalf("without iam.authority_limit.approve: want 403, got %d", w.Code)
	}
}

// A change keeps who and where, takes new amounts, and names what it replaces.
func TestGoverned_AuthorityLimitChangeProposesASuccessor(t *testing.T) {
	s := &stubStore{authorityLimit: activeLimit("holder-1")}
	w := postReq(s, noConfig, scAdmin, "/v1/admin/authority-limits/al-1/change",
		`{"upper_limit":"5000","expected_version":3,"reason_code":"PROMOTION","principal_id":"someone-else"}`, false)
	if w.Code != http.StatusAccepted {
		t.Fatalf("change: %d %s", w.Code, w.Body.String())
	}
	p := s.createdLimit
	if p.SupersedesID != "al-1" || *p.PrincipalID != "holder-1" || p.UpperLimit != "5000" || p.LowerLimit != "0" || p.Currency != "GBP" {
		t.Errorf("successor = %+v", p)
	}

	s = &stubStore{authorityLimit: activeLimit("holder-1")}
	if w := postReq(s, noConfig, scAdmin, "/v1/admin/authority-limits/al-1/change", `{"upper_limit":"5000","expected_version":2,"reason_code":"x"}`, false); w.Code != http.StatusConflict {
		t.Fatalf("stale version: want 409, got %d", w.Code)
	}
	retiring := activeLimit("holder-1")
	retire := domain.AuthorityLimitPendingRetire
	retiring.PendingAction = &retire
	s = &stubStore{authorityLimit: retiring}
	if w := postReq(s, noConfig, scAdmin, "/v1/admin/authority-limits/al-1/change", `{"upper_limit":"5000","reason_code":"x"}`, false); w.Code != http.StatusConflict {
		t.Fatalf("change while a retirement is pending: want 409, got %d", w.Code)
	}
}

// Only a limit in force limits: a pending proposal is invisible to decisions.
func TestGoverned_OnlyActiveLimitsApply(t *testing.T) {
	const (
		tenantID    = "11111111-1111-4111-8111-111111111111"
		legalEntity = "22222222-2222-4222-8222-222222222222"
		principalID = "usr-approver-001"
	)
	decide := func(status string) string {
		limit := domain.AuthorityLimit{AuthorityLimitID: "lim-1", TenantID: tenantID, PrincipalID: strptr(principalID),
			AuthorityType: "payment_release", Currency: "GBP", LowerLimit: "0", UpperLimit: "100.00",
			EffectiveFrom: time.Now().Add(-time.Hour), Status: status}
		s := &stubStore{rbacActions: []string{"payment.release"}, rbacBasis: "rbac:role=R", authorityLimits: []domain.AuthorityLimit{limit}}
		body, _ := json.Marshal(domain.CanonicalDecisionRequest{SubjectID: principalID, PrincipalType: "HUMAN", TenantID: tenantID,
			LegalEntityID: legalEntity, Action: "payment.release", CorrelationID: "c",
			ResourceAttributes: map[string]string{"amount": "25000.00", "currency": "GBP"}})
		req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", bytes.NewReader(body))
		req.Header.Set("X-Tenant-Id", tenantID)
		w := httptest.NewRecorder()
		newTestRouterFull(s, &stubPublisher{}, &stubValidator{}).ServeHTTP(w, req)
		var resp domain.CanonicalDecisionResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Decision
	}
	if got := decide(domain.AuthorityLimitActive); got != domain.CanonicalDecisionDeny {
		t.Errorf("an active 100 GBP limit on a 25000 release: %s, want DENY", got)
	}
	for _, status := range []string{domain.AuthorityLimitPending, domain.AuthorityLimitRejected, domain.AuthorityLimitRetired, domain.AuthorityLimitSuperseded} {
		if got := decide(status); got != domain.CanonicalDecisionPermit {
			t.Errorf("a %s limit was applied: %s", status, got)
		}
	}
}
