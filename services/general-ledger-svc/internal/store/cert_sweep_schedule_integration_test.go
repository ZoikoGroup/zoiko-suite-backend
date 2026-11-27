package store_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/certreliance"
	"zoiko.io/general-ledger-svc/internal/certwatch"
)

// The scheduled certification sweep against real Postgres: the cross-tenant scan under row-level security, the single-replica lock,
// and a whole scheduled run through the real repository and sweeper.

func (e *kenv) scheduler(principal string) *certwatch.Scheduler {
	return &certwatch.Scheduler{
		Sweeper: &certwatch.Sweeper{Store: e.watch, Status: e.status}, Scopes: e.watch, Authz: fakeAuthz{e}, Lock: e.watch,
		Principal: principal, Interval: 0,
	}
}

func TestCertSweepSchedule_ScanSpansTenantsButRowLevelSecurityStillIsolates(t *testing.T) {
	e := newKEnv(t)
	e.postManual(t, "sc-1", certX)

	tenant2, entity2 := uuid.NewString(), uuid.NewString()
	if err := e.watch.Watch(e.ctx(), tenant2, entity2, ref(t, certZ)); err != nil {
		t.Fatal(err)
	}
	// A second certification of the same entity does not duplicate the scope.
	e.postManual(t, "sc-2", certY)

	scopes, err := e.watch.ListScopes(e.ctx())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, s := range scopes {
		got[s.TenantID+"|"+s.LegalEntityID]++
	}
	if len(scopes) != 2 || got[e.tenant+"|"+e.entity] != 1 || got[tenant2+"|"+entity2] != 1 {
		t.Fatalf("scopes: %+v", scopes)
	}

	// The capability is not ambient: an ordinary tenant-scoped transaction still sees only its own tenant's watch rows, and a
	// transaction with no tenant sees none.
	count := func(setup ...string) int {
		tx, err := e.app.Begin(e.ctx())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(e.ctx()) //nolint:errcheck
		for _, s := range setup {
			if _, err := tx.Exec(e.ctx(), s); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := tx.QueryRow(e.ctx(), `SELECT count(*) FROM certification_watch`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT set_config('app.tenant_id', '` + e.tenant + `', true)`); n != 2 {
		t.Fatalf("a tenant-scoped transaction saw %d rows, want its own 2 (certX and certY)", n)
	}
	if n := count(); n != 0 {
		t.Fatalf("a transaction with no tenant and no capability saw %d rows", n)
	}
	if n := count(`SELECT set_config('app.certification_sweeper', 'true', true)`); n != 3 {
		t.Fatalf("the capability saw %d rows, want all 3", n)
	}
}

// The capability can read and nothing else: it is a SELECT policy.
func TestCertSweepSchedule_TheScanCapabilityCannotWrite(t *testing.T) {
	e := newKEnv(t)
	e.postManual(t, "w-1", certX)
	try := func(stmt string, args ...any) (int64, error) {
		tx, err := e.app.Begin(e.ctx())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(e.ctx()) //nolint:errcheck
		if _, err := tx.Exec(e.ctx(), `SELECT set_config('app.certification_sweeper', 'true', true)`); err != nil {
			t.Fatal(err)
		}
		tag, err := tx.Exec(e.ctx(), stmt, args...)
		return tag.RowsAffected(), err
	}
	// Updates and deletes find no row (the capability makes rows visible to SELECT only), so nothing changes.
	if n, err := try(`UPDATE certification_watch SET last_status = 'ACTIVE'`); err != nil || n != 0 {
		t.Fatalf("update: %d rows, %v", n, err)
	}
	if n, err := try(`DELETE FROM certification_watch`); err != nil || n != 0 {
		t.Fatalf("delete: %d rows, %v", n, err)
	}
	// Inserting a row for any tenant is refused by row-level security.
	if _, err := try(`INSERT INTO certification_watch (tenant_id, legal_entity_id, certification_service, run_id) VALUES ($1, $2, 'data-quality-svc', 'dqn_w')`,
		uuid.NewString(), uuid.NewString()); err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("insert with only the capability: %v", err)
	}
	if e.count(t, `SELECT count(*) FROM certification_watch WHERE last_status IS NOT NULL`) != 0 || e.count(t, `SELECT count(*) FROM certification_watch`) != 1 {
		t.Fatal("the capability changed the watch list")
	}
	// The flags table has the same capability (migration 000038, for the open-flag gauges): readable, never writable. Raise a real flag
	// first, so none of this passes only because the table is empty.
	e.status.set(certX, revokedX)
	e.sweep(t, e.entity)
	if e.count(t, `SELECT count(*) FROM certification_flags WHERE status = 'OPEN'`) != 1 {
		t.Fatal("setup: no flag was raised")
	}
	flagsTry := func(stmt string, args ...any) (int64, error) {
		tx, err := e.app.Begin(e.ctx())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(e.ctx()) //nolint:errcheck
		if _, err := tx.Exec(e.ctx(), `SELECT set_config('app.certification_sweeper', 'true', true)`); err != nil {
			t.Fatal(err)
		}
		tag, err := tx.Exec(e.ctx(), stmt, args...)
		return tag.RowsAffected(), err
	}
	tx, err := e.app.Begin(e.ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx()) //nolint:errcheck
	_, _ = tx.Exec(e.ctx(), `SELECT set_config('app.certification_sweeper', 'true', true)`)
	var seen int
	if err := tx.QueryRow(e.ctx(), `SELECT count(*) FROM certification_flags`).Scan(&seen); err != nil || seen != 1 {
		t.Fatalf("the capability must read the flag it is there to count: %d %v", seen, err)
	}
	if n, err := flagsTry(`UPDATE certification_flags SET status = 'ACKNOWLEDGED', acknowledged_by = 'x', acknowledged_at = now(), acknowledgement = 'x'`); err != nil || n != 0 {
		t.Fatalf("update through the capability: %d rows, %v", n, err)
	}
	if n, err := flagsTry(`DELETE FROM certification_flags`); err != nil || n != 0 {
		t.Fatalf("delete through the capability: %d rows, %v", n, err)
	}
	if _, err := flagsTry(`INSERT INTO certification_flags (flag_id, tenant_id, legal_entity_id, certification_service, run_id, subject_type, subject_id, journal_id, revocation_id, reason)
		SELECT gen_random_uuid(), tenant_id, legal_entity_id, certification_service, run_id, 'journal', 'forged', journal_id, 'r', 'r' FROM certification_flags LIMIT 1`); err == nil {
		t.Fatal("the capability inserted a flag")
	}
	if e.count(t, `SELECT count(*) FROM certification_flags WHERE status = 'OPEN'`) != 1 {
		t.Fatal("the capability changed the flags")
	}
	// Without the capability and without a tenant, no flag is visible at all.
	tx2, err := e.app.Begin(e.ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(e.ctx()) //nolint:errcheck
	var bare int
	if err := tx2.QueryRow(e.ctx(), `SELECT count(*) FROM certification_flags`).Scan(&bare); err != nil || bare != 0 {
		t.Fatalf("flags visible with neither a tenant nor the capability: %d %v", bare, err)
	}
}

func TestCertFlagMetrics_OpenFlagStatsCountOnlyWhatAwaitsADecision(t *testing.T) {
	e := newKEnv(t)
	st, err := e.watch.OpenFlagStats(e.ctx())
	if err != nil || st.Open != 0 || st.Oldest != 0 {
		t.Fatalf("nothing flagged: %+v %v", st, err)
	}
	e.postManual(t, "fs-1", certX)
	e.postManual(t, "fs-2", certZ)
	e.postManual(t, "fs-3", certY) // stays in force
	e.status.set(certX, revokedX)
	e.status.set(certZ, revokedZ)
	e.sweep(t, e.entity)
	time.Sleep(1100 * time.Millisecond) // so the oldest has a measurable age

	st, err = e.watch.OpenFlagStats(e.ctx())
	if err != nil || st.Open != 2 || st.Oldest < time.Second || st.Oldest > time.Minute {
		t.Fatalf("two open flags: %+v %v", st, err)
	}
	// A flag a person has decided on is no longer waiting: it leaves both gauges.
	flags := e.flags(t, e.entity, "OPEN")
	if st, _ := e.call("POST", "/v1/certification-flags/"+flags[0]["flag_id"].(string)+"/acknowledge", e.tenant, "controller", map[string]any{"acknowledgement": "reviewed"}); st != 200 {
		t.Fatalf("acknowledge: %d", st)
	}
	st, err = e.watch.OpenFlagStats(e.ctx())
	if err != nil || st.Open != 1 {
		t.Fatalf("one open flag: %+v %v", st, err)
	}
	if st2, _ := e.call("POST", "/v1/certification-flags/"+flags[1]["flag_id"].(string)+"/acknowledge", e.tenant, "controller", map[string]any{"acknowledgement": "reviewed"}); st2 != 200 {
		t.Fatal("acknowledge")
	}
	if st, err = e.watch.OpenFlagStats(e.ctx()); err != nil || st.Open != 0 || st.Oldest != 0 {
		t.Fatalf("all decided: %+v %v", st, err)
	}
}

func TestCertSweepSchedule_OnlyOneReplicaHoldsTheLock(t *testing.T) {
	e := newKEnv(t)
	release, ok, err := e.watch.TryLock(e.ctx())
	if err != nil || !ok {
		t.Fatalf("first lock: %v %v", ok, err)
	}
	if _, ok2, err := e.watch.TryLock(e.ctx()); err != nil || ok2 {
		t.Fatalf("a second replica took the lock: %v %v", ok2, err)
	}
	release()
	release2, ok3, err := e.watch.TryLock(e.ctx())
	if err != nil || !ok3 {
		t.Fatalf("the lock was not released: %v %v", ok3, err)
	}
	release2()

	// Under real contention exactly one of many wins.
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	start := make(chan struct{})
	releases := make(chan func(), 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rel, got, err := e.watch.TryLock(e.ctx())
			if err != nil {
				t.Error(err)
				return
			}
			if got {
				mu.Lock()
				winners++
				mu.Unlock()
				releases <- rel
			}
		}()
	}
	close(start)
	wg.Wait()
	close(releases)
	if winners != 1 {
		t.Fatalf("%d replicas took the lock at once", winners)
	}
	for rel := range releases {
		rel()
	}
}

func TestCertSweepSchedule_AScheduledRunFlagsWithoutAnyoneAsking(t *testing.T) {
	e := newKEnv(t)
	jX := e.postManual(t, "r-1", certX)
	e.postManual(t, "r-2", certY)
	e.status.set(certX, revokedX)

	sum, err := e.scheduler("svc-sweeper").RunOnce(e.ctx())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Scopes != 1 || sum.Swept != 1 || sum.Skipped != 0 || sum.Checked != 2 || sum.Withdrawn != 1 || sum.NewFlags != 1 {
		t.Fatalf("%+v", sum)
	}
	flags := e.flags(t, e.entity, "OPEN")
	if len(flags) != 1 || flags[0]["journal_id"] != jX || flags[0]["certification"] != certX {
		t.Fatalf("flags: %v", flags)
	}
	// The next tick finds the same facts: nothing new, and the lock is free again.
	again, err := e.scheduler("svc-sweeper").RunOnce(e.ctx())
	if err != nil || again.NewFlags != 0 || again.Withdrawn != 1 || again.Locked {
		t.Fatalf("second run: %+v %v", again, err)
	}
	release, ok, _ := e.watch.TryLock(e.ctx())
	if !ok {
		t.Fatal("the lock was left held after a run")
	}
	release() // this check must not itself hold the lock (or its connection) past here

	// A principal that is not authorized to sweep the entity is skipped, and the flags are untouched.
	e.postManual(t, "r-3", certZ)
	e.status.set(certZ, revokedZ)
	e.denied.Store("svc-no-grant|GL_CERTIFICATION_WATCH_SWEEP", true)
	denied, err := e.scheduler("svc-no-grant").RunOnce(e.ctx())
	if err != nil || denied.Swept != 0 || denied.Skipped != 1 || denied.NewFlags != 0 {
		t.Fatalf("denied run: %+v %v", denied, err)
	}
	if len(e.flags(t, e.entity, "OPEN")) != 1 {
		t.Fatal("a principal that is not authorized raised flags")
	}
	// Authorized again, it finds the second revocation.
	if sum, err := e.scheduler("svc-sweeper").RunOnce(e.ctx()); err != nil || sum.NewFlags != 1 {
		t.Fatalf("%+v %v", sum, err)
	}
	if len(e.flags(t, e.entity, "OPEN")) != 2 {
		t.Fatal("the later revocation was not flagged")
	}
}

func TestCertSweepSchedule_EveryTenantsScopeIsSweptUnderItsOwnTenant(t *testing.T) {
	e := newKEnv(t)
	e.postManual(t, "t-1", certX)
	tenant2, entity2 := uuid.NewString(), uuid.NewString()
	if err := e.watch.Watch(e.ctx(), tenant2, entity2, ref(t, certY)); err != nil {
		t.Fatal(err)
	}
	e.status.set(certX, revokedX)

	sum, err := e.scheduler("svc-sweeper").RunOnce(e.ctx())
	if err != nil || sum.Scopes != 2 || sum.Swept != 2 || sum.Checked != 2 || sum.NewFlags != 1 {
		t.Fatalf("%+v %v", sum, err)
	}
	// The second tenant's certification was checked and recorded under ITS tenant (still in force: no flag).
	var st string
	if err := e.admin.QueryRow(e.ctx(), `SELECT COALESCE(last_status, '') FROM certification_watch WHERE tenant_id = $1`, tenant2).Scan(&st); err != nil || st != certreliance.StateActive {
		t.Fatalf("second tenant's watch row: %q %v", st, err)
	}
}

func ref(t *testing.T, s string) certreliance.Ref {
	t.Helper()
	r, err := certreliance.ParseRefs([]string{s})
	if err != nil || len(r) != 1 {
		t.Fatalf("%q: %v", s, err)
	}
	return r[0]
}
