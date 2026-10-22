package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/privacy-rights-svc/internal/domain"
	"zoiko.io/privacy-rights-svc/internal/middleware"
	"zoiko.io/privacy-rights-svc/internal/store"
	"zoiko.io/privacy-rights-svc/internal/testdb"
)

const (
	tenantA = "00000000-0000-0000-0000-0000000000a1"
	tenantB = "00000000-0000-0000-0000-0000000000a2"
	digest  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func ctxFor(tenant string) context.Context {
	return middleware.WithTenant(context.Background(), tenant)
}

func newStore(db *testdb.DB) *store.PgStore { return store.NewPgStore(db.App, zap.NewNop()) }

func count(t *testing.T, db *testdb.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Admin.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", q, err)
	}
	return n
}

func idem(key, principal string) *store.Idem {
	return &store.Idem{
		Key: key, PrincipalID: principal, Endpoint: "CreateRequest", RequestHash: "h-" + key,
		Render: func(any) (int, []byte, error) { return 201, []byte(`{"ok":true}`), nil },
	}
}

func create(t *testing.T, st *store.PgStore, tenant string) *domain.RightsRequest {
	t.Helper()
	r, ex, err := st.CreateRequest(ctxFor(tenant), tenant,
		domain.CreateRightsRequestRequest{SubjectRef: "subject", RightFamily: domain.RightAccess}, "p", idem("k-"+uuid.NewString(), "p"))
	if err != nil || ex != nil {
		t.Fatalf("create: ex=%v err=%v", ex, err)
	}
	return r
}

// ── Gap 1: tenant isolation ──────────────────────────────────────────────────

func TestRLS_TenantsCannotSeeEachOther(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	a := create(t, st, tenantA)
	if _, err := st.FindRequest(ctxFor(tenantB), a.RequestID); err == nil {
		t.Fatal("tenant B read tenant A's case")
	}
	if got, _ := st.ListRequestsBySubject(ctxFor(tenantB), "subject"); len(got) != 0 {
		t.Fatalf("tenant B listed %d of tenant A's cases", len(got))
	}
	if _, err := st.FindRequest(ctxFor(tenantA), a.RequestID); err != nil {
		t.Fatalf("owner cannot read its own case: %v", err)
	}
	// Cross-tenant writes: tenant B cannot touch tenant A's case.
	if _, _, err := st.AttachWFCProcessRef(ctxFor(tenantB), a.RequestID, "wf", nil); err == nil {
		t.Fatal("tenant B wrote to tenant A's case")
	}
	if _, _, err := st.CloseRequest(ctxFor(tenantB), a.RequestID, domain.CloseRequestRequest{Outcome: domain.OutcomeRejected}, "p", nil); err == nil {
		t.Fatal("tenant B closed tenant A's case")
	}
	// A request with no tenant scope is refused by the store itself.
	if _, err := st.FindRequest(context.Background(), a.RequestID); err == nil {
		t.Fatal("store must refuse a missing tenant scope")
	}
}

func TestMigration000005_ExistingData_NullTenantRowsPreservedAndInvisible(t *testing.T) {
	db := testdb.SetupThrough(t, 4)
	ctx := context.Background()
	var nullReq, tenantReq string = uuid.NewString(), uuid.NewString()
	for _, row := range []struct {
		id     string
		tenant any
	}{{nullReq, nil}, {tenantReq, tenantA}} {
		if _, err := db.Admin.Exec(ctx, `
			INSERT INTO rights_requests (request_id, tenant_id, subject_ref, right_family, status, identity_verified, response_package_version, created_by_principal_id)
			VALUES ($1, $2, 's', 'ACCESS', 'RECEIVED', false, 0, 'legacy')`, row.id, row.tenant); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Admin.Exec(ctx, `
		INSERT INTO discovery_manifests (manifest_id, tenant_id, request_id, domain, content_hash, candidate_count, submitted_by_principal_id)
		VALUES (gen_random_uuid(), NULL, $1, 'crm', 'x', 0, 'legacy')`, nullReq); err != nil {
		t.Fatal(err)
	}

	db.ApplyRemaining(t, 4) // must succeed; must not delete anything

	if n := count(t, db, `SELECT count(*) FROM rights_requests WHERE tenant_id IS NULL`); n != 1 {
		t.Fatalf("legacy NULL-tenant case was deleted or re-assigned (count=%d)", n)
	}
	if n := count(t, db, `SELECT count(*) FROM discovery_manifests WHERE tenant_id IS NULL`); n != 1 {
		t.Fatalf("legacy NULL-tenant evidence was deleted (count=%d)", n)
	}
	st := newStore(db)
	for _, tenant := range []string{tenantA, tenantB} {
		if _, err := st.FindRequest(ctxFor(tenant), nullReq); err == nil {
			t.Fatalf("NULL-tenant case is visible to %s", tenant)
		}
		if got, _ := st.ListDiscoveryManifests(ctxFor(tenant), nullReq); len(got) != 0 {
			t.Fatalf("NULL-tenant manifest is visible to %s", tenant)
		}
	}
	if _, err := st.FindRequest(ctxFor(tenantA), tenantReq); err != nil {
		t.Fatalf("tenant case must stay readable: %v", err)
	}
	// tenant_id stays nullable while legacy rows exist, but no NEW NULL row can be written.
	if _, err := db.Admin.Exec(ctx, `
		INSERT INTO rights_requests (request_id, subject_ref, right_family, status, identity_verified, response_package_version, created_by_principal_id)
		VALUES (gen_random_uuid(), 's', 'ACCESS', 'RECEIVED', false, 0, 'x')`); err == nil {
		t.Fatal("a new NULL-tenant case must be rejected")
	}
	// Append-only evidence is still protected.
	if _, err := db.Admin.Exec(ctx, `DELETE FROM discovery_manifests WHERE tenant_id IS NULL`); err == nil {
		t.Fatal("append-only trigger must still reject DELETE")
	}
}

func TestMigration000005_CleanDatabase_StrictPoliciesAndNotNull(t *testing.T) {
	db := testdb.Setup(t)
	ctx := context.Background()
	for _, tbl := range []string{"rights_requests", "identity_verification_events", "discovery_manifests"} {
		var nullable string
		if err := db.Admin.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_name=$1 AND column_name='tenant_id'`, tbl).Scan(&nullable); err != nil || nullable != "NO" {
			t.Fatalf("%s.tenant_id must be NOT NULL on a clean database (got %q, %v)", tbl, nullable, err)
		}
	}
	rows, err := db.Admin.Query(ctx, `SELECT tablename, qual, with_check FROM pg_policies WHERE tablename IN ('rights_requests','identity_verification_events','discovery_manifests','rights_idempotency_keys')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var tbl string
		var q, c *string
		if err := rows.Scan(&tbl, &q, &c); err != nil {
			t.Fatal(err)
		}
		n++
		for _, e := range []*string{q, c} {
			if e != nil && strings.Contains(strings.ToUpper(*e), "IS NULL") {
				t.Fatalf("policy on %s still has a NULL-tenant exception: %s", tbl, *e)
			}
		}
	}
	if n < 4 {
		t.Fatalf("expected policies on 4 tables, found %d", n)
	}
}

// ── Gap 3: atomic idempotency ────────────────────────────────────────────────

func TestIdempotency_DuplicateReturnsStoredRecord_NoSecondCase(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	i := idem("dup", "p")
	first, ex, err := st.CreateRequest(ctxFor(tenantA), tenantA, domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}, "p", i)
	if err != nil || ex != nil || first == nil {
		t.Fatalf("first: %v %v", ex, err)
	}
	second, ex, err := st.CreateRequest(ctxFor(tenantA), tenantA, domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}, "p", i)
	if err != nil || second != nil || ex == nil || ex.ResponseCode != 201 {
		t.Fatalf("duplicate must return the stored record without executing: %v %v %v", second, ex, err)
	}
	if n := count(t, db, `SELECT count(*) FROM rights_requests`); n != 1 {
		t.Fatalf("duplicate created a second case: %d", n)
	}
	// Another principal with the same key is a different claim.
	if _, ex, err := st.CreateRequest(ctxFor(tenantA), tenantA, domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}, "q", idem("dup", "q")); err != nil || ex != nil {
		t.Fatalf("principal scoping broken: %v %v", ex, err)
	}
	// Another tenant with the same key is a different claim.
	if _, ex, err := st.CreateRequest(ctxFor(tenantB), tenantB, domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}, "p", idem("dup", "p")); err != nil || ex != nil {
		t.Fatalf("tenant scoping broken: %v %v", ex, err)
	}
}

func TestIdempotency_SaveFailureRollsBackCase_RetryCreatesOne(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE FUNCTION fail_idem_update() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected idempotency save failure'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER fail_idem_update BEFORE UPDATE ON rights_idempotency_keys FOR EACH ROW EXECUTE FUNCTION fail_idem_update()`,
	} {
		if _, err := db.Admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	i := idem("save-fail", "p")
	if _, _, err := st.CreateRequest(ctxFor(tenantA), tenantA, domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}, "p", i); err == nil {
		t.Fatal("a failed idempotency save must fail the operation")
	}
	if count(t, db, `SELECT count(*) FROM rights_requests`) != 0 || count(t, db, `SELECT count(*) FROM rights_idempotency_keys`) != 0 {
		t.Fatal("case and claim must roll back together")
	}
	if _, err := db.Admin.Exec(ctx, `DROP TRIGGER fail_idem_update ON rights_idempotency_keys`); err != nil {
		t.Fatal(err)
	}
	if _, ex, err := st.CreateRequest(ctxFor(tenantA), tenantA, domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}, "p", i); err != nil || ex != nil {
		t.Fatalf("retry: %v %v", ex, err)
	}
	if count(t, db, `SELECT count(*) FROM rights_requests`) != 1 {
		t.Fatal("retry must produce exactly one case")
	}
}

func TestIdempotency_ConcurrentIdenticalRequests_ExecuteOnce(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners, replays := 0, 0
	for k := 0; k < n; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, ex, err := st.CreateRequest(ctxFor(tenantA), tenantA, domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}, "p", idem("race", "p"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("create: %v", err)
			case r != nil:
				winners++
			case ex != nil:
				replays++
			}
		}()
	}
	wg.Wait()
	if winners != 1 || replays != n-1 {
		t.Fatalf("winners=%d replays=%d", winners, replays)
	}
	if count(t, db, `SELECT count(*) FROM rights_requests`) != 1 {
		t.Fatal("concurrent identical requests duplicated the case")
	}
}

// ── Gap 4: disclosure gate ───────────────────────────────────────────────────

func TestClose_Fulfilled_RequiresVerifiedEventAndManifest(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	ctx := ctxFor(tenantA)
	c := create(t, st, tenantA)

	// Forged denormalised flag with no verified event: must NOT pass.
	if _, err := db.Admin.Exec(context.Background(), `UPDATE rights_requests SET identity_verified = true WHERE request_id = $1`, c.RequestID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CloseRequest(ctx, c.RequestID, domain.CloseRequestRequest{Outcome: domain.OutcomeFulfilled}, "p", nil); err != domain.ErrIdentityNotVerified {
		t.Fatalf("forged flag passed the gate: %v", err)
	}
	// A FAILED verification attempt does not satisfy it either.
	if _, _, err := st.RecordIdentityVerification(ctx, c.RequestID, domain.RecordIdentityVerificationRequest{Verified: false, Method: "m"}, "p", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CloseRequest(ctx, c.RequestID, domain.CloseRequestRequest{Outcome: domain.OutcomeFulfilled}, "p", nil); err != domain.ErrIdentityNotVerified {
		t.Fatalf("failed verification passed the gate: %v", err)
	}
	if _, _, err := st.RecordIdentityVerification(ctx, c.RequestID, domain.RecordIdentityVerificationRequest{Verified: true, Method: "m"}, "p", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CloseRequest(ctx, c.RequestID, domain.CloseRequestRequest{Outcome: domain.OutcomeFulfilled}, "p", nil); err != domain.ErrNoDiscoveryManifest {
		t.Fatalf("expected no-manifest refusal, got %v", err)
	}
	if _, _, err := st.AttachDiscoveryManifest(ctx, c.RequestID, domain.AttachDiscoveryManifestRequest{Domain: "crm", ContentHash: digest, CandidateCount: 2}, "p", nil); err != nil {
		t.Fatal(err)
	}
	closed, _, err := st.CloseRequest(ctx, c.RequestID, domain.CloseRequestRequest{Outcome: domain.OutcomeFulfilled}, "p", nil)
	if err != nil || closed.ResponsePackageVersion != 1 || closed.Status != domain.StatusClosed {
		t.Fatalf("close: %+v %v", closed, err)
	}
}

func TestClosedCase_AllWritesRefusedAs409Sentinel(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	ctx := ctxFor(tenantA)
	c := create(t, st, tenantA)
	if _, _, err := st.CloseRequest(ctx, c.RequestID, domain.CloseRequestRequest{Outcome: domain.OutcomeWithdrawn}, "p", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordIdentityVerification(ctx, c.RequestID, domain.RecordIdentityVerificationRequest{Verified: true, Method: "m"}, "p", nil); err != domain.ErrRequestAlreadyClosed {
		t.Fatalf("identity on closed: %v", err)
	}
	if _, _, err := st.AttachDiscoveryManifest(ctx, c.RequestID, domain.AttachDiscoveryManifestRequest{Domain: "d", ContentHash: digest}, "p", nil); err != domain.ErrRequestAlreadyClosed {
		t.Fatalf("manifest on closed: %v", err)
	}
	if _, _, err := st.AttachWFCProcessRef(ctx, c.RequestID, "wf", nil); err != domain.ErrRequestAlreadyClosed {
		t.Fatalf("wfc on closed: %v", err)
	}
	if _, _, err := st.CloseRequest(ctx, c.RequestID, domain.CloseRequestRequest{Outcome: domain.OutcomeRejected}, "p", nil); err != domain.ErrRequestAlreadyClosed {
		t.Fatalf("second close: %v", err)
	}
}
