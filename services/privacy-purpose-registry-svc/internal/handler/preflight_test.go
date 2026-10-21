package handler_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

// readRoutes are every GET route and the authorization action it must demand.
func readRoutes() []struct{ path, action string } {
	id, vid := uuid.NewString(), uuid.NewString()
	return []struct{ path, action string }{
		{purposesPath, "PRIVACY_PURPOSE_READ"},
		{purposesPath + "/" + id, "PRIVACY_PURPOSE_READ"},
		{activitiesPath + "/" + id, "PRIVACY_ACTIVITY_READ"},
		{activitiesPath + "/" + id + "/versions/" + vid, "PRIVACY_ACTIVITY_READ"},
		{"/privacy/ropa", "PRIVACY_ROPA_READ"},
	}
}

// ── item 1: read authentication / authorization ──────────────────────────────

func TestReads_Unauthenticated_Returns401(t *testing.T) {
	st, az := newStubStore(), &stubAuthz{}
	r := newRouter(st, az)
	for _, rt := range readRoutes() {
		w := call(r, http.MethodGet, rt.path, nil, tenantA, "") // no X-Principal-Id
		wantErr(t, w, http.StatusUnauthorized, domain.ErrCodeAuthenticationFailed)
	}
	if len(az.calls) != 0 {
		t.Fatalf("authorization must not even be consulted for an unauthenticated caller: %+v", az.calls)
	}
}

func TestReads_Unauthorized_Returns403_AndAsksForTheReadAction(t *testing.T) {
	st, az := newStubStore(), &stubAuthz{deny: true}
	r := newRouter(st, az)
	for _, rt := range readRoutes() {
		az.reset()
		w := call(r, http.MethodGet, rt.path, nil, tenantA, "intruder-01")
		wantErr(t, w, http.StatusForbidden, domain.ErrCodeAuthorizationDenied)
		got := az.last()
		if got.action != rt.action || got.scope != tenantA || got.principal != "intruder-01" {
			t.Fatalf("%s: authz asked %+v, want action %s scope %s", rt.path, got, rt.action, tenantA)
		}
	}
}

func TestReads_AuthorizationServiceDown_FailsClosed503(t *testing.T) {
	st, az := newStubStore(), &stubAuthz{unavailable: true}
	r := newRouter(st, az)
	for _, rt := range readRoutes() {
		wantErr(t, call(r, http.MethodGet, rt.path, nil, tenantA, "reader-01"), http.StatusServiceUnavailable, domain.ErrCodeAuthzUnavailable)
	}
}

func TestReads_AuthorizedReaderSucceeds(t *testing.T) {
	st, az := newStubStore(), &stubAuthz{}
	r := newRouter(st, az)
	for _, path := range []string{purposesPath, "/privacy/ropa"} {
		wantOK(t, call(r, http.MethodGet, path, nil, tenantA, "reader-01"), http.StatusOK)
	}
}

// ── item 2: tenant scope ─────────────────────────────────────────────────────

func TestMissingTenant_IsRefused_NeverBecomesPlatformScope(t *testing.T) {
	st, az := newStubStore(), &stubAuthz{}
	r := newRouter(st, az)

	for _, rt := range readRoutes() {
		wantErr(t, call(r, http.MethodGet, rt.path, nil, "", "reader-01"), http.StatusUnauthorized, domain.ErrCodeTenantScopeRequired)
	}
	// Mutating routes too — and nothing may be written "platform-wide by omission".
	wantErr(t, call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{Statement: "s", CompatibilityClass: "PRIMARY"}, "", maker),
		http.StatusUnauthorized, domain.ErrCodeTenantScopeRequired)
	wantErr(t, call(r, http.MethodPost, activitiesPath, goodActivity(), "", maker),
		http.StatusUnauthorized, domain.ErrCodeTenantScopeRequired)
	if len(st.purposes)+len(st.activities) != 0 {
		t.Fatalf("a request with no tenant created records: %d purposes, %d activities", len(st.purposes), len(st.activities))
	}
	if len(az.calls) != 0 {
		t.Fatalf("authorization must not be consulted before the tenant is known: %+v", az.calls)
	}
}

func TestBodyTenant_MustMatchVerifiedTenant(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	w := call(r, http.MethodPost, purposesPath,
		domain.CreatePurposeRequest{TenantID: tenantB, Statement: "s", CompatibilityClass: "PRIMARY"}, tenantA, maker)
	wantErr(t, w, http.StatusForbidden, domain.ErrCodeTenantScopeMismatch)
}

// authorization, request context and RLS must all use the same verified scope.
func TestAuthorizationUsesTheVerifiedScope(t *testing.T) {
	az := &stubAuthz{}
	r := newRouter(newStubStore(), az)

	wantOK(t, call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{Statement: "s", CompatibilityClass: "PRIMARY"}, tenantA, maker), http.StatusCreated)
	if got := az.last(); got.scope != tenantA || got.action != "PRIVACY_PURPOSE_CREATE" {
		t.Fatalf("tenant write authorised against %+v", got)
	}
	wantOK(t, call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{Statement: "s", CompatibilityClass: "PRIMARY"}, platform, maker), http.StatusCreated)
	if got := az.last(); got.scope != platform {
		t.Fatalf("platform write authorised against %+v, want scope %s", got, platform)
	}
}

// ── item 3: cross-tenant & platform-record isolation ─────────────────────────

func TestCrossTenant_ReadsAreDenied(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})

	p := newPurpose(t, r, tenantA)
	v := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))

	// Tenant A sees its records…
	wantOK(t, call(r, http.MethodGet, purposesPath+"/"+p.PurposeID, nil, tenantA, "reader-01"), http.StatusOK)
	wantOK(t, call(r, http.MethodGet, activitiesPath+"/"+v.ActivityID, nil, tenantA, "reader-01"), http.StatusOK)

	// …tenant B sees nothing of them.
	wantErr(t, call(r, http.MethodGet, purposesPath+"/"+p.PurposeID, nil, tenantB, "reader-01"), http.StatusNotFound, domain.ErrCodePurposeNotRegistered)
	wantErr(t, call(r, http.MethodGet, activitiesPath+"/"+v.ActivityID, nil, tenantB, "reader-01"), http.StatusNotFound, domain.ErrCodeActivityInactive)
	wantErr(t, call(r, http.MethodGet, versionURL(v), nil, tenantB, "reader-01"), http.StatusNotFound, domain.ErrCodeNotFound)

	list := decode[[]domain.PurposeVersion](t, call(r, http.MethodGet, purposesPath, nil, tenantB, "reader-01"))
	if len(list) != 0 {
		t.Fatalf("tenant B listed tenant A's purposes: %+v", list)
	}
	ropa := decode[struct{ Count int }](t, call(r, http.MethodGet, "/privacy/ropa", nil, tenantB, "reader-01"))
	if ropa.Count != 0 {
		t.Fatalf("tenant B's RoPA contains tenant A's activities")
	}
}

func TestCrossTenant_WritesAreDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	d := newDraft(t, r, tenantA, goodActivity(p.PurposeID))

	// Tenant B targets tenant A's draft: it simply does not exist for B.
	wantErr(t, call(r, http.MethodPost, versionPath(d, "validate"), nil, tenantB, maker), http.StatusNotFound, domain.ErrCodeNotFound)
	wantErr(t, call(r, http.MethodPost, purposesPath+"/"+p.PurposeID+"/versions/"+p.PurposeVersionID+"/publish", nil, tenantB, checker),
		http.StatusNotFound, domain.ErrCodePurposeNotRegistered)
}

func TestTenantCannotMutatePlatformRecords(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})

	// A platform purpose and activity, created from the explicit platform plane.
	pp := newPurpose(t, r, platform)
	if pp.TenantID != nil {
		t.Fatalf("a platform-plane record must have no tenant, got %v", *pp.TenantID)
	}
	draft := newDraft(t, r, platform, goodActivity(pp.PurposeID))
	if draft.TenantID != nil {
		t.Fatalf("a platform-plane activity must have no tenant")
	}
	// A DRAFT purpose version to try to publish.
	w := call(r, http.MethodPost, purposesPath+"/"+pp.PurposeID+"/versions",
		domain.CreatePurposeVersionRequest{ParentVersionID: pp.PurposeVersionID, Statement: "v2", CompatibilityClass: "PRIMARY"}, platform, maker)
	wantOK(t, w, http.StatusCreated)
	pdraft := decode[domain.PurposeVersion](t, w)

	// Tenant A (authorised for every action) tries every mutation of them.
	cases := []struct {
		name, path string
		body       interface{}
	}{
		{"create purpose version", purposesPath + "/" + pp.PurposeID + "/versions",
			domain.CreatePurposeVersionRequest{ParentVersionID: pp.PurposeVersionID, Statement: "x", CompatibilityClass: "PRIMARY"}},
		{"publish purpose version", purposesPath + "/" + pp.PurposeID + "/versions/" + pdraft.PurposeVersionID + "/publish", nil},
		{"create activity version", activitiesPath + "/" + draft.ActivityID + "/versions",
			domain.CreateActivityVersionRequest{ParentVersionID: draft.ActivityVersionID, PrivacyRole: "CONTROLLER", Owner: "o"}},
		{"validate", versionPath(draft, "validate"), nil},
		{"submit", versionPath(draft, "submit"), nil},
		{"approve", versionPath(draft, "approve"), nil},
		{"reject", versionPath(draft, "reject"), domain.RejectActivityRequest{Reason: "no"}},
		{"activate", versionPath(draft, "activate"), nil},
		{"suspend", versionPath(draft, "suspend"), nil},
		{"resume", versionPath(draft, "resume"), nil},
		{"retire", versionPath(draft, "retire"), nil},
		{"validate (latest alias)", activitiesPath + "/" + draft.ActivityID + "/validate", nil},
	}
	for _, c := range cases {
		w := call(r, http.MethodPost, c.path, c.body, tenantA, checker)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: tenant mutated a platform record: HTTP %d %s", c.name, w.Code, w.Body.String())
		}
		wantErr(t, w, http.StatusForbidden, domain.ErrCodePlatformScope)
	}
	// And the platform draft is untouched.
	if st := decode[domain.ProcessingActivityVersion](t, call(r, http.MethodGet, versionURL(draft), nil, platform, "reader-01")).VersionStatus; st != domain.ActivityStatusDraft {
		t.Fatalf("platform draft changed to %s", st)
	}
}

func TestPlatformPlaneCannotMutateTenantRecords(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	d := newDraft(t, r, tenantA, goodActivity(p.PurposeID))
	// A tenant's record is invisible from the platform plane (RLS), so it is a 404, never a write.
	wantErr(t, call(r, http.MethodPost, versionPath(d, "validate"), nil, platform, maker), http.StatusNotFound, domain.ErrCodeNotFound)
}

func TestPlatformWrite_RequiresThePlatformGrant(t *testing.T) {
	az := &stubAuthz{}
	r := newRouter(newStubStore(), az)
	// authorization-svc refuses the platform scope for this principal.
	az.deny = true
	wantErr(t, call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{Statement: "s", CompatibilityClass: "PRIMARY"}, platform, "tenant-admin"),
		http.StatusForbidden, domain.ErrCodeAuthorizationDenied)
}

// ── item 4: idempotency ──────────────────────────────────────────────────────

// mutatingRoutes is one representative of every family of mutating route.
func mutatingRoutes(t *testing.T, r http.Handler) []struct{ name, path string } {
	p := newPurpose(t, r, tenantA)
	d := newDraft(t, r, tenantA, goodActivity(p.PurposeID))
	return []struct{ name, path string }{
		{"create purpose", purposesPath},
		{"create purpose version", purposesPath + "/" + p.PurposeID + "/versions"},
		{"publish purpose", purposesPath + "/" + p.PurposeID + "/versions/" + p.PurposeVersionID + "/publish"},
		{"create activity", activitiesPath},
		{"create activity version", activitiesPath + "/" + d.ActivityID + "/versions"},
		{"validate", versionPath(d, "validate")},
		{"submit", versionPath(d, "submit")},
		{"approve", versionPath(d, "approve")},
		{"reject", versionPath(d, "reject")},
		{"activate", versionPath(d, "activate")},
		{"suspend", versionPath(d, "suspend")},
		{"resume", versionPath(d, "resume")},
		{"retire", versionPath(d, "retire")},
		{"validate (latest alias)", activitiesPath + "/" + d.ActivityID + "/validate"},
		{"activate (latest alias)", activitiesPath + "/" + d.ActivityID + "/activate"},
	}
}

func TestMutations_MissingIdempotencyKey_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	for _, rt := range mutatingRoutes(t, r) {
		w := call(r, http.MethodPost, rt.path, map[string]string{"reason": "x"}, tenantA, checker, map[string]string{"Idempotency-Key": ""})
		wantErr(t, w, http.StatusBadRequest, domain.ErrCodeIdempotencyRequired)
		if t.Failed() {
			t.Fatalf("route %q", rt.name)
		}
	}
}

func TestMutations_MissingCorrelationID_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	for _, rt := range mutatingRoutes(t, r) {
		w := call(r, http.MethodPost, rt.path, map[string]string{"reason": "x"}, tenantA, checker, map[string]string{"X-Correlation-ID": ""})
		wantErr(t, w, http.StatusBadRequest, domain.ErrCodeCorrelationRequired)
		if t.Failed() {
			t.Fatalf("route %q", rt.name)
		}
	}
}

func TestCorrelationID_IsEchoed(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	w := call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{Statement: "s", CompatibilityClass: "PRIMARY"}, tenantA, maker,
		map[string]string{"X-Correlation-ID": "corr-fixed-42"})
	wantOK(t, w, http.StatusCreated)
	if got := w.Header().Get("X-Correlation-ID"); got != "corr-fixed-42" {
		t.Fatalf("correlation id not echoed: %q", got)
	}
}

func TestIdempotency_ReplayReturnsTheOriginal_AndDoesNotDuplicate(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})
	req := domain.CreatePurposeRequest{Statement: "replayed", CompatibilityClass: "PRIMARY"}
	hdr := map[string]string{"Idempotency-Key": "key-replay-1"}

	first := call(r, http.MethodPost, purposesPath, req, tenantA, maker, hdr)
	wantOK(t, first, http.StatusCreated)
	second := call(r, http.MethodPost, purposesPath, req, tenantA, maker, hdr)
	wantOK(t, second, http.StatusCreated)

	if second.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("replay is not marked Idempotency-Replay: true")
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay returned a different body:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	if len(st.purposes) != 1 {
		t.Fatalf("replay created a second purpose (%d)", len(st.purposes))
	}
}

func TestIdempotency_SameKeyDifferentPayload_Returns409(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	hdr := map[string]string{"Idempotency-Key": "key-conflict-1"}
	wantOK(t, call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{Statement: "one", CompatibilityClass: "PRIMARY"}, tenantA, maker, hdr), http.StatusCreated)
	wantErr(t, call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{Statement: "two", CompatibilityClass: "PRIMARY"}, tenantA, maker, hdr),
		http.StatusConflict, domain.ErrCodeIdempotencyMismatch)
}

// The headline §18.1 ordering requirement: a replay is served only AFTER
// authentication and authorization, so a revoked grant cannot replay a success.
func TestIdempotency_ReplayAfterAuthorizationRevoked_IsDenied(t *testing.T) {
	az := &stubAuthz{}
	r := newRouter(newStubStore(), az)
	req := domain.CreatePurposeRequest{Statement: "granted once", CompatibilityClass: "PRIMARY"}
	hdr := map[string]string{"Idempotency-Key": "key-revoke-1"}

	wantOK(t, call(r, http.MethodPost, purposesPath, req, tenantA, maker, hdr), http.StatusCreated)

	az.deny = true // grant revoked
	w := call(r, http.MethodPost, purposesPath, req, tenantA, maker, hdr)
	wantErr(t, w, http.StatusForbidden, domain.ErrCodeAuthorizationDenied)
	if w.Header().Get("Idempotency-Replay") != "" {
		t.Fatalf("a denied caller was served a cached replay")
	}
}

func TestIdempotency_ReplayWithoutAuthentication_IsRefused(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	req := domain.CreatePurposeRequest{Statement: "authn first", CompatibilityClass: "PRIMARY"}
	hdr := map[string]string{"Idempotency-Key": "key-authn-1"}
	wantOK(t, call(r, http.MethodPost, purposesPath, req, tenantA, maker, hdr), http.StatusCreated)
	wantErr(t, call(r, http.MethodPost, purposesPath, req, tenantA, "", hdr), http.StatusUnauthorized, domain.ErrCodeAuthenticationFailed)
}

func TestIdempotency_IsScopedToTenantAndPrincipal(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})
	req := domain.CreatePurposeRequest{Statement: "scoped", CompatibilityClass: "PRIMARY"}
	hdr := map[string]string{"Idempotency-Key": "key-shared"}

	wantOK(t, call(r, http.MethodPost, purposesPath, req, tenantA, maker, hdr), http.StatusCreated)
	// Same key from another tenant, and from another principal, are NOT replays.
	for _, c := range []struct{ tenant, principal string }{{tenantB, maker}, {tenantA, "maker-02"}} {
		w := call(r, http.MethodPost, purposesPath, req, c.tenant, c.principal, hdr)
		wantOK(t, w, http.StatusCreated)
		if w.Header().Get("Idempotency-Replay") == "true" {
			t.Fatalf("idempotency record leaked to %+v", c)
		}
	}
	if len(st.purposes) != 3 {
		t.Fatalf("expected 3 distinct purposes, got %d", len(st.purposes))
	}
}

func TestIdempotency_StoreFailure_FailsClosed(t *testing.T) {
	st := newStubStore()
	st.idempotencyErr = domain.ErrStoreUnavailable
	r := newRouter(st, &stubAuthz{})
	wantErr(t, call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{Statement: "s", CompatibilityClass: "PRIVACY"}, tenantA, maker),
		http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable)
	if len(st.purposes) != 0 {
		t.Fatalf("a request was executed although idempotency could not be checked")
	}
}

// ── item 11: stable §32 error contract ───────────────────────────────────────

func TestErrorContract_EveryRefusalCarriesAStableCode(t *testing.T) {
	st := newStubStore()
	az := &stubAuthz{}
	r := newRouter(st, az)
	p := newPurpose(t, r, tenantA)

	// 400 validation
	wantErr(t, call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{}, tenantA, maker), http.StatusBadRequest, domain.ErrCodeValidationFailed)
	wantErr(t, call(r, http.MethodPost, activitiesPath, domain.CreateActivityRequest{PrivacyRole: "WIZARD", Owner: "o"}, tenantA, maker), http.StatusBadRequest, domain.ErrCodeValidationFailed)
	// 404s carry the §32 code that names the missing thing
	wantErr(t, call(r, http.MethodGet, purposesPath+"/"+uuid.NewString(), nil, tenantA, "r"), http.StatusNotFound, "PRV-001")
	wantErr(t, call(r, http.MethodGet, activitiesPath+"/"+uuid.NewString(), nil, tenantA, "r"), http.StatusNotFound, "PRV-002")
	wantErr(t, call(r, http.MethodPost, activitiesPath+"/"+uuid.NewString()+"/validate", nil, tenantA, maker), http.StatusNotFound, domain.ErrCodeNotFound)
	// 409: re-publishing is an attempted edit of published evidence — PRV-020
	wantErr(t, call(r, http.MethodPost, purposesPath+"/"+p.PurposeID+"/versions/"+p.PurposeVersionID+"/publish", nil, tenantA, "checker-02"),
		http.StatusConflict, "PRV-020")
	// 409: illegal transition
	d := newDraft(t, r, tenantA, goodActivity(p.PurposeID))
	e := wantErr(t, call(r, http.MethodPost, versionPath(d, "activate"), nil, tenantA, ops), http.StatusConflict, domain.ErrCodeInvalidTransition)
	if !strings.Contains(e.Error, "DRAFT") {
		t.Fatalf("a conflict should say what state the version is in: %q", e.Error)
	}
}
