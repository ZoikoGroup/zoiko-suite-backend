package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// Governance audit N-07: step-up failed open. An absent authentication age
// read as a login zero seconds old, and the caller could raise the age limit
// itself. The policy is now the server's.

func stepUpRouter(s *stubStore, p *handler.StepUpPolicy) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", false, zap.NewNop())
	if p != nil {
		h.SetStepUpPolicy(*p)
	}
	handler.RegisterRoutes(r, h)
	return r
}

func stepUpDecision(t *testing.T, r chi.Router, action, attrs string, headers map[string]string) string {
	t.Helper()
	body := `{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"` + action +
		`","tenant_id":"11111111-1111-4111-8111-111111111111","attributes":{` + attrs + `}}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", action, w.Code, w.Body.String())
	}
	outcome, _ := decodeDecision(t, w)
	return outcome
}

func TestStepUp_ServerPolicy(t *testing.T) {
	grants := []string{"PERIOD_REOPEN_APPROVE", "PAYMENT_RELEASE"}
	permitUnknown := handler.StepUpPolicy{MaxAuthnAgeSeconds: 300, UnknownAgeStepsUp: false}
	listed := handler.StepUpPolicy{Actions: []string{"payment_release"}, MaxAuthnAgeSeconds: 300, UnknownAgeStepsUp: true}

	cases := []struct {
		name    string
		policy  *handler.StepUpPolicy
		action  string
		attrs   string
		headers map[string]string
		want    string
	}{
		{"reopen, no age stated → step up (was GRANTED)", nil, "PERIOD_REOPEN_APPROVE", ``, nil, "STEP_UP"},
		{"reopen, no age, permit configured → granted", &permitUnknown, "PERIOD_REOPEN_APPROVE", ``, nil, "GRANTED"},
		{"reopen, fresh age in header → granted", nil, "PERIOD_REOPEN_APPROVE", ``, map[string]string{"X-Authn-Age-Seconds": "60"}, "GRANTED"},
		{"reopen, stale age → step up", nil, "PERIOD_REOPEN_APPROVE", `"authn_age_seconds":"400"`, nil, "STEP_UP"},
		{"caller cannot loosen the limit", nil, "PERIOD_REOPEN_APPROVE", `"authn_age_seconds":"400","max_authn_age_seconds":"100000"`, nil, "STEP_UP"},
		{"caller may tighten the limit", nil, "PERIOD_REOPEN_APPROVE", `"authn_age_seconds":"100","max_authn_age_seconds":"60"`, nil, "STEP_UP"},
		{"server-listed action, no age → step up", &listed, "PAYMENT_RELEASE", ``, nil, "STEP_UP"},
		{"unlisted action is not stepped up", nil, "PAYMENT_RELEASE", ``, nil, "GRANTED"},
	}
	for _, c := range cases {
		s := &stubStore{rbacActions: grants, rbacBasis: "rbac:role=R"}
		if got := stepUpDecision(t, stepUpRouter(s, c.policy), c.action, c.attrs, c.headers); got != c.want {
			t.Errorf("%s: outcome %s, want %s", c.name, got, c.want)
		}
	}
}

// The canonical API learns whether environment.authn_age_seconds was sent at
// all: 0 sent is a fresh login, absent is unknown.
func TestStepUp_CanonicalEnvironmentAgePresence(t *testing.T) {
	for body, want := range map[string]string{
		`"environment":{"authn_age_seconds":0}`: "PERMIT",
		`"environment":{}`:                      "STEP_UP",
	} {
		s := &stubStore{rbacActions: []string{"PERIOD_REOPEN_APPROVE"}, rbacBasis: "rbac:role=R"}
		req := httptest.NewRequest(http.MethodPost, handler.CanonicalDecisionsPath, bytes.NewBufferString(
			`{"subject_id":"p-1","action":"PERIOD_REOPEN_APPROVE","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","tenant_id":"11111111-1111-4111-8111-111111111111",`+body+`}`))
		w := httptest.NewRecorder()
		stepUpRouter(s, nil).ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", body, w.Code, w.Body.String())
		}
		if !bytes.Contains(w.Body.Bytes(), []byte(`"decision":"`+want+`"`)) {
			t.Errorf("%s: want decision %s, got %s", body, want, w.Body.String())
		}
	}
}

// Second-pass scan, 13 Oct 2026: a stated age that is not a non-negative
// whole number parsed as 0 and counted as a login zero seconds old, so it
// passed step-up. It is now an unknown age, decided by the policy.
func TestStepUp_MalformedOrNegativeAgeIsUnknown(t *testing.T) {
	grants := []string{"PERIOD_REOPEN_APPROVE"}
	cases := []struct {
		name    string
		attrs   string
		headers map[string]string
	}{
		{"non-numeric header", ``, map[string]string{"X-Authn-Age-Seconds": "abc"}},
		{"negative header", ``, map[string]string{"X-Authn-Age-Seconds": "-600"}},
		{"non-numeric attribute", `"authn_age_seconds":"fresh"`, nil},
		{"negative attribute", `"authn_age_seconds":"-1"`, nil},
	}
	for _, c := range cases {
		s := &stubStore{rbacActions: grants, rbacBasis: "rbac:role=R"}
		if got := stepUpDecision(t, stepUpRouter(s, nil), "PERIOD_REOPEN_APPROVE", c.attrs, c.headers); got != "STEP_UP" {
			t.Errorf("%s: outcome %s, want STEP_UP", c.name, got)
		}
	}
}
