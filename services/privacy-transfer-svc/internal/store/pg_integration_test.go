package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/privacy-transfer-svc/internal/domain"
	"zoiko.io/privacy-transfer-svc/internal/middleware"
	"zoiko.io/privacy-transfer-svc/internal/store"
	"zoiko.io/privacy-transfer-svc/internal/testdb"
)

const (
	tenantA = "00000000-0000-0000-0000-0000000000a1"
	tenantB = "00000000-0000-0000-0000-0000000000a2"
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
		Key: key, PrincipalID: principal, Endpoint: "op", RequestHash: "h-" + key,
		Render: func(any) (int, []byte, error) { return 201, []byte(`{"ok":true}`), nil },
	}
}

func meta(p string) store.Meta { return store.Meta{PrincipalID: p, CorrelationID: "corr-" + p} }

func newRel(t *testing.T, st *store.PgStore, tenant string) *domain.ProcessorRelationship {
	t.Helper()
	r, ex, err := st.CreateRelationship(ctxFor(tenant), tenant,
		domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s", ProcessingInstructions: "i"},
		meta("p"), idem("k-"+uuid.NewString(), "p"))
	if err != nil || ex != nil {
		t.Fatalf("create relationship: ex=%v err=%v", ex, err)
	}
	return r
}

// ── Gap 1: tenant isolation ──────────────────────────────────────────────────

func TestRLS_TenantsCannotSeeOrTouchEachOther(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	rel := newRel(t, st, tenantA)
	mech, _, err := st.CreateMechanism(ctxFor(tenantA), tenantA, domain.CreateTransferMechanismRequest{MechanismType: "SCC"}, meta("p"), nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.FindRelationship(ctxFor(tenantB), rel.RelationshipID); err == nil {
		t.Fatal("tenant B read tenant A's relationship")
	}
	if _, err := st.FindMechanism(ctxFor(tenantB), mech.MechanismID); err == nil {
		t.Fatal("tenant B read tenant A's mechanism")
	}
	if got, _ := st.ListRelationships(ctxFor(tenantB)); len(got) != 0 {
		t.Fatalf("tenant B listed %d of tenant A's relationships", len(got))
	}
	if _, _, err := st.UpdateRelationshipStatus(ctxFor(tenantB), rel.RelationshipID, domain.RelationshipInactive, meta("p"), nil); err == nil {
		t.Fatal("tenant B changed tenant A's relationship")
	}
	if _, _, err := st.AttachSubprocessor(ctxFor(tenantB), rel.RelationshipID, domain.AttachSubprocessorRequest{ProviderIdentity: "x", Service: "y"}, meta("p"), nil); err == nil {
		t.Fatal("tenant B attached a subprocessor to tenant A's relationship")
	}
	if _, err := st.FindRelationship(context.Background(), rel.RelationshipID); err == nil {
		t.Fatal("the store must refuse a missing tenant scope")
	}
	if _, err := st.FindRelationship(ctxFor(tenantA), rel.RelationshipID); err != nil {
		t.Fatalf("owner cannot read its own relationship: %v", err)
	}
}

func TestMigration000005_ExistingData_NullTenantRowsPreservedAndInvisible(t *testing.T) {
	db := testdb.SetupThrough(t, 4)
	ctx := context.Background()
	nullRel, tenantRel := uuid.NewString(), uuid.NewString()
	for _, row := range []struct {
		id     string
		tenant any
	}{{nullRel, nil}, {tenantRel, tenantA}} {
		if _, err := db.Admin.Exec(ctx, `
			INSERT INTO processor_relationships (relationship_id, tenant_id, controller_ref, processor_ref, service, created_by_principal_id)
			VALUES ($1, $2, 'c', 'p', 's', 'legacy')`, row.id, row.tenant); err != nil {
			t.Fatal(err)
		}
	}
	nullMech := uuid.NewString()
	if _, err := db.Admin.Exec(ctx, `
		INSERT INTO transfer_mechanisms (mechanism_id, tenant_id, mechanism_type, valid_from, created_by_principal_id)
		VALUES ($1, NULL, 'SCC', NOW(), 'legacy')`, nullMech); err != nil {
		t.Fatal(err)
	}

	db.ApplyRemaining(t, 4) // must succeed and delete nothing

	if count(t, db, `SELECT count(*) FROM processor_relationships WHERE tenant_id IS NULL`) != 1 ||
		count(t, db, `SELECT count(*) FROM transfer_mechanisms WHERE tenant_id IS NULL`) != 1 {
		t.Fatal("legacy NULL-tenant rows were deleted or re-assigned")
	}
	st := newStore(db)
	for _, tenant := range []string{tenantA, tenantB} {
		if _, err := st.FindRelationship(ctxFor(tenant), nullRel); err == nil {
			t.Fatalf("NULL-tenant relationship visible to %s", tenant)
		}
		if _, err := st.FindMechanism(ctxFor(tenant), nullMech); err == nil {
			t.Fatalf("NULL-tenant mechanism visible to %s", tenant)
		}
	}
	if _, err := st.FindRelationship(ctxFor(tenantA), tenantRel); err != nil {
		t.Fatalf("tenant row must stay readable: %v", err)
	}
	if _, err := db.Admin.Exec(ctx, `
		INSERT INTO processor_relationships (controller_ref, processor_ref, service, created_by_principal_id)
		VALUES ('c', 'p', 's', 'x')`); err == nil {
		t.Fatal("a new NULL-tenant relationship must be rejected")
	}
	if _, err := db.Admin.Exec(ctx, `DELETE FROM transfer_mechanisms WHERE tenant_id IS NULL`); err == nil {
		t.Fatal("append-only trigger must still reject DELETE")
	}
}

func TestMigration000005_CleanDatabase_StrictPoliciesAndNotNull(t *testing.T) {
	db := testdb.Setup(t)
	ctx := context.Background()
	tables := []string{"processor_relationships", "subprocessors", "transfer_mechanisms", "transfer_assessments", "transfer_decisions"}
	for _, tbl := range tables {
		var nullable string
		if err := db.Admin.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_name=$1 AND column_name='tenant_id'`, tbl).Scan(&nullable); err != nil || nullable != "NO" {
			t.Fatalf("%s.tenant_id must be NOT NULL on a clean database (got %q, %v)", tbl, nullable, err)
		}
	}
	rows, err := db.Admin.Query(ctx, `SELECT tablename, qual, with_check FROM pg_policies WHERE tablename = ANY($1)`, append(tables, "transfer_idempotency_keys", "event_outbox"))
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
	if n < 7 {
		t.Fatalf("expected policies on 7 tables, found %d", n)
	}
}

// ── Gap 3: atomic idempotency ────────────────────────────────────────────────

func TestIdempotency_DuplicateReturnsStoredRecord_NoSecondRow_NoSecondEvent(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	i := idem("dup", "p")
	req := domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s"}
	first, ex, err := st.CreateRelationship(ctxFor(tenantA), tenantA, req, meta("p"), i)
	if err != nil || ex != nil || first == nil {
		t.Fatalf("first: %v %v", ex, err)
	}
	second, ex, err := st.CreateRelationship(ctxFor(tenantA), tenantA, req, meta("p"), i)
	if err != nil || second != nil || ex == nil || ex.ResponseCode != 201 {
		t.Fatalf("duplicate must return the stored record without executing: %v %v %v", second, ex, err)
	}
	if count(t, db, `SELECT count(*) FROM processor_relationships`) != 1 || count(t, db, `SELECT count(*) FROM event_outbox`) != 1 {
		t.Fatal("duplicate created a second relationship or event")
	}
	if _, ex, err := st.CreateRelationship(ctxFor(tenantA), tenantA, req, meta("q"), idem("dup", "q")); err != nil || ex != nil {
		t.Fatalf("principal scoping broken: %v %v", ex, err)
	}
	if _, ex, err := st.CreateRelationship(ctxFor(tenantB), tenantB, req, meta("p"), idem("dup", "p")); err != nil || ex != nil {
		t.Fatalf("tenant scoping broken: %v %v", ex, err)
	}
}

func TestIdempotency_SaveFailureRollsBackRowAndEvent_RetryCreatesOne(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE FUNCTION fail_idem_update() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected idempotency save failure'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER fail_idem_update BEFORE UPDATE ON transfer_idempotency_keys FOR EACH ROW EXECUTE FUNCTION fail_idem_update()`,
	} {
		if _, err := db.Admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	i := idem("save-fail", "p")
	req := domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s"}
	if _, _, err := st.CreateRelationship(ctxFor(tenantA), tenantA, req, meta("p"), i); err == nil {
		t.Fatal("a failed idempotency save must fail the operation")
	}
	if count(t, db, `SELECT count(*) FROM processor_relationships`) != 0 || count(t, db, `SELECT count(*) FROM transfer_idempotency_keys`) != 0 || count(t, db, `SELECT count(*) FROM event_outbox`) != 0 {
		t.Fatal("row, claim and event must all roll back together")
	}
	if _, err := db.Admin.Exec(ctx, `DROP TRIGGER fail_idem_update ON transfer_idempotency_keys`); err != nil {
		t.Fatal(err)
	}
	if _, ex, err := st.CreateRelationship(ctxFor(tenantA), tenantA, req, meta("p"), i); err != nil || ex != nil {
		t.Fatalf("retry: %v %v", ex, err)
	}
	if count(t, db, `SELECT count(*) FROM processor_relationships`) != 1 || count(t, db, `SELECT count(*) FROM event_outbox`) != 1 {
		t.Fatal("retry must produce exactly one relationship and one event")
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
			r, ex, err := st.CreateRelationship(ctxFor(tenantA), tenantA, domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s"}, meta("p"), idem("race", "p"))
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
	if count(t, db, `SELECT count(*) FROM processor_relationships`) != 1 || count(t, db, `SELECT count(*) FROM event_outbox`) != 1 {
		t.Fatal("concurrent identical requests duplicated the row or the event")
	}
}

// ── Gap 7: outbox ────────────────────────────────────────────────────────────

func TestOutbox_EventsEnqueuedAtomicallyWithContext(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	ctx := ctxFor(tenantA)
	rel := newRel(t, st, tenantA) // processor.changed
	if _, _, err := st.UpdateRelationshipStatus(ctx, rel.RelationshipID, domain.RelationshipInactive, meta("actor"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AttachSubprocessor(ctx, rel.RelationshipID, domain.AttachSubprocessorRequest{ProviderIdentity: "x", Service: "y"}, meta("actor"), nil); err != nil {
		t.Fatal(err)
	}
	mech, _, err := st.CreateMechanism(ctx, tenantA, domain.CreateTransferMechanismRequest{MechanismType: "SCC"}, meta("actor"), nil)
	if err != nil {
		t.Fatal(err)
	}
	at := timeIn(48)
	if _, _, err := st.RecordAssessment(ctx, tenantA, domain.RecordTransferAssessmentRequest{RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentApprove}, meta("actor"), nil); err != nil {
		t.Fatal(err)
	}
	if count(t, db, `SELECT count(*) FROM event_outbox WHERE event_type='privacy.assessment.expiring'`) != 0 {
		t.Fatal("an assessment without a review trigger must not emit")
	}
	if _, _, err := st.RecordAssessment(ctx, tenantA, domain.RecordTransferAssessmentRequest{RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentApprove, ReviewTriggerAt: &at}, meta("actor"), nil); err != nil {
		t.Fatal(err)
	}
	d := &domain.TransferDecision{RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID, Result: domain.ResultAuthorized, ReasonCodes: []string{}, ActorPrincipalID: "actor", CorrelationID: "corr-actor"}
	if _, err := st.RecordDecision(ctx, tenantA, d, meta("actor"), nil); err != nil {
		t.Fatal(err)
	}

	for typ, want := range map[string]int{"privacy.processor.changed": 3, "privacy.assessment.expiring": 1, "privacy.transfer.authorization_changed": 1} {
		if got := count(t, db, `SELECT count(*) FROM event_outbox WHERE event_type=$1`, typ); got != want {
			t.Fatalf("%s: got %d events, want %d", typ, got, want)
		}
	}
	if count(t, db, `SELECT count(*) FROM event_outbox WHERE tenant_id <> $1`, tenantA) != 0 {
		t.Fatal("every event must carry the tenant")
	}
	if count(t, db, `SELECT count(*) FROM event_outbox WHERE event_type='privacy.transfer.authorization_changed' AND entity_id=$1 AND actor_id='actor' AND correlation_id='corr-actor'`, d.DecisionID) != 1 {
		t.Fatal("authorization_changed must carry decision id, actor and correlation id")
	}
	// A refused operation leaves no event.
	before := count(t, db, `SELECT count(*) FROM event_outbox`)
	if _, _, err := st.UpdateRelationshipStatus(ctx, uuid.NewString(), domain.RelationshipActive, meta("a"), nil); err == nil {
		t.Fatal("expected not found")
	}
	if count(t, db, `SELECT count(*) FROM event_outbox`) != before {
		t.Fatal("a refused operation enqueued an event")
	}
}

func TestOutbox_FailureRollsBackTheDecision(t *testing.T) {
	db := testdb.Setup(t)
	st := newStore(db)
	ctx := context.Background()
	rel := newRel(t, st, tenantA)
	mech, _, err := st.CreateMechanism(ctxFor(tenantA), tenantA, domain.CreateTransferMechanismRequest{MechanismType: "SCC"}, meta("p"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE FUNCTION fail_outbox() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected outbox failure'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER fail_outbox BEFORE INSERT ON event_outbox FOR EACH ROW EXECUTE FUNCTION fail_outbox()`,
	} {
		if _, err := db.Admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	d := &domain.TransferDecision{RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID, Result: domain.ResultAuthorized, ReasonCodes: []string{}, ActorPrincipalID: "p"}
	if _, err := st.RecordDecision(ctxFor(tenantA), tenantA, d, meta("p"), idem("dec", "p")); err == nil {
		t.Fatal("expected the injected outbox failure")
	}
	if count(t, db, `SELECT count(*) FROM transfer_decisions`) != 0 || count(t, db, `SELECT count(*) FROM transfer_idempotency_keys WHERE idempotency_key='dec'`) != 0 {
		t.Fatal("the decision and its claim must not persist without their event")
	}
}

func timeIn(hours int) time.Time { return time.Now().UTC().Add(time.Duration(hours) * time.Hour) }
