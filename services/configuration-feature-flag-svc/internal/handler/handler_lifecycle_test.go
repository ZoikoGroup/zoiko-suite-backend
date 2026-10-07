package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
)

// §10.1 attest "workload identity": a runtime attests only for itself.
func TestAttest_RuntimeMustBeTheCallingWorkload(t *testing.T) {
	body := `{"runtime_id":"rt-1","attest_key":"k1","environment":"production","observed_epoch":1,"observed_digest":"d"}`
	cases := []struct {
		name, workload string
		want           int
		wantCode       string
	}{
		{"no workload identity", "", http.StatusUnauthorized, "workload_identity_missing"},
		{"another workload's runtime id", "rt-2", http.StatusForbidden, "runtime_identity_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{}
			req := authed(httptest.NewRequest(http.MethodPost, "/v1/runtime/attest", strings.NewReader(body)))
			req.Header.Set("X-Workload-Id", tc.workload)
			w := httptest.NewRecorder()
			newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
			if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.wantCode) {
				t.Fatalf("expected %d %s, got %d: %s", tc.want, tc.wantCode, w.Code, w.Body.String())
			}
			if s.gotAttestation.RuntimeID != "" {
				t.Errorf("a refused attestation must not reach the store")
			}
		})
	}
}

// NP-08: the plan the evaluation uses is the gateway's header, not the body;
// the region is the gateway-verified jurisdiction.
func TestEvaluate_TrustedPlanAndRegionComeFromHeaders(t *testing.T) {
	s := &stubStore{evaluation: &domain.FlagEvaluation{Key: "f"}}
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/flags/f/evaluate",
		strings.NewReader(`{"environment":"production","subject_key":"u-1","context":{"plan":"enterprise"}}`)))
	req.Header.Set("X-Commercial-Plan", "starter")
	req.Header.Set("X-Jurisdiction-Context", "EU")
	w := httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotEvaluate.TrustedPlan != "starter" || s.gotEvaluate.Region != "EU" {
		t.Errorf("expected trusted plan starter / region EU from headers, got %q / %q", s.gotEvaluate.TrustedPlan, s.gotEvaluate.Region)
	}
}

func TestResolve_RegionFromJurisdictionHeader(t *testing.T) {
	s := &stubStore{resolveResult: &domain.ResolvedConfigSnapshot{}}
	req := scoped(httptest.NewRequest(http.MethodPost, "/v1/config/resolve", strings.NewReader(`{"environment":"production"}`)))
	req.Header.Set("X-Jurisdiction-Context", "EU")
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(httptest.NewRecorder(), req)
	if s.gotResolve.Region != "EU" {
		t.Errorf("expected region EU, got %q", s.gotResolve.Region)
	}
}

// §10.1 publish approval binding: the envelope's X-Approval-Reference is
// accepted when the body does not carry one.
func TestPublish_ApprovalReferenceFromEnvelope(t *testing.T) {
	s := &stubStore{
		getDefinitionResult: &domain.ConfigDefinition{DefinitionID: "d-1"},
		publishVersion:      &domain.ConfigDefinitionVersion{VersionID: "v-1"},
	}
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/definitions/k/publish", strings.NewReader(`{"lifecycle":"PUBLISHED"}`)))
	req.Header.Set("X-Approval-Reference", "CAB-7")
	w := httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
	if s.gotPublishDefinition.ApprovalReference != "CAB-7" {
		t.Fatalf("expected the envelope approval reference forwarded, got %q (%d %s)", s.gotPublishDefinition.ApprovalReference, w.Code, w.Body.String())
	}
}

func TestPublish_MissingApprovalIs409(t *testing.T) {
	s := &stubStore{getDefinitionResult: &domain.ConfigDefinition{DefinitionID: "d-1"}, publishErr: domain.ErrApprovalReferenceRequired}
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/definitions/k/publish", strings.NewReader(`{"lifecycle":"PUBLISHED"}`)))
	w := httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 approval_reference_required, got %d: %s", w.Code, w.Body.String())
	}
}

// INV-21: retire and removal routes.
func TestRetireAndRemoval(t *testing.T) {
	s := &stubStore{retirement: &domain.FlagRetirement{RetirementID: "r-1"}}
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/flags/legacy/retire", strings.NewReader(`{"environment":"production","final_enabled":false}`)))
	w := httptest.NewRecorder()
	az := &stubAuthz{}
	newRecoveryRouter(s, az).ServeHTTP(w, req)
	if w.Code != http.StatusCreated || s.gotRetire.Key != "legacy" || s.gotRetire.FinalEnabled || s.gotRetire.FinalRollout != 100 {
		t.Fatalf("retire: %d %+v %s", w.Code, s.gotRetire, w.Body.String())
	}
	if az.actionType != "FEATURE_FLAG_GLOBAL_WRITE" {
		t.Errorf("retiring a global flag needs the global grant, asked %q", az.actionType)
	}

	missing := authed(httptest.NewRequest(http.MethodPost, "/v1/flags/legacy/removal", strings.NewReader(`{"environment":"production"}`)))
	w = httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, missing)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "consumer_scan_evidence") {
		t.Fatalf("removal without evidence: expected 400, got %d: %s", w.Code, w.Body.String())
	}

	ok := authed(httptest.NewRequest(http.MethodPost, "/v1/flags/legacy/removal",
		strings.NewReader(`{"environment":"production","consumer_scan_evidence":{"references":0}}`)))
	w = httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, ok)
	if w.Code != http.StatusOK || s.gotRemoval.Key != "legacy" {
		t.Fatalf("removal: %d %s", w.Code, w.Body.String())
	}

	s.retireErr = domain.ErrFlagNotRetired
	w = httptest.NewRecorder()
	again := authed(httptest.NewRequest(http.MethodPost, "/v1/flags/legacy/removal",
		strings.NewReader(`{"environment":"production","consumer_scan_evidence":{"references":0}}`)))
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, again)
	if w.Code != http.StatusConflict {
		t.Fatalf("removing a key not retired: expected 409, got %d", w.Code)
	}
}

// INV-07: the override route takes all five layers; SERVICE (tenantless) needs
// the global grant, ORG_UNIT and USER_PREFERENCE the tenant grant.
func TestOverride_ExtendedLayersAndTheirGrants(t *testing.T) {
	cases := []struct {
		scope, body, wantAction string
	}{
		{"service", `{"key":"k","environment":"production","value":1,"scope_id":"reports-svc"}`, "CONFIGURATION_GLOBAL_WRITE"},
		{"org_unit", `{"key":"k","environment":"production","value":1,"scope_id":"ou-1"}`, "CONFIGURATION_WRITE"},
		{"user_preference", `{"key":"k","environment":"production","value":1,"scope_id":"principal-test-admin"}`, "CONFIGURATION_WRITE"},
	}
	for _, tc := range cases {
		t.Run(tc.scope, func(t *testing.T) {
			s := &stubStore{overrideEntry: &domain.ConfigEntry{ConfigID: "o-1"}}
			az := &stubAuthz{}
			req := authed(httptest.NewRequest(http.MethodPut, "/v1/config/overrides/"+tc.scope, strings.NewReader(tc.body)))
			w := httptest.NewRecorder()
			newRecoveryRouter(s, az).ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			if az.actionType != tc.wantAction {
				t.Errorf("expected %s, asked %q", tc.wantAction, az.actionType)
			}
			if s.gotOverride.ScopeID == nil || *s.gotOverride.ScopeID == "" {
				t.Errorf("scope id not forwarded: %+v", s.gotOverride)
			}
		})
	}
	s := &stubStore{}
	req := authed(httptest.NewRequest(http.MethodPut, "/v1/config/overrides/org_unit", strings.NewReader(`{"key":"k","environment":"production","value":1}`)))
	w := httptest.NewRecorder()
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an extended layer without scope_id: expected 400, got %d", w.Code)
	}
}

func TestResolve_LayerIdentitiesFromTrustedHeaders(t *testing.T) {
	s := &stubStore{resolveResult: &domain.ResolvedConfigSnapshot{}}
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/config/resolve", strings.NewReader(`{"environment":"production"}`)))
	req.Header.Set("X-Workload-Id", "reports-svc")
	req.Header.Set("X-Org-Unit-Id", "ou-finance")
	newRecoveryRouter(s, &stubAuthz{}).ServeHTTP(httptest.NewRecorder(), req)
	g := s.gotResolve
	if g.Service != "reports-svc" || g.OrgUnit != "ou-finance" || g.Subject != testPrincipal {
		t.Errorf("expected service/org unit/subject from headers, got %q %q %q", g.Service, g.OrgUnit, g.Subject)
	}
}
