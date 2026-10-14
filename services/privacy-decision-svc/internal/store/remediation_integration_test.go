package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/store"
	"zoiko.io/privacy-decision-svc/internal/testdb"
)

const (
	tenantOne = "00000000-0000-0000-0000-0000000000a1"
	tenantTwo = "00000000-0000-0000-0000-0000000000a2"
)

func newDecision(principal string) *domain.PrivacyDecision {
	return &domain.PrivacyDecision{
		SubjectRef: "subject", ProcessingActivityID: "act", PurposeID: "purp",
		ProposedOperation: domain.OperationUse, Result: domain.ResultPermit,
		ReasonCodes: []string{}, PolicyRefs: []string{"CONSENT"},
		ActorPrincipalID: principal, CorrelationID: "corr", InputFingerprint: "v2:abc",
	}
}

func okRender(d *domain.PrivacyDecision) (int, []byte, error) {
	return 200, []byte(`{"decision_id":"` + d.DecisionID + `","actor_principal_id":"` + d.ActorPrincipalID + `"}`), nil
}

func countRows(t *testing.T, db *testdb.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Admin.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// ── Gap 1: migration 000005 must never delete evidence ──────────────────────

func insertLegacyDecision(t *testing.T, db *testdb.DB, tenant any) string {
	t.Helper()
	id := uuid.New().String()
	_, err := db.Admin.Exec(context.Background(), `
		INSERT INTO privacy_decisions (decision_id, tenant_id, subject_ref, processing_activity_id, purpose_id,
			proposed_operation, result, actor_principal_id)
		VALUES ($1, $2, 's', 'a', 'p', 'USE', 'PERMIT', 'legacy-actor')`, id, tenant)
	if err != nil {
		t.Fatalf("insert legacy decision: %v", err)
	}
	return id
}

func TestMigration000005_ExistingData_NullTenantEvidencePreserved(t *testing.T) {
	db := testdb.SetupThrough(t, 4)
	nullRow := insertLegacyDecision(t, db, nil)
	tenantRow := insertLegacyDecision(t, db, tenantOne)

	db.ApplyRemaining(t, 4) // 000005 + 000006 must succeed with legacy NULL rows present

	// Nothing deleted, nothing re-assigned.
	if n := countRows(t, db, `SELECT count(*) FROM privacy_decisions WHERE decision_id = $1 AND tenant_id IS NULL`, nullRow); n != 1 {
		t.Fatalf("legacy NULL-tenant evidence row was deleted or re-assigned (count=%d)", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM privacy_decisions`); n != 2 {
		t.Fatalf("expected both legacy rows preserved, got %d", n)
	}

	// Strict RLS: the app role sees its tenant's row, never the NULL-tenant row.
	st := store.NewPgStore(db.App, zap.NewNop())
	if _, err := st.FindDecision(context.Background(), tenantOne, nullRow); err == nil {
		t.Fatal("NULL-tenant legacy row must be invisible under strict RLS")
	}
	if _, err := st.FindDecision(context.Background(), tenantOne, tenantRow); err != nil {
		t.Fatalf("tenant row must stay readable: %v", err)
	}

	// Legacy rows block NOT NULL, so the column stays nullable (BLOCKED — needs authoritative tenant mapping) …
	var nullable string
	if err := db.Admin.QueryRow(context.Background(), `
		SELECT is_nullable FROM information_schema.columns WHERE table_name='privacy_decisions' AND column_name='tenant_id'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "YES" {
		t.Fatalf("tenant_id must remain nullable while legacy NULL rows exist, got is_nullable=%s", nullable)
	}
	// … but no NEW NULL-tenant row can ever be written.
	if _, err := db.Admin.Exec(context.Background(), `
		INSERT INTO privacy_decisions (subject_ref, processing_activity_id, purpose_id, proposed_operation, result, actor_principal_id)
		VALUES ('s','a','p','USE','PERMIT','x')`); err == nil {
		t.Fatal("new NULL-tenant decision must be rejected by the tenant_id_required constraint")
	}
	// Append-only trigger is untouched: the legacy row still cannot be deleted or updated.
	if _, err := db.Admin.Exec(context.Background(), `DELETE FROM privacy_decisions WHERE decision_id=$1`, nullRow); err == nil {
		t.Fatal("append-only trigger must still reject DELETE")
	}
	if _, err := db.Admin.Exec(context.Background(), `UPDATE privacy_decisions SET tenant_id=$2 WHERE decision_id=$1`, nullRow, tenantOne); err == nil {
		t.Fatal("append-only trigger must still reject UPDATE (no tenant fabrication)")
	}
}

func TestMigration000005_CleanDatabase_TenantIDNotNullAndStrictRLS(t *testing.T) {
	db := testdb.Setup(t)
	var nullable string
	if err := db.Admin.QueryRow(context.Background(), `
		SELECT is_nullable FROM information_schema.columns WHERE table_name='privacy_decisions' AND column_name='tenant_id'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" {
		t.Fatalf("tenant_id must be NOT NULL on a clean database, got is_nullable=%s", nullable)
	}
	for _, tbl := range []string{"privacy_decisions", "decision_idempotency_keys"} {
		var rls, force bool
		if err := db.Admin.QueryRow(context.Background(),
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname=$1`, tbl).Scan(&rls, &force); err != nil {
			t.Fatal(err)
		}
		if !rls || !force {
			t.Fatalf("%s must have ENABLE+FORCE row level security (rls=%v force=%v)", tbl, rls, force)
		}
	}
	rows, err := db.Admin.Query(context.Background(), `SELECT policyname, qual, with_check FROM pg_policies WHERE tablename IN ('privacy_decisions','decision_idempotency_keys')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name string
		var qual, check *string
		if err := rows.Scan(&name, &qual, &check); err != nil {
			t.Fatal(err)
		}
		n++
		for _, e := range []*string{qual, check} {
			if e != nil && strings.Contains(strings.ToUpper(*e), "IS NULL") {
				t.Fatalf("policy %s still has a NULL-tenant exception: %s", name, *e)
			}
		}
	}
	if n == 0 {
		t.Fatal("no RLS policies found")
	}
	if _, err := db.Admin.Exec(context.Background(), `
		INSERT INTO privacy_decisions (subject_ref, processing_activity_id, purpose_id, proposed_operation, result, actor_principal_id)
		VALUES ('s','a','p','USE','PERMIT','x')`); err == nil {
		t.Fatal("NULL-tenant insert must be rejected")
	}
}

func TestMigration000006_BackfillsPrincipalFromStoredResponse(t *testing.T) {
	db := testdb.SetupThrough(t, 5)
	ctx := context.Background()
	for _, row := range []struct{ key, body string }{
		{"k-json", `{"actor_principal_id":"principal-legacy"}`},
		{"k-nonjson", `not json`},
	} {
		if _, err := db.Admin.Exec(ctx, `
			INSERT INTO decision_idempotency_keys (idempotency_key, tenant_id, endpoint, request_hash, response_code, response_body)
			VALUES ($1, $2, 'EvaluateDecision', 'h', 200, convert_to($3,'UTF8'))`, row.key, tenantOne, row.body); err != nil {
			t.Fatal(err)
		}
	}
	db.ApplyRemaining(t, 5)
	var p1, p2 string
	_ = db.Admin.QueryRow(ctx, `SELECT principal_id FROM decision_idempotency_keys WHERE idempotency_key='k-json'`).Scan(&p1)
	_ = db.Admin.QueryRow(ctx, `SELECT principal_id FROM decision_idempotency_keys WHERE idempotency_key='k-nonjson'`).Scan(&p2)
	if p1 != "principal-legacy" || p2 != "" {
		t.Fatalf("backfill wrong: json=%q nonjson=%q", p1, p2)
	}
}

// ── Gap 2: decision + idempotency persist atomically ────────────────────────

func TestCommitDecision_FirstRequestDuplicateConflict(t *testing.T) {
	db := testdb.Setup(t)
	st := store.NewPgStore(db.App, zap.NewNop())
	ctx := context.Background()
	claim := domain.IdempotencyRecord{Key: "k1", TenantID: tenantOne, PrincipalID: "p1", Endpoint: "EvaluateDecision", RequestHash: "h1"}

	d1 := newDecision("p1")
	if ex, err := st.CommitDecision(ctx, claim, d1, okRender); err != nil || ex != nil {
		t.Fatalf("first commit: ex=%v err=%v", ex, err)
	}
	// Duplicate: nothing new persisted, original record returned.
	d2 := newDecision("p1")
	ex, err := st.CommitDecision(ctx, claim, d2, okRender)
	if err != nil || ex == nil {
		t.Fatalf("duplicate must return the stored record: ex=%v err=%v", ex, err)
	}
	if !strings.Contains(string(ex.ResponseBody), d1.DecisionID) || ex.RequestHash != "h1" {
		t.Fatalf("replay record does not belong to the original decision: %s", ex.ResponseBody)
	}
	// Conflict (different hash, same key) returns the ORIGINAL record; the caller maps it to 409.
	conflict := claim
	conflict.RequestHash = "h-other"
	ex, err = st.CommitDecision(ctx, conflict, newDecision("p1"), okRender)
	if err != nil || ex == nil || ex.RequestHash != "h1" {
		t.Fatalf("conflicting key must surface the original record unchanged: ex=%v err=%v", ex, err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM privacy_decisions`); n != 1 {
		t.Fatalf("duplicate/conflict must not create evidence: %d decisions", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM decision_idempotency_keys`); n != 1 {
		t.Fatalf("expected a single idempotency record, got %d", n)
	}
}

func TestCommitDecision_SaveFailureRollsBackDecision_RetrySafe(t *testing.T) {
	db := testdb.Setup(t)
	st := store.NewPgStore(db.App, zap.NewNop())
	ctx := context.Background()

	// Make the idempotency-response save (the UPDATE) fail after the decision insert succeeded.
	for _, stmt := range []string{
		`CREATE FUNCTION fail_idem_update() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected idempotency save failure'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER fail_idem_update BEFORE UPDATE ON decision_idempotency_keys FOR EACH ROW EXECUTE FUNCTION fail_idem_update()`,
	} {
		if _, err := db.Admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	claim := domain.IdempotencyRecord{Key: "k-fail", TenantID: tenantOne, PrincipalID: "p1", Endpoint: "EvaluateDecision", RequestHash: "h"}
	if _, err := st.CommitDecision(ctx, claim, newDecision("p1"), okRender); err == nil {
		t.Fatal("expected the injected save failure to surface as an error")
	}
	if n := countRows(t, db, `SELECT count(*) FROM privacy_decisions`); n != 0 {
		t.Fatalf("decision must be rolled back with the failed idempotency save, found %d", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM decision_idempotency_keys`); n != 0 {
		t.Fatalf("idempotency claim must be rolled back, found %d", n)
	}

	// Retry after the fault clears: succeeds, exactly one decision.
	if _, err := db.Admin.Exec(ctx, `DROP TRIGGER fail_idem_update ON decision_idempotency_keys`); err != nil {
		t.Fatal(err)
	}
	if ex, err := st.CommitDecision(ctx, claim, newDecision("p1"), okRender); err != nil || ex != nil {
		t.Fatalf("retry: ex=%v err=%v", ex, err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM privacy_decisions`); n != 1 {
		t.Fatalf("retry must leave exactly one decision, got %d", n)
	}
}

func TestCommitDecision_RenderFailureRollsBack(t *testing.T) {
	db := testdb.Setup(t)
	st := store.NewPgStore(db.App, zap.NewNop())
	claim := domain.IdempotencyRecord{Key: "k-render", TenantID: tenantOne, PrincipalID: "p1", Endpoint: "EvaluateDecision", RequestHash: "h"}
	_, err := st.CommitDecision(context.Background(), claim, newDecision("p1"), func(*domain.PrivacyDecision) (int, []byte, error) {
		return 0, nil, context.DeadlineExceeded
	})
	if err == nil {
		t.Fatal("expected render failure to fail the commit")
	}
	if countRows(t, db, `SELECT count(*) FROM privacy_decisions`) != 0 || countRows(t, db, `SELECT count(*) FROM decision_idempotency_keys`) != 0 {
		t.Fatal("render failure must persist nothing")
	}
}

func TestCommitDecision_ConcurrentSameKey_OneDecision(t *testing.T) {
	db := testdb.Setup(t)
	st := store.NewPgStore(db.App, zap.NewNop())
	claim := domain.IdempotencyRecord{Key: "k-race", TenantID: tenantOne, PrincipalID: "p1", Endpoint: "EvaluateDecision", RequestHash: "h"}

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners, replays := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ex, err := st.CommitDecision(context.Background(), claim, newDecision("p1"), okRender)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("commit: %v", err)
			case ex == nil:
				winners++
			default:
				replays++
			}
		}()
	}
	wg.Wait()
	if winners != 1 || replays != n-1 {
		t.Fatalf("expected exactly one winner, got winners=%d replays=%d", winners, replays)
	}
	if c := countRows(t, db, `SELECT count(*) FROM privacy_decisions`); c != 1 {
		t.Fatalf("concurrent same-key requests duplicated evidence: %d", c)
	}
}

// ── Gap 15: principal-scoped idempotency ────────────────────────────────────

func TestIdempotency_ScopedByPrincipal(t *testing.T) {
	db := testdb.Setup(t)
	st := store.NewPgStore(db.App, zap.NewNop())
	ctx := context.Background()
	base := domain.IdempotencyRecord{Key: "shared-key", TenantID: tenantOne, Endpoint: "EvaluateDecision", RequestHash: "h"}

	a, b := base, base
	a.PrincipalID, b.PrincipalID = "principal-A", "principal-B"
	da, db2 := newDecision("principal-A"), newDecision("principal-B")
	if ex, err := st.CommitDecision(ctx, a, da, okRender); err != nil || ex != nil {
		t.Fatalf("A: ex=%v err=%v", ex, err)
	}
	// Principal B using the same key must NOT replay A's decision.
	if got, err := st.GetIdempotency(ctx, tenantOne, "principal-B", "shared-key"); err != nil || got != nil {
		t.Fatalf("principal B must not see principal A's record: %v %v", got, err)
	}
	if ex, err := st.CommitDecision(ctx, b, db2, okRender); err != nil || ex != nil {
		t.Fatalf("B must get its own fresh decision: ex=%v err=%v", ex, err)
	}
	ra, _ := st.GetIdempotency(ctx, tenantOne, "principal-A", "shared-key")
	rb, _ := st.GetIdempotency(ctx, tenantOne, "principal-B", "shared-key")
	if ra == nil || rb == nil || strings.Contains(string(ra.ResponseBody), db2.DecisionID) || !strings.Contains(string(rb.ResponseBody), db2.DecisionID) {
		t.Fatalf("records crossed principals: A=%v B=%v", ra, rb)
	}
	// Tenant isolation still holds for the same principal and key.
	if got, _ := st.GetIdempotency(ctx, tenantTwo, "principal-A", "shared-key"); got != nil {
		t.Fatal("tenant two must not see tenant one's idempotency record")
	}
}
