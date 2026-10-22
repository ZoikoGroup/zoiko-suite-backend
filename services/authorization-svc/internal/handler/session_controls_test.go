package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// Session controls (ZS-IAM-001 §13-15; 000030). Each test is a path the final
// re-audit proved open on a running instance, or a control added with it.

const (
	scTenant  = "33333333-3333-4333-8333-333333333333"
	scEntity  = "33333333-3333-4333-8333-eeeeeeeeeee3"
	scAdmin   = "sc-admin"
	scSubject = "sc-operator"
	scThird   = "sc-approver"
)

// ineligibleStore withholds every permission from one principal, so the
// subject-eligibility check can be exercised.
type ineligibleStore struct {
	*stubStore
	ineligible string
}

func (s *ineligibleStore) FindGrantedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error) {
	if principalID == s.ineligible {
		return nil, "", nil
	}
	return s.stubStore.FindGrantedActions(ctx, principalID, legalEntityID, tenantID)
}

func scRouter(s handler.AuthorizationStore, enforce bool) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), platformID, false, zap.NewNop())
	if enforce {
		h.SetCommandContract(handler.CommandContractEnforce)
	}
	handler.RegisterRoutes(r, h)
	return r
}

func scPost(r chi.Router, caller, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", scTenant)
	req.Header.Set("X-Principal-Id", caller)
	// A verified envelope with a recent TOTP step-up, as identity.Gate
	// attaches it; the refusals without one are session_step_up_test.go.
	req = withStepUp(req, caller)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

const jitBody = `{"principal_id":"` + scSubject + `","requested_actions":[%s],"ticket_ref":"T-1","reason":"r",` +
	`"duration_seconds":900,"resource_scope":"ledger/2026-10","target_environment":"production"}`

func jit(actions string) string { return replaceOnce(jitBody, "%s", actions) }

func replaceOnce(s, old, repl string) string {
	i := bytes.Index([]byte(s), []byte(old))
	return s[:i] + repl + s[i+len(old):]
}

func TestSessionControls_WildcardRefused(t *testing.T) {
	r := scRouter(&stubStore{}, false)
	for path, body := range map[string]string{
		"/admin/v1/privileged-sessions":  jit(`"*"`),
		"/admin/v1/break-glass-sessions": `{"principal_id":"` + scSubject + `","incident_id":"INC-1","reason":"r","requested_actions":["*"]}`,
		"/v1/support/sessions":           `{"support_operator_id":"` + scSubject + `","ticket_ref":"S-1","purpose":"p","tenant_consent_obtained":true,"allowed_actions":["*"]}`,
	} {
		if w := scPost(r, scAdmin, path, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s with a wildcard: want 400, got %d: %s", path, w.Code, w.Body.String())
		}
	}
}

func TestSessionControls_JITNeedsEligibleSubjectAndMandatoryFields(t *testing.T) {
	s := &ineligibleStore{stubStore: &stubStore{}, ineligible: scSubject}
	if w := scPost(scRouter(s, false), scAdmin, "/admin/v1/privileged-sessions", jit(`"ledger.view"`)); w.Code != http.StatusForbidden {
		t.Fatalf("ineligible subject: want 403, got %d: %s", w.Code, w.Body.String())
	}
	missing := `{"principal_id":"` + scSubject + `","requested_actions":["ledger.view"],"ticket_ref":"T","reason":"r","duration_seconds":900}`
	if w := scPost(scRouter(&stubStore{}, false), scAdmin, "/admin/v1/privileged-sessions", missing); w.Code != http.StatusBadRequest {
		t.Fatalf("no resource / environment: want 400, got %d", w.Code)
	}
}

// High-impact JIT waits for approval; a low-risk one is active at once.
//
// Governance audit N-08: "high-impact" used to be a denylist of
// administration and payment/posting/filing tokens, so the approvals,
// submissions, bank changes and reopens below went ACTIVE at once,
// self-issuable. Anything that is not a pure read now waits.
func TestSessionControls_HighImpactJITIsPending(t *testing.T) {
	for _, tc := range []struct {
		action string
		code   int
	}{
		{`"PAYMENT_RELEASE"`, http.StatusAccepted}, {`"iam.role.manage"`, http.StatusAccepted},
		{`"PO_APPROVE"`, http.StatusAccepted}, {`"VENDOR_BANK_UPDATE"`, http.StatusAccepted},
		{`"PERIOD_REOPEN"`, http.StatusAccepted}, {`"TAX_RETURN_SUBMIT"`, http.StatusAccepted},
		{`"emergency.override"`, http.StatusAccepted}, {`"payroll.run.execute"`, http.StatusAccepted},
		{`"ledger.view"`, http.StatusCreated}, {`"tax.return.read"`, http.StatusCreated},
	} {
		s := &stubStore{}
		w := scPost(scRouter(s, false), scAdmin, "/admin/v1/privileged-sessions", jit(tc.action))
		if w.Code != tc.code {
			t.Errorf("%s: want %d, got %d: %s", tc.action, tc.code, w.Code, w.Body.String())
		}
		if s.createdPrivilegedParam.RequestedBy != scAdmin {
			t.Errorf("%s: requested_by = %q, want the caller", tc.action, s.createdPrivilegedParam.RequestedBy)
		}
	}
}

func TestSessionControls_ApprovalIsIndependent(t *testing.T) {
	requester := scAdmin
	pending := &domain.PrivilegedSession{SessionID: "ps-1", TenantID: scTenant, PrincipalID: scSubject,
		Status: domain.PrivilegedSessionStatusPendingApproval, RequestedBy: &requester}
	for _, approver := range []string{scSubject, scAdmin} {
		s := &stubStore{privilegedSession: pending}
		if w := scPost(scRouter(s, false), approver, "/admin/v1/privileged-sessions/ps-1/approve", `{}`); w.Code != http.StatusForbidden {
			t.Errorf("approval by %s: want 403, got %d: %s", approver, w.Code, w.Body.String())
		}
		if s.decidedBy != "" {
			t.Errorf("approval by %s reached the store", approver)
		}
	}
	s := &stubStore{privilegedSession: pending}
	w := scPost(scRouter(s, false), scThird, "/admin/v1/privileged-sessions/ps-1/approve", `{}`)
	if w.Code != http.StatusOK || s.decidedBy != scThird || !s.decidedApprove {
		t.Fatalf("independent approval: code %d, decided by %q: %s", w.Code, s.decidedBy, w.Body.String())
	}
}

func TestSessionControls_BreakGlassRefusesFinancialPowers(t *testing.T) {
	body := `{"principal_id":"` + scSubject + `","incident_id":"INC-1","reason":"r","requested_actions":["GL_POST"]}`
	if w := scPost(scRouter(&stubStore{}, false), scAdmin, "/admin/v1/break-glass-sessions", body); w.Code != http.StatusBadRequest {
		t.Fatalf("break-glass GL_POST: want 400, got %d: %s", w.Code, w.Body.String())
	}
	s := &ineligibleStore{stubStore: &stubStore{}, ineligible: scSubject}
	ok := `{"principal_id":"` + scSubject + `","incident_id":"INC-1","reason":"r","requested_actions":["gateway.failover"]}`
	if w := scPost(scRouter(s, false), scAdmin, "/admin/v1/break-glass-sessions", ok); w.Code != http.StatusForbidden {
		t.Fatalf("ineligible break-glass subject: want 403, got %d", w.Code)
	}
}

func TestSessionControls_SupportConsentMustBeStated(t *testing.T) {
	body := `{"support_operator_id":"` + scSubject + `","ticket_ref":"S-1","purpose":"p"}`
	if w := scPost(scRouter(&stubStore{}, false), scAdmin, "/v1/support/sessions", body); w.Code != http.StatusBadRequest {
		t.Fatalf("support session without a consent statement: want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func supportDecision(t *testing.T, ss *domain.SupportSession, action string) string {
	t.Helper()
	ss.SessionID, ss.TenantID, ss.SupportOperatorID = "sup-1", scTenant, scSubject
	ss.Status, ss.ExpiresAt = domain.SupportSessionStatusActive, time.Now().Add(time.Hour)
	w := scPost(scRouter(&stubStore{supportSession: ss}, false), scSubject, "/v1/authorize",
		`{"principal_id":"`+scSubject+`","legal_entity_id":"`+scEntity+`","action_type":"`+action+`","support_session_id":"sup-1"}`)
	var resp struct {
		Outcome string `json:"decision_outcome"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Outcome
}

// The re-audit's live finding: a read-only support session granted
// PAYMENT_RELEASE. Writes are now anything that is not a read verb.
func TestSessionControls_SupportReadOnlyAndNamedWrites(t *testing.T) {
	cases := []struct {
		name    string
		session domain.SupportSession
		action  string
		want    string
	}{
		{"read-only, legacy write", domain.SupportSession{ReadOnly: true, TenantConsentObtained: true}, "PAYMENT_RELEASE", domain.OutcomeDenied},
		{"read-only, read", domain.SupportSession{ReadOnly: true, TenantConsentObtained: true}, "invoice.read", domain.OutcomeGranted},
		{"read-only, named write", domain.SupportSession{ReadOnly: true, TenantConsentObtained: true, AllowedActions: []string{"invoice.edit"}}, "invoice.edit", domain.OutcomeDenied},
		{"writable, unnamed write", domain.SupportSession{TenantConsentObtained: true}, "PAYMENT_RELEASE", domain.OutcomeDenied},
		{"writable, named write", domain.SupportSession{TenantConsentObtained: true, AllowedActions: []string{"invoice.edit"}}, "invoice.edit", domain.OutcomeGranted},
		{"no consent", domain.SupportSession{ReadOnly: true}, "invoice.read", domain.OutcomeDenied},
	}
	for _, tc := range cases {
		ss := tc.session
		if got := supportDecision(t, &ss, tc.action); got != tc.want {
			t.Errorf("%s (%s): got %s, want %s", tc.name, tc.action, got, tc.want)
		}
	}
}

// A pending JIT session grants nothing, and a legacy "*" session no longer
// grants everything.
func TestSessionControls_PendingAndWildcardSessionsGrantNothing(t *testing.T) {
	for name, ps := range map[string]*domain.PrivilegedSession{
		"pending":  {Status: domain.PrivilegedSessionStatusPendingApproval, RequestedActions: []string{"PAYMENT_RELEASE"}},
		"wildcard": {Status: domain.PrivilegedSessionStatusActive, RequestedActions: []string{"*"}},
	} {
		ps.SessionID, ps.TenantID, ps.PrincipalID, ps.ExpiresAt = "ps-1", scTenant, scSubject, time.Now().Add(time.Hour)
		w := scPost(scRouter(&stubStore{privilegedSession: ps}, false), scSubject, "/v1/authorize",
			`{"principal_id":"`+scSubject+`","legal_entity_id":"`+scEntity+`","action_type":"PAYMENT_RELEASE","privileged_session_id":"ps-1"}`)
		if !bytes.Contains(w.Body.Bytes(), []byte(`"decision_outcome":"DENIED"`)) {
			t.Errorf("%s session: want DENIED, got %s", name, w.Body.String())
		}
	}
}

func TestSessionControls_RevokeNeedsReason(t *testing.T) {
	for _, path := range []string{"/admin/v1/privileged-sessions/s-1/revoke", "/admin/v1/break-glass-sessions/s-1/revoke", "/v1/support/sessions/s-1/revoke"} {
		if w := scPost(scRouter(&stubStore{}, true), scAdmin, path, `{}`); w.Code != http.StatusBadRequest {
			t.Errorf("%s without reason (enforce): want 400, got %d: %s", path, w.Code, w.Body.String())
		}
	}
}

func TestSessionControls_ReviewIsIndependent(t *testing.T) {
	requester := scAdmin
	ended := &domain.PrivilegedSession{SessionID: "ps-1", TenantID: scTenant, PrincipalID: scSubject,
		Status: domain.PrivilegedSessionStatusExpired, RequestedBy: &requester, ReviewStatus: domain.SessionReviewPending}
	for _, reviewer := range []string{scSubject, scAdmin} {
		s := &stubStore{privilegedSession: ended}
		if w := scPost(scRouter(s, false), reviewer, "/admin/v1/privileged-sessions/ps-1/review", `{"outcome":"ATTESTED"}`); w.Code != http.StatusForbidden {
			t.Errorf("review by %s: want 403, got %d", reviewer, w.Code)
		}
	}
	s := &stubStore{privilegedSession: ended}
	if w := scPost(scRouter(s, false), scThird, "/admin/v1/privileged-sessions/ps-1/review", `{"outcome":"FLAGGED"}`); w.Code != http.StatusBadRequest {
		t.Errorf("FLAGGED without notes: want 400, got %d", w.Code)
	}
	if w := scPost(scRouter(s, false), scThird, "/admin/v1/privileged-sessions/ps-1/review", `{"outcome":"ATTESTED"}`); w.Code != http.StatusOK || s.reviewedOutcome != domain.SessionReviewAttested {
		t.Errorf("independent review: code %d, outcome %q", w.Code, s.reviewedOutcome)
	}
}

func TestSessionControls_Classifiers(t *testing.T) {
	for a, want := range map[string]bool{"PAYMENT_RELEASE": true, "payment.release": true, "GL_POST": true, "vat_return.file": true, "invoice.read": false, "gateway.failover": false} {
		if got := domain.IsFinancialAction(a); got != want {
			t.Errorf("IsFinancialAction(%q) = %v", a, got)
		}
	}
	for a, want := range map[string]bool{"invoice.read": true, "AUDIT_EVIDENCE_LIST": true, "VENDOR_DD_VIEW": true, "PAYMENT_RELEASE": false, "invoice.edit": false, "read": true} {
		if got := domain.IsReadAction(a); got != want {
			t.Errorf("IsReadAction(%q) = %v", a, got)
		}
	}
}
