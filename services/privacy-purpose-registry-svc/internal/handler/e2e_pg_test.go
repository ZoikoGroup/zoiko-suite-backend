package handler_test

// End-to-end: HTTP -> handler -> PgStore -> real PostgreSQL under RLS, as the
// restricted application role. These tests prove the WIRING that the
// stub-backed unit tests and the store-level integration tests each only
// prove half of: that the platform-scope sentinel really becomes the
// app.platform_scope capability, that a replay is refused after a grant is
// revoked even with a real idempotency table, and that every committed
// lifecycle change leaves exactly one event and one audit row.

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/handler"
	"zoiko.io/privacy-purpose-registry-svc/internal/middleware"
	"zoiko.io/privacy-purpose-registry-svc/internal/store"
	"zoiko.io/privacy-purpose-registry-svc/internal/testdb"
)

func newPgRouter(t *testing.T, az *stubAuthz) (chi.Router, *testdb.DB) {
	t.Helper()
	db := testdb.Setup(t)
	if db == nil {
		return nil, nil
	}
	h := handler.New(store.NewPgStore(db.App, zap.NewNop()), az, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r, db
}

func queryInt(t *testing.T, db *testdb.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Admin.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestE2E_FullLifecycleOverHTTP_OneEventAndOneAuditRowPerTransition(t *testing.T) {
	r, db := newPgRouter(t, &stubAuthz{})
	if r == nil {
		return
	}
	p := newPurpose(t, r, tenantA, func(c *domain.CreatePurposeRequest) { c.SpecialConditionRefs = []string{"art9-2a"} })
	v1 := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))
	v2 := toActive(t, r, tenantA, newDraftVersion(t, r, tenantA, v1, goodActivity(p.PurposeID)))

	if n := queryInt(t, db, `SELECT count(*) FROM processing_activity_versions WHERE activity_id=$1 AND version_status='ACTIVE'`, v1.ActivityID); n != 1 {
		t.Fatalf("exactly one ACTIVE version required, found %d", n)
	}
	// v1: submitted, approved, activated, retired(by supersession). v2: submitted, approved, activated.
	want := map[string]int{
		"privacy.processing_activity.submitted": 2, "privacy.processing_activity.approved": 2,
		"privacy.processing_activity.activated": 2, "privacy.processing_activity.retired": 1,
	}
	for typ, n := range want {
		if got := queryInt(t, db, `SELECT count(*) FROM event_outbox WHERE event_type=$1 AND entity_id=$2`, typ, v1.ActivityID); got != n {
			t.Errorf("%s: want exactly %d event(s), got %d", typ, n, got)
		}
	}
	// Audit: v1 {validate, submit, approve, activate, supersede}; v2 {validate, submit, approve, activate}.
	if n := queryInt(t, db, `SELECT count(*) FROM activity_lifecycle_audit WHERE activity_version_id=$1`, v1.ActivityVersionID); n != 5 {
		t.Errorf("v1 audit rows: want 5, got %d", n)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM activity_lifecycle_audit WHERE activity_version_id=$1`, v2.ActivityVersionID); n != 4 {
		t.Errorf("v2 audit rows: want 4, got %d", n)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM activity_lifecycle_audit WHERE actor_principal_id IN ('maker-01','checker-01','ops-01')`); n != 9 {
		t.Errorf("every audit row must carry the real acting principal, matched %d of 9", n)
	}
	// The audit trail is readable over HTTP, oldest first, with the real actors; another tenant sees nothing.
	trail := decode[[]domain.ActivityLifecycleAudit](t, call(r, http.MethodGet, activitiesPath+"/"+v1.ActivityID+"/audit", nil, tenantA, "reader"))
	if len(trail) != 9 {
		t.Fatalf("GET audit: want 9 rows for the activity (5 + 4), got %d", len(trail))
	}
	for i := 1; i < len(trail); i++ {
		if trail[i].RecordedAt.Before(trail[i-1].RecordedAt) {
			t.Fatalf("GET audit rows must be oldest first")
		}
	}
	if trail[0].Action != "VALIDATE" || trail[0].ActorPrincipalID == "" {
		t.Fatalf("first audit row must be the validate by a real principal: %+v", trail[0])
	}
	if other := decode[[]domain.ActivityLifecycleAudit](t, call(r, http.MethodGet, activitiesPath+"/"+v1.ActivityID+"/audit", nil, tenantB, "reader")); len(other) != 0 {
		t.Fatalf("another tenant must see no audit rows, got %d", len(other))
	}
	if bad := decode[[]domain.ActivityLifecycleAudit](t, call(r, http.MethodGet, activitiesPath+"/not-a-uuid/audit", nil, tenantA, "reader")); len(bad) != 0 {
		t.Fatalf("a malformed activity id must yield an empty list, got %d", len(bad))
	}
	// The actor on the activated event is the HTTP principal, tenant and correlation carried through.
	if n := queryInt(t, db, `SELECT count(*) FROM event_outbox WHERE event_type='privacy.processing_activity.activated' AND actor_id='ops-01' AND tenant_id=$1 AND correlation_id LIKE 'corr-%'`, tenantA); n != 2 {
		t.Errorf("activated events must carry actor, tenant and correlation id; matched %d of 2", n)
	}
}

func TestE2E_TenantIsolationAndPlatformPlane_OverRealRLS(t *testing.T) {
	r, db := newPgRouter(t, &stubAuthz{})
	if r == nil {
		return
	}
	pa := newPurpose(t, r, tenantA)
	va := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(pa.PurposeID)))

	// Cross-tenant: tenant B cannot see or touch tenant A's records.
	wantErr(t, call(r, http.MethodGet, activitiesPath+"/"+va.ActivityID, nil, tenantB, "reader"), http.StatusNotFound, domain.ErrCodeActivityInactive)
	wantErr(t, call(r, http.MethodPost, versionPath(va, "suspend"), nil, tenantB, ops), http.StatusNotFound, domain.ErrCodeNotFound)

	// The explicit platform plane really creates NULL-tenant rows.
	pp := newPurpose(t, r, platform)
	if n := queryInt(t, db, `SELECT count(*) FROM purposes WHERE purpose_id=$1 AND tenant_id IS NULL`, pp.PurposeID); n != 1 {
		t.Fatalf("platform-plane purpose was not stored as a platform record")
	}
	draft := newDraft(t, r, platform, goodActivity(pp.PurposeID))
	if n := queryInt(t, db, `SELECT count(*) FROM processing_activities WHERE activity_id=$1 AND tenant_id IS NULL`, draft.ActivityID); n != 1 {
		t.Fatalf("platform-plane activity was not stored as a platform record")
	}

	// Both tenants can READ the platform purpose; neither can MUTATE it.
	for _, scope := range []string{tenantA, tenantB} {
		wantOK(t, call(r, http.MethodGet, purposesPath+"/"+pp.PurposeID, nil, scope, "reader"), http.StatusOK)
		wantErr(t, call(r, http.MethodPost, versionPath(draft, "validate"), nil, scope, maker), http.StatusForbidden, domain.ErrCodePlatformScope)
	}
	// The platform plane can drive its own record through the lifecycle.
	toActive(t, r, platform, draft)
	if n := queryInt(t, db, `SELECT count(*) FROM processing_activity_versions WHERE activity_id=$1 AND version_status='ACTIVE'`, draft.ActivityID); n != 1 {
		t.Fatalf("platform activity did not reach ACTIVE")
	}
	// …and tenant events are tenant-tagged while platform events are not.
	if n := queryInt(t, db, `SELECT count(*) FROM event_outbox WHERE entity_id=$1 AND tenant_id=''`, draft.ActivityID); n < 3 {
		t.Fatalf("platform events should carry an empty tenant, found %d", n)
	}
	// A tenant-less request is refused before it reaches the database.
	wantErr(t, call(r, http.MethodGet, "/privacy/ropa", nil, "", "reader"), http.StatusUnauthorized, domain.ErrCodeTenantScopeRequired)
}

func TestE2E_ReplayStoredInRealTable_ButNeverServedAfterRevocation(t *testing.T) {
	az := &stubAuthz{}
	r, db := newPgRouter(t, az)
	if r == nil {
		return
	}
	req := domain.CreatePurposeRequest{Statement: "replay me", CompatibilityClass: "PRIMARY"}
	hdr := map[string]string{"Idempotency-Key": "e2e-key-1"}

	first := call(r, http.MethodPost, purposesPath, req, tenantA, maker, hdr)
	wantOK(t, first, http.StatusCreated)
	second := call(r, http.MethodPost, purposesPath, req, tenantA, maker, hdr)
	wantOK(t, second, http.StatusCreated)
	if second.Header().Get("Idempotency-Replay") != "true" || !sameJSON(t, first.Body.String(), second.Body.String()) {
		t.Fatalf("replay not served from the idempotency table")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM purposes WHERE tenant_id=$1`, tenantA); n != 1 {
		t.Fatalf("replay created %d purposes", n)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM purpose_registry_idempotency_keys WHERE idempotency_key='e2e-key-1'`); n != 1 {
		t.Fatalf("idempotency record not persisted exactly once")
	}

	az.deny = true // grant revoked
	wantErr(t, call(r, http.MethodPost, purposesPath, req, tenantA, maker, hdr), http.StatusForbidden, domain.ErrCodeAuthorizationDenied)
	// Another tenant reusing the key sees no replay.
	az.deny = false
	other := call(r, http.MethodPost, purposesPath, req, tenantB, maker, hdr)
	wantOK(t, other, http.StatusCreated)
	if other.Header().Get("Idempotency-Replay") == "true" {
		t.Fatalf("idempotency record leaked across tenants")
	}
}

func TestE2E_ActivationGateRefusal_LeavesGoverningVersionAndEventsUntouched(t *testing.T) {
	r, db := newPgRouter(t, &stubAuthz{})
	if r == nil {
		return
	}
	p := newPurpose(t, r, tenantA)
	v1 := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))

	// A successor approved with a jurisdiction the registry no longer recognises
	// (forced in as a legacy row; the immutability trigger forbids it via the API).
	next := toApproved(t, r, tenantA, newDraftVersion(t, r, tenantA, v1, goodActivity(p.PurposeID)))
	if _, err := db.Admin.Exec(context.Background(), `ALTER TABLE processing_activity_versions DISABLE TRIGGER activity_versions_content_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Admin.Exec(context.Background(), `UPDATE processing_activity_versions SET jurisdictions='["ZZ"]' WHERE activity_version_id=$1`, next.ActivityVersionID); err != nil {
		t.Fatal(err)
	}
	before := queryInt(t, db, `SELECT count(*) FROM event_outbox`)

	w := call(r, http.MethodPost, versionPath(next, "activate"), nil, tenantA, ops)
	e := wantErr(t, w, http.StatusUnprocessableEntity, "PRV-004")
	if len(e.Findings) == 0 {
		t.Fatalf("refusal carried no findings")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM processing_activity_versions WHERE activity_version_id=$1 AND version_status='ACTIVE'`, v1.ActivityVersionID); n != 1 {
		t.Fatalf("the governing version was retired by a refused activation")
	}
	if after := queryInt(t, db, `SELECT count(*) FROM event_outbox`); after != before {
		t.Fatalf("a refused activation left %d event(s)", after-before)
	}
}
