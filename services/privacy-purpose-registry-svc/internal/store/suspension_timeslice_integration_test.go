package store_test

// Real-PostgreSQL regression tests: suspension is time-sliced in as_of
// resolution (§9.1 as_of, §20.3 bitemporality, PRV-002 "suspended for effective
// time"). The status a read reports is the status AT the requested instants,
// derived from the append-only SUSPEND/RESUME rows of activity_lifecycle_audit —
// not today's stored status.

import (
	"context"
	"testing"
	"time"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/store"
	"zoiko.io/privacy-purpose-registry-svc/internal/testdb"
)

func statusAtInstants(t *testing.T, st *store.PgStore, scope, activityID string, asOf, known time.Time) domain.ActivityVersionStatus {
	t.Helper()
	v, err := st.ResolveActivityAsOf(tctx(scope), activityID, asOf, known)
	if err != nil {
		t.Fatalf("as_of %s known %s: want a governing version, got %v", asOf.Format(time.RFC3339Nano), known.Format(time.RFC3339Nano), err)
	}
	return v.VersionStatus
}

func lifecycleInstants(t *testing.T, db *testdb.DB, versionID, action string) []time.Time {
	t.Helper()
	rows, err := db.Admin.Query(context.Background(),
		`SELECT recorded_at FROM activity_lifecycle_audit WHERE activity_version_id=$1 AND action=$2 ORDER BY recorded_at`, versionID, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		out = append(out, at.UTC())
	}
	return out
}

// Real transitions: the instants are the ones the database stamped.
func TestAsOf_SuspensionIsTimeSliced_RealTransitions(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v := firstActive(t, st, tenantA, p)
	// Make the activation unambiguously earlier than everything probed below.
	if _, err := db.Admin.Exec(context.Background(), `
		UPDATE processing_activity_versions SET effective_from = NOW() - interval '2 hours', approved_at = NOW() - interval '3 hours'
		WHERE activity_version_id = $1`, v.ActivityVersionID); err != nil {
		t.Fatal(err)
	}
	tr := func(from, to domain.ActivityVersionStatus, corr string) {
		mustT[*domain.ProcessingActivityVersion](t, corr)(st.TransitionActivity(ctx, act.ActivityID, v.ActivityVersionID, from, to, "user-3", corr))
		time.Sleep(20 * time.Millisecond)
	}
	tr(domain.ActivityStatusActive, domain.ActivityStatusSuspended, "s1")
	tr(domain.ActivityStatusSuspended, domain.ActivityStatusActive, "r1")
	tr(domain.ActivityStatusActive, domain.ActivityStatusSuspended, "s2") // currently SUSPENDED

	sus := lifecycleInstants(t, db, v.ActivityVersionID, "SUSPEND")
	res := lifecycleInstants(t, db, v.ActivityVersionID, "RESUME")
	if len(sus) != 2 || len(res) != 1 {
		t.Fatalf("expected 2 SUSPEND and 1 RESUME rows, got %d/%d", len(sus), len(res))
	}
	s1, r1, s2 := sus[0], res[0], sus[1]
	known := dbNow(t, db).Add(time.Hour) // everything recorded is known
	at := func(asOf time.Time) domain.ActivityVersionStatus {
		return statusAtInstants(t, st, tenantA, act.ActivityID, asOf, known)
	}
	ms := time.Millisecond

	// (1) before the first suspension it was ACTIVE — today's SUSPENDED must not leak backwards.
	if got := at(s1.Add(-ms)); got != domain.ActivityStatusActive {
		t.Fatalf("before the suspension: want ACTIVE, got %s", got)
	}
	if got := at(s1.Add(-time.Hour)); got != domain.ActivityStatusActive {
		t.Fatalf("an hour before the suspension: want ACTIVE, got %s", got)
	}
	// (2) inside a suspension (the suspend instant itself belongs to it: half-open [suspend, resume)).
	if got := at(s1); got != domain.ActivityStatusSuspended {
		t.Fatalf("at the suspend instant: want SUSPENDED, got %s", got)
	}
	if got := at(s1.Add(r1.Sub(s1) / 2)); got != domain.ActivityStatusSuspended {
		t.Fatalf("inside the first suspension: want SUSPENDED, got %s", got)
	}
	// (3) after the resume, until the next suspension.
	if got := at(r1); got != domain.ActivityStatusActive {
		t.Fatalf("at the resume instant: want ACTIVE, got %s", got)
	}
	if got := at(r1.Add(s2.Sub(r1) / 2)); got != domain.ActivityStatusActive {
		t.Fatalf("between resume and second suspension: want ACTIVE, got %s", got)
	}
	if got := at(s2); got != domain.ActivityStatusSuspended {
		t.Fatalf("second suspension: want SUSPENDED, got %s", got)
	}
	if got := at(s2.Add(ms)); got != domain.ActivityStatusSuspended {
		t.Fatalf("after the last suspension: want SUSPENDED, got %s", got)
	}

	// (5) knowledge time: a suspension recorded after known_as_of is not yet known.
	far := s2.Add(ms)
	if got := statusAtInstants(t, st, tenantA, act.ActivityID, far, s1.Add(-ms)); got != domain.ActivityStatusActive {
		t.Fatalf("as known before any suspension was recorded: want ACTIVE, got %s", got)
	}
	if got := statusAtInstants(t, st, tenantA, act.ActivityID, far, s1.Add(r1.Sub(s1)/2)); got != domain.ActivityStatusSuspended {
		t.Fatalf("as known during the first suspension: want SUSPENDED, got %s", got)
	}
	if got := statusAtInstants(t, st, tenantA, act.ActivityID, far, r1.Add(s2.Sub(r1)/2)); got != domain.ActivityStatusActive {
		t.Fatalf("as known after the resume but before the second suspension: want ACTIVE, got %s", got)
	}

	// (6) "now" (both instants at/after the database clock) is the stored status.
	future := dbNow(t, db).Add(time.Minute)
	if got := statusAtInstants(t, st, tenantA, act.ActivityID, future, future); got != domain.ActivityStatusSuspended {
		t.Fatalf("current read: want the stored SUSPENDED, got %s", got)
	}
}

// Several versions with different effective and suspension windows. Audit rows
// are inserted with explicit instants (the table is append-only; INSERT is allowed).
func TestAsOf_SuspensionIsTimeSliced_MultipleVersionsDeterministic(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	p := mkPurpose(t, st, tenantA)
	act, v1 := firstActive(t, st, tenantA, p)
	v2 := mustT[*domain.ProcessingActivityVersion](t, "v2")(st.CreateActivityVersion(ctx, act.ActivityID, versionReq(v1, p.PurposeID), "user-1"))
	v2 = activate(t, st, ctx, approve(t, st, ctx, v2, "m2"), "m2-activate") // retires v1

	admin := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(context.Background(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	//   v1 governed [-6h, -2h), approved -7h;   v2 governs [-2h, ∞), approved -3h.
	admin(`UPDATE processing_activity_versions SET effective_from = NOW() - interval '6 hours', effective_to = NOW() - interval '2 hours', approved_at = NOW() - interval '7 hours' WHERE activity_version_id = $1`, v1.ActivityVersionID)
	admin(`UPDATE processing_activity_versions SET effective_from = NOW() - interval '2 hours', effective_to = NULL, approved_at = NOW() - interval '3 hours' WHERE activity_version_id = $1`, v2.ActivityVersionID)
	event := func(versionID, action, offset string) {
		t.Helper()
		from, to := "ACTIVE", "SUSPENDED"
		if action == "RESUME" {
			from, to = "SUSPENDED", "ACTIVE"
		}
		admin(`INSERT INTO activity_lifecycle_audit (tenant_id, activity_id, activity_version_id, action, from_status, to_status, actor_principal_id, correlation_id, recorded_at)
		       VALUES ($1, $2, $3, $4, $5, $6, 'user-3', 'seed', NOW() - $7::interval)`,
			tenantA, act.ActivityID, versionID, action, from, to, offset)
	}
	event(v1.ActivityVersionID, "SUSPEND", "5 hours")  // v1 suspended [-5h, -4h)
	event(v1.ActivityVersionID, "RESUME", "4 hours")
	event(v2.ActivityVersionID, "SUSPEND", "90 minutes") // v2 suspended [-90m, -30m)
	event(v2.ActivityVersionID, "RESUME", "30 minutes")

	now := dbNow(t, db)
	probe := func(d time.Duration) (string, domain.ActivityVersionStatus) {
		v, err := st.ResolveActivityAsOf(ctx, act.ActivityID, now.Add(d), now.Add(time.Hour))
		if err != nil {
			t.Fatalf("as_of now%+v: %v", d, err)
		}
		return v.ActivityVersionID, v.VersionStatus
	}
	for _, c := range []struct {
		d      time.Duration
		id     string
		status domain.ActivityVersionStatus
		label  string
	}{
		{-5*time.Hour - 30*time.Minute, v1.ActivityVersionID, domain.ActivityStatusActive, "v1 before its suspension (v1 is RETIRED today)"},
		{-4*time.Hour - 30*time.Minute, v1.ActivityVersionID, domain.ActivityStatusSuspended, "v1 during its suspension"},
		{-3 * time.Hour, v1.ActivityVersionID, domain.ActivityStatusActive, "v1 after its resume, before it was superseded"},
		{-1*time.Hour - 45*time.Minute, v2.ActivityVersionID, domain.ActivityStatusActive, "v2 before its suspension"},
		{-time.Hour, v2.ActivityVersionID, domain.ActivityStatusSuspended, "v2 during its suspension"},
		{-10 * time.Minute, v2.ActivityVersionID, domain.ActivityStatusActive, "v2 after its resume"},
	} {
		id, status := probe(c.d)
		if id != c.id || status != c.status {
			t.Errorf("%s: got version %s status %s, want %s %s", c.label, id, status, c.id, c.status)
		}
	}
	// Supersession is untouched: at the boundary the new version governs.
	if id, _ := probe(-2*time.Hour + time.Second); id != v2.ActivityVersionID {
		t.Fatalf("just after the supersession boundary v2 must govern, got %s", id)
	}
	// Knowledge time: v2's suspension was recorded 90m ago; as known 2h ago it was not yet suspended.
	v, err := st.ResolveActivityAsOf(ctx, act.ActivityID, now.Add(-30*time.Minute-time.Second), now.Add(-2*time.Hour+time.Minute))
	if err != nil || v.ActivityVersionID != v2.ActivityVersionID || v.VersionStatus != domain.ActivityStatusActive {
		t.Fatalf("as known before the suspension was recorded v2 was ACTIVE: %+v %v", v, err)
	}
	// Current read: v2 is ACTIVE now and the version/status are unchanged.
	if id, status := probe(time.Minute); id != v2.ActivityVersionID || status != domain.ActivityStatusActive {
		t.Fatalf("current read changed: %s %s", id, status)
	}
}

// A version suspended before the audit table existed has no SUSPEND row; it
// cannot be time-sliced and stays SUSPENDED rather than becoming ACTIVE.
func TestAsOf_LegacySuspendedWithoutLifecycleRows_StaysSuspended(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	p := mkPurpose(t, st, tenantA)
	act, v := firstActive(t, st, tenantA, p)
	if _, err := db.Admin.Exec(context.Background(), `
		UPDATE processing_activity_versions SET version_status = 'SUSPENDED', effective_from = NOW() - interval '2 hours', approved_at = NOW() - interval '3 hours'
		WHERE activity_version_id = $1`, v.ActivityVersionID); err != nil {
		t.Fatal(err)
	}
	now := dbNow(t, db)
	if got := statusAtInstants(t, st, tenantA, act.ActivityID, now.Add(-time.Hour), now.Add(time.Hour)); got != domain.ActivityStatusSuspended {
		t.Fatalf("a legacy suspension cannot be time-sliced and must stay SUSPENDED, got %s", got)
	}
}
