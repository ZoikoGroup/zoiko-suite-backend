package handler_test

import (
	"bytes"
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
	idgate "zoiko.io/authorization-svc/internal/identity"
	"zoiko.io/authorization-svc/internal/siem"
)

// withStepUp attaches what identity.Gate puts on a request whose envelope
// carries a TOTP step-up made a minute ago: the session tests' happy path.
func withStepUp(req *http.Request, principal string) *http.Request {
	return withAssertion(req, &idgate.Assertion{
		PrincipalID: principal, TenantID: req.Header.Get("X-Tenant-Id"), Basis: idgate.BasisEnvelope,
		MFAVerified: true, AuthTime: time.Now().Add(-time.Minute), SessionContextID: "sc-" + principal,
	})
}

func withAssertion(req *http.Request, a *idgate.Assertion) *http.Request {
	if a == nil {
		return req
	}
	return req.WithContext(idgate.WithAssertion(req.Context(), a))
}

func scPostAs(r chi.Router, caller, path, body string, a *idgate.Assertion) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", scTenant)
	req.Header.Set("X-Principal-Id", caller)
	req = withAssertion(req, a)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// A-03: every way of failing the step-up is refused 401 step_up_required, on
// JIT issue, JIT approval and break-glass issue, and nothing reaches the store.
func TestSessionStepUp_RefusedWithoutARecentSecondFactor(t *testing.T) {
	now := time.Now()
	cases := map[string]*idgate.Assertion{
		"no verified identity": nil,
		// Even carrying MFA facts, a delegate cannot vouch for how recently the
		// person authenticated: only the basis can refuse this one.
		"delegate service": {PrincipalID: scAdmin, Basis: idgate.BasisDelegate, Delegate: "access-control-svc",
			MFAVerified: true, AuthTime: now.Add(-time.Minute)},
		"password only":     {PrincipalID: scAdmin, Basis: idgate.BasisEnvelope, AuthTime: now.Add(-time.Minute)},
		"stale step-up":     {PrincipalID: scAdmin, Basis: idgate.BasisEnvelope, MFAVerified: true, AuthTime: now.Add(-16 * time.Minute)},
		"unknown auth time": {PrincipalID: scAdmin, Basis: idgate.BasisEnvelope, MFAVerified: true},
		"future auth time":  {PrincipalID: scAdmin, Basis: idgate.BasisEnvelope, MFAVerified: true, AuthTime: now.Add(10 * time.Minute)},
		"someone else's":    {PrincipalID: scThird, Basis: idgate.BasisEnvelope, MFAVerified: true, AuthTime: now.Add(-time.Minute)},
	}
	bg := `{"principal_id":"` + scSubject + `","incident_id":"INC-1","reason":"r","requested_actions":["gateway.failover"]}`
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			s := &stubStore{}
			w := scPostAs(scRouter(s, false), scAdmin, "/admin/v1/privileged-sessions", jit(`"ledger.view"`), a)
			assertStepUpRequired(t, "jit issue", w)
			if s.createdPrivilegedParam.PrincipalID != "" {
				t.Fatal("a refused JIT request reached the store")
			}

			w = scPostAs(scRouter(&stubStore{}, false), scAdmin, "/admin/v1/break-glass-sessions", bg, a)
			assertStepUpRequired(t, "break-glass issue", w)

			requester := scThird
			pending := &domain.PrivilegedSession{SessionID: "ps-1", TenantID: scTenant, PrincipalID: scSubject,
				Status: domain.PrivilegedSessionStatusPendingApproval, RequestedBy: &requester}
			s = &stubStore{privilegedSession: pending}
			w = scPostAs(scRouter(s, false), scAdmin, "/admin/v1/privileged-sessions/ps-1/approve", `{}`, a)
			assertStepUpRequired(t, "jit approval", w)
			if s.decidedBy != "" {
				t.Fatal("a refused approval reached the store")
			}
		})
	}
}

func assertStepUpRequired(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("%s: want 401, got %d: %s", what, w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "step_up_required" || body["required_assurance"] != "MFA" {
		t.Fatalf("%s: body %s", what, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "insufficient_user_authentication") {
		t.Fatalf("%s: WWW-Authenticate %q", what, w.Header().Get("WWW-Authenticate"))
	}
}

// A stepped-up request is issued, and the evidence reaches the store.
func TestSessionStepUp_EvidenceIsRecorded(t *testing.T) {
	s := &stubStore{}
	authn := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	a := &idgate.Assertion{PrincipalID: scAdmin, TenantID: scTenant, Basis: idgate.BasisEnvelope,
		MFAVerified: true, AuthTime: authn, SessionContextID: "sc-42"}
	w := scPostAs(scRouter(s, false), scAdmin, "/admin/v1/privileged-sessions", jit(`"ledger.view"`), a)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	ev := s.createdPrivilegedParam.StepUp
	if ev == nil || ev.Assurance != "MFA" || !ev.AuthnAt.Equal(authn) || ev.SessionContextID != "sc-42" {
		t.Fatalf("step-up evidence not passed to the store: %+v", ev)
	}
}

// AUTHZ_SESSION_STEP_UP=off (local only) admits, and records nothing it
// did not verify as a step-up.
func TestSessionStepUp_OffAdmitsLocally(t *testing.T) {
	s := &stubStore{}
	r := scRouterWith(s, func(h *handler.Handler) { h.SetSessionStepUp(handler.SessionStepUpPolicy{Off: true}) })
	w := scPostAs(r, scAdmin, "/admin/v1/privileged-sessions", jit(`"ledger.view"`), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201 with step-up off, got %d: %s", w.Code, w.Body.String())
	}
	if s.createdPrivilegedParam.StepUp != nil {
		t.Fatal("an unverified session was recorded with step-up evidence")
	}
}

// B-01: a request longer than the configured maximum is refused 400, never
// shortened; one at the maximum is accepted.
func TestSessionLimits_RefuseNotShorten(t *testing.T) {
	limits := func(h *handler.Handler) {
		h.SetSessionLimits(handler.SessionLimits{JIT: 28800, BreakGlass: 7200, Support: 14400})
	}
	jitWith := func(d string) string {
		return `{"principal_id":"` + scSubject + `","requested_actions":["ledger.view"],"ticket_ref":"T-1","reason":"r",` +
			`"duration_seconds":` + d + `,"resource_scope":"ledger/2026-10","target_environment":"production"}`
	}
	s := &stubStore{}
	w := scPostAs(scRouterWith(s, limits), scAdmin, "/admin/v1/privileged-sessions", jitWith("28801"), nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "duration_exceeds_maximum") {
		t.Fatalf("JIT one second over: want 400, got %d: %s", w.Code, w.Body.String())
	}
	if s.createdPrivilegedParam.PrincipalID != "" {
		t.Fatal("an over-long request reached the store")
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/privileged-sessions", bytes.NewBufferString(jitWith("28800")))
	req.Header.Set("X-Tenant-Id", scTenant)
	req.Header.Set("X-Principal-Id", scAdmin)
	rec := httptest.NewRecorder()
	scRouterWith(s, limits).ServeHTTP(rec, withStepUp(req, scAdmin))
	if rec.Code != http.StatusCreated || s.createdPrivilegedParam.MaxDurationSeconds != 28800 || s.createdPrivilegedParam.DurationSeconds != 28800 {
		t.Fatalf("JIT at the maximum: %d %+v", rec.Code, s.createdPrivilegedParam)
	}

	bg := `{"principal_id":"` + scSubject + `","incident_id":"INC-1","reason":"r","requested_actions":["gateway.failover"],"duration_seconds":7201}`
	if w := scPostAs(scRouterWith(&stubStore{}, limits), scAdmin, "/admin/v1/break-glass-sessions", bg, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("break-glass one second over: want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// scRouterWith is scRouter with a configuration hook.
func scRouterWith(s handler.AuthorizationStore, configure func(*handler.Handler)) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), platformID, false, zap.NewNop())
	configure(h)
	handler.RegisterRoutes(r, h)
	return r
}
