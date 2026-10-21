package store_test

// Real-PostgreSQL integration tests. Every test gets a fresh database with
// every *.up.sql migration applied and talks to it as a NOSUPERUSER,
// NOBYPASSRLS role (testdb.Setup refuses to continue otherwise), so the row
// level security under test is the same RLS the service runs under. The
// Admin pool is a superuser used ONLY to inspect state and to attempt
// writes "around" the application.
//
// Set REQUIRE_DB_TESTS=1 to make an unreachable database a failure instead
// of a skip.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/events"
	"zoiko.io/privacy-purpose-registry-svc/internal/middleware"
	"zoiko.io/privacy-purpose-registry-svc/internal/store"
	"zoiko.io/privacy-purpose-registry-svc/internal/testdb"
)

const (
	tenantA  = "11111111-1111-1111-1111-111111111111"
	tenantB  = "22222222-2222-2222-2222-222222222222"
	platform = domain.PlatformScopeID
)

// tctx is a request context for a scope: a tenant id, or the platform-scope
// sentinel (which middleware.WithTenant also flags as an explicit platform
// context, exactly as the HTTP middleware does).
func tctx(scope string) context.Context { return middleware.WithTenant(context.Background(), scope) }

// recordTenant is the tenant a row is stamped with for a scope ("" = platform).
func recordTenant(scope string) string {
	if scope == platform {
		return ""
	}
	return scope
}

func newStore(t *testing.T) (*store.PgStore, *testdb.DB) {
	t.Helper()
	db := testdb.Setup(t)
	if db == nil {
		return nil, nil
	}
	return store.NewPgStore(db.App, zap.NewNop()), db
}

// ── fixtures ─────────────────────────────────────────────────────────────────

func mkPurpose(t *testing.T, st *store.PgStore, scope string) *domain.PurposeVersion {
	t.Helper()
	ctx := tctx(scope)
	p, v, err := st.CreatePurpose(ctx, recordTenant(scope), domain.CreatePurposeRequest{
		Statement: "Process shipping addresses", CompatibilityClass: "PRIMARY",
		LawfulBasisRefs: []string{"basis-contract"}, NecessityRationale: "address needed to ship",
		SpecialConditionRefs: []string{"art9-2a"},
	}, "user-1")
	if err != nil {
		t.Fatalf("CreatePurpose: %v", err)
	}
	pub, err := st.PublishPurposeVersion(ctx, p.PurposeID, v.PurposeVersionID, "user-2", "corr-publish")
	if err != nil {
		t.Fatalf("PublishPurposeVersion: %v", err)
	}
	return pub
}

func activityReq(purposeID string) domain.CreateActivityRequest {
	return domain.CreateActivityRequest{
		PrivacyRole: string(domain.RoleController), Owner: "logistics", PrivacyOwner: "dpo",
		PurposeIDs: []string{purposeID}, SubjectClasses: []string{"CUSTOMER"}, DataCategories: []string{"CONTACT_INFO"},
		Sources: []string{"checkout"}, Recipients: []string{"carrier"}, Jurisdictions: []string{"GB"},
		RetentionRuleRefs: []string{"ret-1y"}, TransferRefs: []string{"xfer-1"}, ProcessorRefs: []string{"proc-1"},
		NoticeConsentDependency: string(domain.NoticeConsentNotRequired), DPIATIAStatus: string(domain.DPIATIANotRequired),
	}
}

func versionReq(parent *domain.ProcessingActivityVersion, purposeID string) domain.CreateActivityVersionRequest {
	r := activityReq(purposeID)
	return domain.CreateActivityVersionRequest{
		ParentVersionID: parent.ActivityVersionID, PrivacyRole: r.PrivacyRole, Owner: r.Owner, PrivacyOwner: r.PrivacyOwner,
		PurposeIDs: r.PurposeIDs, SubjectClasses: r.SubjectClasses, DataCategories: r.DataCategories, Sources: r.Sources,
		Recipients: r.Recipients, Jurisdictions: r.Jurisdictions, RetentionRuleRefs: r.RetentionRuleRefs,
		TransferRefs: r.TransferRefs, ProcessorRefs: r.ProcessorRefs,
		NoticeConsentDependency: r.NoticeConsentDependency, DPIATIAStatus: r.DPIATIAStatus,
	}
}

func mustT[T any](t *testing.T, what string) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return v
	}
}

// approve drives a DRAFT to APPROVED: validate (user-1) -> submit (user-1) -> approve (user-2).
func approve(t *testing.T, st *store.PgStore, ctx context.Context, v *domain.ProcessingActivityVersion, corr string) *domain.ProcessingActivityVersion {
	t.Helper()
	mustT[*domain.ProcessingActivityVersion](t, "validate")(st.SetValidationOutcome(ctx, v.ActivityID, v.ActivityVersionID, nil, "user-1", corr+"-validate"))
	mustT[*domain.ProcessingActivityVersion](t, "submit")(st.TransitionActivity(ctx, v.ActivityID, v.ActivityVersionID, domain.ActivityStatusValidated, domain.ActivityStatusSubmitted, "user-1", corr+"-submit"))
	return mustT[*domain.ProcessingActivityVersion](t, "approve")(st.TransitionActivity(ctx, v.ActivityID, v.ActivityVersionID, domain.ActivityStatusSubmitted, domain.ActivityStatusApproved, "user-2", corr+"-approve"))
}

func activate(t *testing.T, st *store.PgStore, ctx context.Context, v *domain.ProcessingActivityVersion, corr string) *domain.ProcessingActivityVersion {
	t.Helper()
	return mustT[*domain.ProcessingActivityVersion](t, "activate")(st.ActivateActivity(ctx, v.ActivityID, v.ActivityVersionID, "user-3", corr))
}

// firstActive creates activity + first version and takes it to ACTIVE.
func firstActive(t *testing.T, st *store.PgStore, scope string, p *domain.PurposeVersion) (*domain.ProcessingActivity, *domain.ProcessingActivityVersion) {
	t.Helper()
	ctx := tctx(scope)
	a, v, err := st.CreateActivity(ctx, recordTenant(scope), activityReq(p.PurposeID), "user-1")
	if err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}
	return a, activate(t, st, ctx, approve(t, st, ctx, v, "c1"), "c1-activate")
}

// ── inspection helpers (superuser; bypasses RLS) ─────────────────────────────

type outboxRow struct {
	Type, Entity, Tenant, Actor, Corr string
	Published                         bool
	Payload                           map[string]any
}

func outbox(t *testing.T, db *testdb.DB, where string, args ...any) []outboxRow {
	t.Helper()
	q := `SELECT event_type, entity_id, tenant_id, actor_id, correlation_id, published_at IS NOT NULL, payload FROM event_outbox`
	if where != "" {
		q += " WHERE " + where
	}
	rows, err := db.Admin.Query(context.Background(), q+" ORDER BY created_at, outbox_event_id", args...)
	if err != nil {
		t.Fatalf("outbox query: %v", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var raw []byte
		if err := rows.Scan(&r.Type, &r.Entity, &r.Tenant, &r.Actor, &r.Corr, &r.Published, &raw); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(raw, &r.Payload)
		out = append(out, r)
	}
	return out
}

func countByType(rows []outboxRow) map[string]int {
	m := map[string]int{}
	for _, r := range rows {
		m[r.Type]++
	}
	return m
}

func countActive(t *testing.T, db *testdb.DB, activityID string) int {
	t.Helper()
	var n int
	if err := db.Admin.QueryRow(context.Background(),
		`SELECT count(*) FROM processing_activity_versions WHERE activity_id=$1 AND version_status='ACTIVE'`, activityID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// session opens a transaction on the RESTRICTED pool with the RLS session
// variables a request would carry, runs fn, and always rolls back.
func session(t *testing.T, db *testdb.DB, tenant string, platformCap bool, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.App.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	pc := "false"
	if platformCap {
		pc = "true"
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id',$1,true), set_config('app.platform_scope',$2,true)", tenant, pc); err != nil {
		t.Fatal(err)
	}
	fn(tx)
}

func isRLSViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

// ── item 7: exactly one ACTIVE version ───────────────────────────────────────

func TestActivation_RetiresPrevious_OneActive_UniqueIndexBacksItUp(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v1 := firstActive(t, st, tenantA, p)

	v2 := mustT[*domain.ProcessingActivityVersion](t, "create v2")(st.CreateActivityVersion(ctx, act.ActivityID, versionReq(v1, p.PurposeID), "user-1"))
	v2 = activate(t, st, ctx, approve(t, st, ctx, v2, "c2"), "c2-activate")

	v1Post := mustT[*domain.ProcessingActivityVersion](t, "find v1")(st.FindActivityVersion(ctx, act.ActivityID, v1.ActivityVersionID))
	if v1Post.VersionStatus != domain.ActivityStatusRetired {
		t.Fatalf("v1 should be RETIRED, is %s", v1Post.VersionStatus)
	}
	// History preserved and the valid-time windows abut exactly: no gap, no overlap.
	if v1Post.EffectiveTo == nil || !v1Post.EffectiveTo.Equal(*v2.EffectiveFrom) {
		t.Fatalf("v1.effective_to (%v) must equal v2.effective_from (%v)", v1Post.EffectiveTo, v2.EffectiveFrom)
	}
	if v1Post.ActivatedByPrincipalID == nil || v1Post.ApprovedByPrincipalID == nil {
		t.Fatalf("superseded version lost its lifecycle evidence: %+v", v1Post)
	}
	if n := countActive(t, db, act.ActivityID); n != 1 {
		t.Fatalf("exactly one ACTIVE version required, found %d", n)
	}
	ropa := mustT[[]domain.ProcessingActivityVersion](t, "ropa")(st.ListActiveActivities(ctx, "", ""))
	if len(ropa) != 1 || ropa[0].ActivityVersionID != v2.ActivityVersionID {
		t.Fatalf("RoPA must list only v2, got %+v", ropa)
	}

	// Non-bypassable: even a superuser forcing a second ACTIVE row is refused.
	_, err := db.Admin.Exec(context.Background(), `UPDATE processing_activity_versions SET version_status='ACTIVE' WHERE activity_version_id=$1`, v1.ActivityVersionID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("uq_activity_versions_one_active must reject a second ACTIVE row with 23505, got %v", err)
	}
}

func TestActivation_ConcurrentActivations_LeaveExactlyOneActive(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v1, err := st.CreateActivity(ctx, tenantA, activityReq(p.PurposeID), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	versions := []*domain.ProcessingActivityVersion{approve(t, st, ctx, v1, "k1")}
	for i := 0; i < 3; i++ {
		v := mustT[*domain.ProcessingActivityVersion](t, "successor")(st.CreateActivityVersion(ctx, act.ActivityID, versionReq(versions[0], p.PurposeID), "user-1"))
		versions = append(versions, approve(t, st, ctx, v, "k"+string(rune('2'+i))))
	}

	var wg sync.WaitGroup
	var failures atomic.Int32
	for _, v := range versions {
		wg.Add(1)
		go func(v *domain.ProcessingActivityVersion) {
			defer wg.Done()
			if _, err := st.ActivateActivity(ctx, v.ActivityID, v.ActivityVersionID, "user-3", "race"); err != nil {
				failures.Add(1)
				t.Errorf("concurrent activation failed: %v", err)
			}
		}(v)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.FailNow()
	}

	if n := countActive(t, db, act.ActivityID); n != 1 {
		t.Fatalf("concurrent activation must leave exactly one ACTIVE version, found %d", n)
	}
	var retired int
	_ = db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM processing_activity_versions WHERE activity_id=$1 AND version_status='RETIRED'`, act.ActivityID).Scan(&retired)
	if retired != 3 {
		t.Fatalf("the three superseded versions must be RETIRED, found %d", retired)
	}
	// Four activations, three of which superseded a predecessor: 4 + 3 events, none lost, none doubled.
	c := countByType(outbox(t, db, "entity_id=$1", act.ActivityID))
	if c["privacy.processing_activity.activated"] != 4 || c["privacy.processing_activity.retired"] != 3 {
		t.Fatalf("event counts wrong after the race: %+v", c)
	}
}

func TestActivation_OfANonApprovedVersion_DoesNotRetireTheGoverningOne(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v1 := firstActive(t, st, tenantA, p)
	draft := mustT[*domain.ProcessingActivityVersion](t, "draft")(st.CreateActivityVersion(ctx, act.ActivityID, versionReq(v1, p.PurposeID), "user-1"))

	before := countByType(outbox(t, db, ""))
	if _, err := st.ActivateActivity(ctx, act.ActivityID, draft.ActivityVersionID, "user-3", "bad"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
	if n := countActive(t, db, act.ActivityID); n != 1 {
		t.Fatalf("the governing version was retired by a refused activation (active=%d)", n)
	}
	got := mustT[*domain.ProcessingActivityVersion](t, "find")(st.FindActivityVersion(ctx, act.ActivityID, v1.ActivityVersionID))
	if got.VersionStatus != domain.ActivityStatusActive {
		t.Fatalf("v1 is %s", got.VersionStatus)
	}
	after := countByType(outbox(t, db, ""))
	if before["privacy.processing_activity.retired"] != after["privacy.processing_activity.retired"] {
		t.Fatalf("a refused activation emitted a retired event")
	}
}

func TestResume_CollidingWithAnotherActiveVersion_IsAStateConflictNotAnOutage(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v1 := firstActive(t, st, tenantA, p)
	if _, err := st.TransitionActivity(ctx, act.ActivityID, v1.ActivityVersionID, domain.ActivityStatusActive, domain.ActivityStatusSuspended, "user-3", "s"); err != nil {
		t.Fatal(err)
	}
	v2 := mustT[*domain.ProcessingActivityVersion](t, "v2")(st.CreateActivityVersion(ctx, act.ActivityID, versionReq(v1, p.PurposeID), "user-1"))
	// Force a second ACTIVE version "around" the application (a legacy row).
	if _, err := db.Admin.Exec(context.Background(), `UPDATE processing_activity_versions SET version_status='ACTIVE', effective_from=NOW() WHERE activity_version_id=$1`, v2.ActivityVersionID); err != nil {
		t.Fatal(err)
	}
	_, err := st.TransitionActivity(ctx, act.ActivityID, v1.ActivityVersionID, domain.ActivityStatusSuspended, domain.ActivityStatusActive, "user-3", "r")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("resume into a collision must be ErrInvalidTransition, got %v", err)
	}
}

// ── item 9: exactly one event per transition; payload completeness ───────────

func TestEvents_ExactlyOnePerLifecycleTransition(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v, err := st.CreateActivity(ctx, tenantA, activityReq(p.PurposeID), "user-1")
	if err != nil {
		t.Fatal(err)
	}

	// validate: audit evidence but NO event (not in §19).
	mustT[*domain.ProcessingActivityVersion](t, "validate")(st.SetValidationOutcome(ctx, act.ActivityID, v.ActivityVersionID, nil, "user-1", "e-validate"))
	mustT[*domain.ProcessingActivityVersion](t, "submit")(st.TransitionActivity(ctx, act.ActivityID, v.ActivityVersionID, domain.ActivityStatusValidated, domain.ActivityStatusSubmitted, "user-1", "e-submit"))
	mustT[*domain.ProcessingActivityVersion](t, "approve")(st.TransitionActivity(ctx, act.ActivityID, v.ActivityVersionID, domain.ActivityStatusSubmitted, domain.ActivityStatusApproved, "user-2", "e-approve"))
	activate(t, st, ctx, v, "e-activate")
	mustT[*domain.ProcessingActivityVersion](t, "suspend")(st.TransitionActivity(ctx, act.ActivityID, v.ActivityVersionID, domain.ActivityStatusActive, domain.ActivityStatusSuspended, "user-3", "e-suspend"))
	mustT[*domain.ProcessingActivityVersion](t, "resume")(st.TransitionActivity(ctx, act.ActivityID, v.ActivityVersionID, domain.ActivityStatusSuspended, domain.ActivityStatusActive, "user-3", "e-resume"))
	mustT[*domain.ProcessingActivityVersion](t, "retire")(st.TransitionActivity(ctx, act.ActivityID, v.ActivityVersionID, domain.ActivityStatusActive, domain.ActivityStatusRetired, "user-3", "e-retire"))

	// A second activity exercises reject.
	_, v2, _ := st.CreateActivity(ctx, tenantA, activityReq(p.PurposeID), "user-1")
	mustT[*domain.ProcessingActivityVersion](t, "validate2")(st.SetValidationOutcome(ctx, v2.ActivityID, v2.ActivityVersionID, nil, "user-1", "r-validate"))
	mustT[*domain.ProcessingActivityVersion](t, "submit2")(st.TransitionActivity(ctx, v2.ActivityID, v2.ActivityVersionID, domain.ActivityStatusValidated, domain.ActivityStatusSubmitted, "user-1", "r-submit"))
	mustT[*domain.ProcessingActivityVersion](t, "reject")(st.RejectActivity(ctx, v2.ActivityID, v2.ActivityVersionID, "incomplete", "user-2", "r-reject"))

	// A failed validation changes nothing observable but the findings.
	_, v3, _ := st.CreateActivity(ctx, tenantA, activityReq(p.PurposeID), "user-1")
	mustT[*domain.ProcessingActivityVersion](t, "validate3")(st.SetValidationOutcome(ctx, v3.ActivityID, v3.ActivityVersionID,
		[]domain.ValidationFinding{{Code: "PRV-003", Field: "owner", Message: "x"}}, "user-1", "f-validate"))

	events := outbox(t, db, "event_type LIKE 'privacy.processing_activity.%'")
	got := countByType(events)
	want := map[string]int{
		"privacy.processing_activity.submitted": 2, "privacy.processing_activity.approved": 1,
		"privacy.processing_activity.activated": 1, "privacy.processing_activity.suspended": 1,
		"privacy.processing_activity.resumed": 1, "privacy.processing_activity.retired": 1,
		"privacy.processing_activity.rejected": 1,
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: want exactly %d event(s), got %d", k, n, got[k])
		}
	}
	if len(events) != 8 {
		t.Errorf("want 8 lifecycle events in total, got %d: %+v", len(events), got)
	}
	// No (entity, type, correlation) pair is ever enqueued twice.
	seen := map[string]bool{}
	for _, e := range events {
		k := e.Entity + "|" + e.Type + "|" + e.Corr
		if seen[k] {
			t.Errorf("duplicate event %s", k)
		}
		seen[k] = true
	}
}

func TestEvents_PayloadCarriesTheRequiredFields_AndSupersessionIsAnnounced(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v1 := firstActive(t, st, tenantA, p)
	v2 := mustT[*domain.ProcessingActivityVersion](t, "v2")(st.CreateActivityVersion(ctx, act.ActivityID, versionReq(v1, p.PurposeID), "user-1"))
	v2 = activate(t, st, ctx, approve(t, st, ctx, v2, "d2"), "d2-activate")

	rows := outbox(t, db, "entity_id=$1", act.ActivityID)
	var activated2, retired1 *outboxRow
	for i := range rows {
		r := &rows[i]
		pl := r.Payload["payload"].(map[string]any)
		switch {
		case r.Type == "privacy.processing_activity.activated" && pl["activity_version_id"] == v2.ActivityVersionID:
			activated2 = r
		case r.Type == "privacy.processing_activity.retired" && pl["activity_version_id"] == v1.ActivityVersionID:
			retired1 = r
		}
	}
	if activated2 == nil {
		t.Fatal("no activated event for v2")
	}
	if retired1 == nil {
		t.Fatal("§19/§9.2: superseding an ACTIVE version must emit its retired event — none found")
	}

	pl := activated2.Payload["payload"].(map[string]any)
	for _, field := range []string{"activity_id", "activity_version_id", "tenant_id", "effective_from", "privacy_role", "purpose_ids", "policy_refs"} {
		if v, ok := pl[field]; !ok || v == nil || v == "" {
			t.Errorf("activated event is missing %q: %+v", field, pl)
		}
	}
	if activated2.Tenant != tenantA || activated2.Payload["tenant_id"] != tenantA {
		t.Errorf("tenant missing from the envelope: row=%q envelope=%v", activated2.Tenant, activated2.Payload["tenant_id"])
	}
	refs := toStrings(pl["policy_refs"])
	for _, want := range []string{"basis-contract", "art9-2a", "ret-1y", "xfer-1", "proc-1"} {
		if !contains(refs, want) {
			t.Errorf("policy_refs lacks %q: %v", want, refs)
		}
	}
	if activated2.Actor != "user-3" || activated2.Corr != "d2-activate" {
		t.Errorf("actor/correlation wrong: %q %q", activated2.Actor, activated2.Corr)
	}

	rp := retired1.Payload["payload"].(map[string]any)
	if rp["previous_status"] != "ACTIVE" || rp["superseded_by_version_id"] != v2.ActivityVersionID || rp["effective_to"] == nil {
		t.Errorf("retired event does not describe the supersession: %+v", rp)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ── item 9: events survive a broker outage ───────────────────────────────────

type fakeKafkaWriter struct {
	failures atomic.Int32
	mu       sync.Mutex
	messages []kafka.Message
}

func (f *fakeKafkaWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if f.failures.Load() > 0 {
		f.failures.Add(-1)
		return errors.New("kafka: broker connection refused (outage)")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, msgs...)
	return nil
}

func TestOutbox_EventsSurviveABrokerOutage(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	_, v, _ := st.CreateActivity(ctx, tenantA, activityReq(p.PurposeID), "user-1")
	approve(t, st, ctx, v, "o1") // submitted + approved events enqueued while the broker is down

	pending := func() int {
		var n int
		_ = db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM event_outbox WHERE published_at IS NULL`).Scan(&n)
		return n
	}
	total := pending()
	if total < 3 { // purpose.published + submitted + approved
		t.Fatalf("expected the committed events in the outbox, found %d", total)
	}

	w := &fakeKafkaWriter{}
	w.failures.Store(1000) // broker down
	relay := events.NewOutboxRelay(db.App, w, "t", zap.NewNop())
	sent, err := relay.DrainPending(context.Background(), 50)
	if err != nil || sent != 0 {
		t.Fatalf("outage drain: sent=%d err=%v", sent, err)
	}
	if pending() != total {
		t.Fatalf("an outage lost events: %d pending, want %d", pending(), total)
	}
	var attempts int
	var lastErr *string
	_ = db.Admin.QueryRow(context.Background(), `SELECT max(attempts), max(last_error) FROM event_outbox`).Scan(&attempts, &lastErr)
	if attempts < 1 || lastErr == nil || !strings.Contains(*lastErr, "outage") {
		t.Fatalf("failure not recorded durably: attempts=%d last_error=%v", attempts, lastErr)
	}

	// Broker recovers; backoff elapses; every event is delivered exactly once.
	w.failures.Store(0)
	_, _ = db.Admin.Exec(context.Background(), `UPDATE event_outbox SET next_attempt_at = NOW() - interval '1 second'`)
	sent, err = relay.DrainPending(context.Background(), 50)
	if err != nil || sent != total {
		t.Fatalf("recovery drain: sent=%d want %d err=%v", sent, total, err)
	}
	if pending() != 0 {
		t.Fatalf("events still pending after recovery")
	}
	if len(w.messages) != total {
		t.Fatalf("delivered %d messages, want %d", len(w.messages), total)
	}
	if again, _ := relay.DrainPending(context.Background(), 50); again != 0 || len(w.messages) != total {
		t.Fatalf("an already-published event was re-sent")
	}
	for _, m := range w.messages {
		var e events.Event
		if err := json.Unmarshal(m.Value, &e); err != nil || e.EventID == "" || e.SourceService != "privacy-purpose-registry-svc" || e.EventType == "" {
			t.Fatalf("malformed envelope on the wire: %s (%v)", m.Value, err)
		}
	}
}

// A rolled-back transaction must leave NO event behind (events are enqueued in
// the same transaction as the state change, never "after commit").
func TestOutbox_RolledBackChange_LeavesNoEvent(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v1 := firstActive(t, st, tenantA, p)
	v2 := mustT[*domain.ProcessingActivityVersion](t, "v2")(st.CreateActivityVersion(ctx, act.ActivityID, versionReq(v1, p.PurposeID), "user-1"))
	before := len(outbox(t, db, ""))
	// v2 is a DRAFT: a retire-from-ACTIVE of it matches no row, so the tx rolls back.
	if _, err := st.TransitionActivity(ctx, act.ActivityID, v2.ActivityVersionID, domain.ActivityStatusActive, domain.ActivityStatusRetired, "user-3", "x"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
	if after := len(outbox(t, db, "")); after != before {
		t.Fatalf("a refused transition left %d event(s) behind", after-before)
	}
}

// ── item 10: audit evidence ──────────────────────────────────────────────────

type auditRow struct {
	Action, To, Actor, Corr string
	From, SupersededBy      *string
}

func audit(t *testing.T, db *testdb.DB, versionID string) []auditRow {
	t.Helper()
	rows, err := db.Admin.Query(context.Background(),
		`SELECT action, from_status, to_status, actor_principal_id, correlation_id, superseded_by_version_id::text
		   FROM activity_lifecycle_audit WHERE activity_version_id=$1 ORDER BY recorded_at, audit_id`, versionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Action, &r.From, &r.To, &r.Actor, &r.Corr, &r.SupersededBy); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestAudit_EveryLifecycleActionPersistsItsActor_IndependentOfTheEventBus(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v1 := firstActive(t, st, tenantA, p)
	v2 := mustT[*domain.ProcessingActivityVersion](t, "v2")(st.CreateActivityVersion(ctx, act.ActivityID, versionReq(v1, p.PurposeID), "user-1"))
	v2 = activate(t, st, ctx, approve(t, st, ctx, v2, "a2"), "a2-activate")

	// The broker has NEVER been reachable in this test — the audit rows exist anyway.
	got := audit(t, db, v1.ActivityVersionID)
	wantActions := []string{"VALIDATE", "SUBMIT", "APPROVE", "ACTIVATE", "SUPERSEDE"}
	if len(got) != len(wantActions) {
		t.Fatalf("v1 audit trail: want %v, got %+v", wantActions, got)
	}
	actors := []string{"user-1", "user-1", "user-2", "user-3", "user-3"}
	for i, r := range got {
		if r.Action != wantActions[i] || r.Actor != actors[i] || r.Corr == "" {
			t.Errorf("audit[%d] = %+v, want action %s actor %s and a correlation id", i, r, wantActions[i], actors[i])
		}
	}
	sup := got[4]
	if sup.To != "RETIRED" || sup.From == nil || *sup.From != "ACTIVE" || sup.SupersededBy == nil || *sup.SupersededBy != v2.ActivityVersionID {
		t.Errorf("supersede audit row does not record what superseded v1: %+v", sup)
	}

	// Append-only, even against a superuser.
	for _, stmt := range []string{
		`UPDATE activity_lifecycle_audit SET actor_principal_id='forged'`,
		`DELETE FROM activity_lifecycle_audit`,
	} {
		if _, err := db.Admin.Exec(context.Background(), stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q must be rejected as append-only, got %v", stmt, err)
		}
	}
	// And an actor is mandatory.
	_, err := db.Admin.Exec(context.Background(), `INSERT INTO activity_lifecycle_audit (activity_id, activity_version_id, action, to_status, actor_principal_id)
		SELECT activity_id, activity_version_id, 'X', 'ACTIVE', '' FROM processing_activity_versions LIMIT 1`)
	if err == nil {
		t.Errorf("an audit row without an actor was accepted")
	}
}

func TestAudit_ApproverRejecterActivatorColumns(t *testing.T) {
	st, _ := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	_, v, _ := st.CreateActivity(ctx, tenantA, activityReq(p.PurposeID), "user-1")
	v = approve(t, st, ctx, v, "m")
	if v.ApprovedByPrincipalID == nil || *v.ApprovedByPrincipalID != "user-2" || v.ApprovedAt == nil {
		t.Fatalf("approver evidence: %+v", v)
	}
	v = activate(t, st, ctx, v, "m-act")
	if v.ActivatedByPrincipalID == nil || *v.ActivatedByPrincipalID != "user-3" || v.ActivatedAt == nil {
		t.Fatalf("activator evidence: %+v", v)
	}
	_, r, _ := st.CreateActivity(ctx, tenantA, activityReq(p.PurposeID), "user-1")
	mustT[*domain.ProcessingActivityVersion](t, "validate")(st.SetValidationOutcome(ctx, r.ActivityID, r.ActivityVersionID, nil, "user-1", "r"))
	mustT[*domain.ProcessingActivityVersion](t, "submit")(st.TransitionActivity(ctx, r.ActivityID, r.ActivityVersionID, domain.ActivityStatusValidated, domain.ActivityStatusSubmitted, "user-1", "r"))
	rej := mustT[*domain.ProcessingActivityVersion](t, "reject")(st.RejectActivity(ctx, r.ActivityID, r.ActivityVersionID, "no", "user-9", "r"))
	if rej.RejectedByPrincipalID == nil || *rej.RejectedByPrincipalID != "user-9" || rej.RejectedAt == nil || rej.RejectionReason == nil {
		t.Fatalf("rejecter evidence: %+v", rej)
	}
}

// ── items 2/3: platform & tenant isolation under real RLS ────────────────────

func TestRLS_PlatformRecordsAreWritableOnlyFromTheExplicitPlatformContext(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	req := domain.CreatePurposeRequest{Statement: "s", CompatibilityClass: "PRIMARY"}
	plat := mkPurpose(t, st, platform)
	ten := mkPurpose(t, st, tenantA)

	t.Run("a tenant context cannot create a platform row", func(t *testing.T) {
		if _, _, err := st.CreatePurpose(tctx(tenantA), "", req, "u"); err == nil {
			t.Fatal("tenant A created a platform (tenant_id NULL) purpose")
		}
	})
	t.Run("a context with NO tenant can write nothing — omission is not platform scope", func(t *testing.T) {
		if _, _, err := st.CreatePurpose(context.Background(), "", req, "u"); err == nil {
			t.Fatal("a tenant-less context created a platform purpose")
		}
		if _, _, err := st.CreatePurpose(context.Background(), tenantA, req, "u"); err == nil {
			t.Fatal("a tenant-less context created a tenant purpose")
		}
	})
	t.Run("the platform context cannot create a tenant row", func(t *testing.T) {
		if _, _, err := st.CreatePurpose(tctx(platform), tenantA, req, "u"); err == nil {
			t.Fatal("the platform plane created a tenant's purpose")
		}
	})
	t.Run("cross-tenant create is refused", func(t *testing.T) {
		if _, _, err := st.CreatePurpose(tctx(tenantA), tenantB, req, "u"); err == nil {
			t.Fatal("tenant A created a purpose owned by tenant B")
		}
	})

	t.Run("updates: a tenant cannot touch platform rows; platform cannot touch tenant rows; tenants cannot touch each other", func(t *testing.T) {
		cases := []struct {
			name, tenant string
			platformCap  bool
			where        string
		}{
			{"tenant A -> platform row", tenantA, false, "tenant_id IS NULL"},
			{"tenant B -> tenant A row", tenantB, false, "tenant_id = '" + tenantA + "'"},
			{"platform -> tenant A row", platform, true, "tenant_id = '" + tenantA + "'"},
			{"no tenant -> any row", "", false, "true"},
		}
		for _, c := range cases {
			session(t, db, c.tenant, c.platformCap, func(tx pgx.Tx) {
				tag, err := tx.Exec(context.Background(), `UPDATE purposes SET created_by_principal_id='hacked' WHERE `+c.where)
				if err != nil && !isRLSViolation(err) {
					t.Errorf("%s: unexpected error %v", c.name, err)
				}
				if err == nil && tag.RowsAffected() != 0 {
					t.Errorf("%s: updated %d row(s) it does not own", c.name, tag.RowsAffected())
				}
			})
		}
		// Same for the version tables.
		session(t, db, tenantA, false, func(tx pgx.Tx) {
			tag, _ := tx.Exec(context.Background(), `UPDATE purpose_versions SET compatibility_class='X' WHERE purpose_id=$1`, plat.PurposeID)
			if tag.RowsAffected() != 0 {
				t.Errorf("tenant A modified a platform purpose version")
			}
		})
	})

	t.Run("the platform context CAN modify platform rows, and only those", func(t *testing.T) {
		session(t, db, platform, true, func(tx pgx.Tx) {
			tag, err := tx.Exec(context.Background(), `UPDATE purposes SET created_by_principal_id='platform-op' WHERE purpose_id=$1`, plat.PurposeID)
			if err != nil || tag.RowsAffected() != 1 {
				t.Errorf("platform context could not update its own record: %v rows=%d", err, tag.RowsAffected())
			}
		})
	})

	t.Run("reads: tenants see their own rows plus platform rows; never each other's", func(t *testing.T) {
		if _, err := st.ResolvePurposeAsOf(tctx(tenantB), ten.PurposeID, time.Now().Add(time.Minute), time.Now().Add(time.Minute)); !errors.Is(err, domain.ErrPurposeNotFound) {
			t.Errorf("tenant B resolved tenant A's purpose: %v", err)
		}
		for _, scope := range []string{tenantA, tenantB, platform} {
			if _, err := st.ResolvePurposeAsOf(tctx(scope), plat.PurposeID, time.Now().Add(time.Minute), time.Now().Add(time.Minute)); err != nil {
				t.Errorf("%s cannot read a platform purpose: %v", scope, err)
			}
		}
		if _, err := st.ResolvePurposeAsOf(tctx(platform), ten.PurposeID, time.Now().Add(time.Minute), time.Now().Add(time.Minute)); !errors.Is(err, domain.ErrPurposeNotFound) {
			t.Errorf("the platform plane resolved a tenant's purpose: %v", err)
		}
		listB := mustT[[]domain.PurposeVersion](t, "list B")(st.ListPurposes(tctx(tenantB)))
		for _, v := range listB {
			if v.TenantID != nil && *v.TenantID == tenantA {
				t.Errorf("tenant B listed tenant A's purpose")
			}
		}
	})
}

func TestRLS_RegistryHistoryCannotBeDeleted(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	p := mkPurpose(t, st, tenantA)
	for _, tbl := range []string{"purpose_versions", "purposes"} {
		// Through the restricted role: no DELETE policy exists, so nothing is deletable.
		session(t, db, tenantA, false, func(tx pgx.Tx) {
			tag, err := tx.Exec(context.Background(), `DELETE FROM `+tbl)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("restricted role deleted %d row(s) from %s", tag.RowsAffected(), tbl)
			}
		})
		// Even a superuser (who bypasses RLS) is stopped by the trigger.
		_, err := db.Admin.Exec(context.Background(), `DELETE FROM `+tbl)
		if err == nil || !strings.Contains(err.Error(), "retained governance history") {
			t.Errorf("DELETE FROM %s must be rejected by the history trigger, got %v", tbl, err)
		}
	}
	var n int
	_ = db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM purposes WHERE purpose_id=$1`, p.PurposeID).Scan(&n)
	if n != 1 {
		t.Fatalf("the purpose was deleted")
	}
}

func TestRLS_OutboxAndIdempotencyDoNotLeakAcrossScopes(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	mkPurpose(t, st, tenantA)
	mkPurpose(t, st, platform)

	count := func(tenant string, platformCap bool, q string) int {
		var n int
		session(t, db, tenant, platformCap, func(tx pgx.Tx) {
			if err := tx.QueryRow(context.Background(), q).Scan(&n); err != nil {
				t.Fatal(err)
			}
		})
		return n
	}
	const all = `SELECT count(*) FROM event_outbox`
	if n := count(tenantA, false, all); n != 1 {
		t.Errorf("tenant A should see exactly its own outbox event, sees %d", n)
	}
	if n := count(tenantB, false, all); n != 0 {
		t.Errorf("tenant B can read tenant A's pending events (%d)", n)
	}
	if n := count("", false, all); n != 0 {
		t.Errorf("a session with no tenant can read every tenant's pending events (%d) — the old policy's hole", n)
	}
	if n := count(platform, true, all); n != 1 {
		t.Errorf("the platform plane should see only platform events, sees %d", n)
	}
	// A tenant-less session cannot INSERT one either.
	session(t, db, "", false, func(tx pgx.Tx) {
		_, err := tx.Exec(context.Background(), `INSERT INTO event_outbox (outbox_event_id,event_type,tenant_id,payload) VALUES ('evt-x','x',$1,'{}')`, tenantA)
		if err == nil {
			t.Errorf("a tenant-less session injected an event into tenant A's outbox")
		}
	})

	// Idempotency records: cached bodies must not cross scopes.
	rec := domain.IdempotencyRecord{IdempotencyKey: "k1", TenantID: ptr(tenantA), PrincipalID: "p", Operation: "op", RequestHash: "h", ResponseStatus: 201, ResponseBody: []byte(`{"secret":1}`)}
	if err := st.SaveIdempotency(tctx(tenantA), rec); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetIdempotency(tctx(tenantA), tenantA, "p", "k1"); got == nil {
		t.Fatal("tenant A cannot read back its own idempotency record")
	}
	if got, _ := st.GetIdempotency(tctx(tenantB), tenantA, "p", "k1"); got != nil {
		t.Error("tenant B read tenant A's cached response")
	}
	if got, _ := st.GetIdempotency(tctx(platform), "", "p", "k1"); got != nil {
		t.Error("the platform plane read a tenant's cached response")
	}
	if err := st.SaveIdempotency(context.Background(), domain.IdempotencyRecord{IdempotencyKey: "k2", PrincipalID: "p", Operation: "op", RequestHash: "h", ResponseStatus: 200, ResponseBody: []byte(`{}`)}); err == nil {
		t.Error("a tenant-less context stored a platform idempotency record")
	}
}

func ptr[T any](v T) *T { return &v }

// ── item 6/12: effective & knowledge time ────────────────────────────────────

func TestAsOf_ValidTimeAndKnowledgeTime_AgainstRealSQL(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v1 := firstActive(t, st, tenantA, p)
	v2 := mustT[*domain.ProcessingActivityVersion](t, "v2")(st.CreateActivityVersion(ctx, act.ActivityID, versionReq(v1, p.PurposeID), "user-1"))
	v2 = activate(t, st, ctx, approve(t, st, ctx, v2, "b2"), "b2-activate")

	// Deterministic timeline (only lifecycle-evidence columns are rewritten, which the immutability trigger allows):
	//   v1 governed [now-3h, now-1h), approved now-4h;  v2 governs [now-1h, ∞), approved now-90m.
	_, err := db.Admin.Exec(context.Background(), `
		UPDATE processing_activity_versions SET
		  effective_from = NOW() - interval '3 hours', effective_to = NOW() - interval '1 hour', approved_at = NOW() - interval '4 hours'
		WHERE activity_version_id = $1`, v1.ActivityVersionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Admin.Exec(context.Background(), `
		UPDATE processing_activity_versions SET
		  effective_from = NOW() - interval '1 hour', effective_to = NULL, approved_at = NOW() - interval '90 minutes'
		WHERE activity_version_id = $1`, v2.ActivityVersionID)
	if err != nil {
		t.Fatal(err)
	}

	now := dbNow(t, db) // the DATABASE's clock: the timeline above was written with its NOW()
	at := func(asOf, known time.Duration) (*domain.ProcessingActivityVersion, error) {
		return st.ResolveActivityAsOf(ctx, act.ActivityID, now.Add(asOf), now.Add(known))
	}
	if got, err := at(-2*time.Hour, 0); err != nil || got.ActivityVersionID != v1.ActivityVersionID {
		t.Fatalf("2h ago v1 governed: %v %v", got, err)
	}
	if got, err := at(-30*time.Minute, 0); err != nil || got.ActivityVersionID != v2.ActivityVersionID {
		t.Fatalf("30m ago v2 governed: %v %v", got, err)
	}
	if got, err := at(0, 0); err != nil || got.ActivityVersionID != v2.ActivityVersionID {
		t.Fatalf("now v2 governs: %v %v", got, err)
	}
	// A boundary instant belongs to the NEW version (windows are [from, to)).
	if got, err := at(-time.Hour+time.Second, 0); err != nil || got.ActivityVersionID != v2.ActivityVersionID {
		t.Fatalf("just after the boundary v2 governs: %v %v", got, err)
	}
	if _, err := at(-5*time.Hour, 0); !errors.Is(err, domain.ErrActivityNotFound) {
		t.Fatalf("before any version governed must be not-found, got %v", err)
	}
	// Knowledge time: 3h ago we had approved v1 but not v2.
	if got, err := at(-time.Hour+time.Minute, -3*time.Hour); !errors.Is(err, domain.ErrActivityNotFound) {
		t.Fatalf("v2 was unknown 3h ago and v1 no longer governed; want not-found, got %v %v", got, err)
	}
	if got, err := at(-2*time.Hour, -3*time.Hour); err != nil || got.ActivityVersionID != v1.ActivityVersionID {
		t.Fatalf("as known 3h ago, v1 governed 2h ago: %v %v", got, err)
	}
	if _, err := at(-2*time.Hour, -5*time.Hour); !errors.Is(err, domain.ErrActivityNotFound) {
		t.Fatalf("v1 was not even approved 5h ago: %v", err)
	}
}

func TestRetire_ClosesTheValidTimeWindow(t *testing.T) {
	st, _ := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v := firstActive(t, st, tenantA, p)
	retired := mustT[*domain.ProcessingActivityVersion](t, "retire")(st.TransitionActivity(ctx, act.ActivityID, v.ActivityVersionID, domain.ActivityStatusActive, domain.ActivityStatusRetired, "user-3", "ret"))
	if retired.EffectiveTo == nil || !retired.EffectiveTo.After(*v.EffectiveFrom) {
		t.Fatalf("retire must close a non-empty window: from=%v to=%v", v.EffectiveFrom, retired.EffectiveTo)
	}
	// Every probe is built from the database-assigned window, not the host clock.
	known := v.EffectiveFrom.Add(time.Hour)
	inside := v.EffectiveFrom.Add(retired.EffectiveTo.Sub(*v.EffectiveFrom) / 2)
	if got, err := st.ResolveActivityAsOf(ctx, act.ActivityID, inside, known); err != nil || got.ActivityVersionID != v.ActivityVersionID {
		t.Fatalf("it governed inside its window: %v %v", got, err)
	}
	if _, err := st.ResolveActivityAsOf(ctx, act.ActivityID, *retired.EffectiveTo, known); !errors.Is(err, domain.ErrActivityNotFound) {
		t.Fatalf("the window is half-open: at effective_to it no longer governs, got %v", err)
	}
	if _, err := st.ResolveActivityAsOf(ctx, act.ActivityID, retired.EffectiveTo.Add(time.Minute), known); !errors.Is(err, domain.ErrActivityNotFound) {
		t.Fatalf("a retired activity must not govern afterwards: %v", err)
	}
}

func TestPurposePublish_CannotBackdateEffectiveFrom(t *testing.T) {
	st, _ := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	past, future := time.Now().Add(-48*time.Hour).UTC(), time.Now().Add(48*time.Hour).UTC()
	for name, c := range map[string]struct {
		eff    time.Time
		isPast bool
	}{"backdated draft": {past, true}, "future-dated draft": {future, false}} {
		p, v, err := st.CreatePurpose(ctx, tenantA, domain.CreatePurposeRequest{Statement: "s", CompatibilityClass: "PRIMARY", EffectiveFrom: &c.eff}, "u1")
		if err != nil {
			t.Fatal(err)
		}
		pub, err := st.PublishPurposeVersion(ctx, p.PurposeID, v.PurposeVersionID, "u2", "c")
		if err != nil {
			t.Fatal(err)
		}
		if c.isPast && !pub.EffectiveFrom.After(past.Add(time.Hour)) {
			t.Errorf("%s: effective_from was left in the past (%v) — publishing must not rewrite history", name, pub.EffectiveFrom)
		}
		if !c.isPast && !pub.EffectiveFrom.Equal(future.Truncate(time.Microsecond)) {
			t.Errorf("%s: a deliberate future date was altered: %v", name, pub.EffectiveFrom)
		}
	}
}

func TestPurposeVersion_ImmutableOnceActivityContentLeavesDraft(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	p := mkPurpose(t, st, tenantA)
	_, v := firstActive(t, st, tenantA, p)
	for _, col := range []string{"privacy_owner='x'", "processor_refs='[\"x\"]'", "owner='x'", "sources='[\"x\"]'"} {
		if _, err := db.Admin.Exec(context.Background(), `UPDATE processing_activity_versions SET `+col+` WHERE activity_version_id=$1`, v.ActivityVersionID); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("content column update %q must be rejected after the version left DRAFT, got %v", col, err)
		}
	}
	if _, err := db.Admin.Exec(context.Background(), `UPDATE purpose_versions SET necessity_rationale='changed' WHERE purpose_id=$1`, p.PurposeID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("a PUBLISHED purpose's rationale must be immutable, got %v", err)
	}
}

// dbNow is the DATABASE's clock. Timestamps that the store assigns (effective_from,
// effective_to, approved_at, …) come from the database's NOW(), which can differ
// from the test host's clock by a fraction of a second (e.g. a container's VM).
// Any test that compares instants must therefore build them from the database's
// clock or from the values the store returned — never from time.Now().
func dbNow(t *testing.T, db *testdb.DB) time.Time {
	t.Helper()
	var n time.Time
	if err := db.Admin.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n.UTC()
}
