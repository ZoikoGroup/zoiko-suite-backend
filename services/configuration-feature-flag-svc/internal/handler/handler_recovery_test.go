package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/authz"
	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/handler"
	svcmiddleware "zoiko.io/configuration-feature-flag-svc/internal/middleware"
)

func newRecoveryRouter(s *stubStore, az *stubAuthz) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(s, az, testAuthzScopeID, testMetrics(), zap.NewNop()))
	return r
}

// targetRoutes act on an existing change or emergency change. They must be
// authorized by the TARGET's scope: a tenant-scoped writer used to be able to
// approve and activate a global change, or activate a global break-glass one,
// because these routes checked CONFIGURATION_WRITE whatever the target was.
var targetRoutes = []struct {
	name, path, body string
}{
	{"approve change", "/v1/config/changes/c-1/approve", `{"approved":true}`},
	{"activate change", "/v1/config/changes/c-1/activate", `{}`},
	{"rollback change", "/v1/config/changes/c-1/rollback", `{}`},
	{"activate emergency change", "/v1/emergency-changes/e-1/activate", `{}`},
	{"close retrospective", "/v1/emergency-changes/e-1/retrospective", `{"reference":"PIR-1"}`},
}

func TestTargetRoutes_GlobalTargetNeedsGlobalGrant(t *testing.T) {
	for _, rt := range targetRoutes {
		t.Run(rt.name, func(t *testing.T) {
			s := &stubStore{targetScope: nil} // global target
			az := &stubAuthz{err: authz.ErrDenied}
			req := authed(httptest.NewRequest(http.MethodPost, rt.path, strings.NewReader(rt.body)))
			w := httptest.NewRecorder()
			newRecoveryRouter(s, az).ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 without the global grant, got %d: %s", w.Code, w.Body.String())
			}
			if az.actionType != "CONFIGURATION_GLOBAL_WRITE" {
				t.Errorf("a global target must be authorized as CONFIGURATION_GLOBAL_WRITE, asked %q", az.actionType)
			}
		})
	}
}

func TestTargetRoutes_TenantTargetNeedsTenantGrant(t *testing.T) {
	tenant := testTenant
	for _, rt := range targetRoutes {
		t.Run(rt.name, func(t *testing.T) {
			s := &stubStore{targetScope: &tenant}
			az := &stubAuthz{err: authz.ErrDenied}
			req := authed(httptest.NewRequest(http.MethodPost, rt.path, strings.NewReader(rt.body)))
			newRecoveryRouter(s, az).ServeHTTP(httptest.NewRecorder(), req)
			if az.actionType != "CONFIGURATION_WRITE" {
				t.Errorf("the caller's own tenant target is CONFIGURATION_WRITE, asked %q", az.actionType)
			}
		})
	}
}

func TestTargetRoutes_ForeignTargetRefusedBeforeAuthz(t *testing.T) {
	other := otherTenant
	for _, rt := range targetRoutes {
		t.Run(rt.name, func(t *testing.T) {
			s := &stubStore{targetScope: &other}
			az := &stubAuthz{}
			req := authed(httptest.NewRequest(http.MethodPost, rt.path, strings.NewReader(rt.body)))
			w := httptest.NewRecorder()
			newRecoveryRouter(s, az).ServeHTTP(w, req)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "tenant_scope_mismatch") {
				t.Fatalf("expected 403 tenant_scope_mismatch, got %d: %s", w.Code, w.Body.String())
			}
			if az.calls != 0 {
				t.Errorf("a foreign target must be refused without consulting authz")
			}
		})
	}
}

func TestTargetRoutes_UnknownTarget404(t *testing.T) {
	for _, rt := range targetRoutes {
		t.Run(rt.name, func(t *testing.T) {
			s := &stubStore{targetScopeErr: domain.ErrChangeNotFound}
			req := authed(httptest.NewRequest(http.MethodPost, rt.path, strings.NewReader(rt.body)))
			w := httptest.NewRecorder()
			newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// ── kill switch route ───────────────────────────────────────────────────────

func TestKillSwitch_ScopeChoosesTheGrantAndCreates(t *testing.T) {
	cases := []struct {
		name, body, wantAction string
	}{
		{"environment", `{"environment":"production","reason":"errors","safe_behavior":"disable","expires_at":"2030-01-01T00:00:00Z"}`, "FEATURE_FLAG_GLOBAL_WRITE"},
		{"tenant", `{"environment":"production","tenant_id":"` + testTenant + `","reason":"errors","safe_behavior":"DISABLE","expires_at":"2030-01-01T00:00:00Z"}`, "FEATURE_FLAG_WRITE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{killSwitch: &domain.KillSwitch{KillSwitchID: "ks-1", SafeBehavior: domain.SafeBehaviorDisable}}
			az := &stubAuthz{}
			req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags/new_ui/kill-switch", strings.NewReader(tc.body)))
			w := httptest.NewRecorder()
			newRecoveryRouter(s, az).ServeHTTP(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
			}
			if az.actionType != tc.wantAction {
				t.Errorf("expected %s, asked %q", tc.wantAction, az.actionType)
			}
			if s.gotKillSwitch.FlagKey != "new_ui" || s.gotKillSwitch.SafeBehavior != domain.SafeBehaviorDisable ||
				s.gotKillSwitch.CallerTenantID != testTenant || s.gotKillSwitch.ActorPrincipalID != testPrincipal {
				t.Errorf("unexpected params forwarded: %+v", s.gotKillSwitch)
			}
		})
	}
}

func TestKillSwitch_Refusals(t *testing.T) {
	cases := []struct {
		name, body string
		storeErr   error
		want       int
	}{
		{"no expiry", `{"environment":"production","reason":"r","safe_behavior":"DISABLE"}`, nil, http.StatusBadRequest},
		{"no reason", `{"environment":"production","safe_behavior":"DISABLE","expires_at":"2030-01-01T00:00:00Z"}`, nil, http.StatusBadRequest},
		{"foreign tenant", `{"environment":"production","tenant_id":"` + otherTenant + `","reason":"r","safe_behavior":"DISABLE","expires_at":"2030-01-01T00:00:00Z"}`, nil, http.StatusForbidden},
		{"config key", `{"environment":"production","reason":"r","safe_behavior":"DISABLE","expires_at":"2030-01-01T00:00:00Z"}`, domain.ErrNotAFeatureFlag, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{killSwitchErr: tc.storeErr}
			req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags/new_ui/kill-switch", strings.NewReader(tc.body)))
			w := httptest.NewRecorder()
			newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, w.Code, w.Body.String())
			}
		})
	}
}

// ── rollback / retrospective ────────────────────────────────────────────────

func TestRollback_ProposesAndForwardsTarget(t *testing.T) {
	s := &stubStore{rollback: &domain.ConfigChange{ChangeID: "c-2", Status: domain.ChangeStatusProposed}}
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/changes/c-1/rollback", strings.NewReader(`{}`)))
	w := httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
	if w.Code != http.StatusCreated || s.rollbackOf != "c-1" {
		t.Fatalf("expected 201 proposing a rollback of c-1, got %d (%q): %s", w.Code, s.rollbackOf, w.Body.String())
	}
}

func TestRollback_InvalidTarget409(t *testing.T) {
	s := &stubStore{rollbackErr: domain.ErrRollbackTargetInvalid}
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/changes/c-1/rollback", strings.NewReader(`{}`)))
	w := httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 rollback_target_invalid, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRetrospective_RequiresReference(t *testing.T) {
	s := &stubStore{}
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/emergency-changes/e-1/retrospective", strings.NewReader(`{"reference":"  "}`)))
	w := httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || s.gotRetroRef != "" {
		t.Fatalf("expected 400 before the store, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRetrospective_NotPending409(t *testing.T) {
	s := &stubStore{retroErr: domain.ErrRetrospectiveNotPending}
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/emergency-changes/e-1/retrospective", strings.NewReader(`{"reference":"PIR-1"}`)))
	w := httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}
