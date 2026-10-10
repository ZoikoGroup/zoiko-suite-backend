package store_test

// Real-Postgres suite. Same gating convention as the sibling services: skipped
// unless TEST_DATABASE_URL points at a Postgres this machine can use (and
// FAILED instead of skipped under CI / REQUIRE_DB_TESTS, since a skipped
// integration suite reports ok having verified nothing).
//
// Run it as a NOSUPERUSER NOBYPASSRLS role that OWNS the schema (FORCE RLS
// applies to the owner). A superuser DSN skips the RLS test. See
// RELEASE_CERTIFICATE.md for the exact run.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/service"
	"zoiko.io/accounting-period-svc/internal/store"
)

func requireTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" || os.Getenv("REQUIRE_DB_TESTS") != "" {
			t.Fatal("TEST_DATABASE_URL is not set, but CI or REQUIRE_DB_TESTS demands these run. " +
				"A skipped integration suite reports ok having verified nothing.")
		}
		t.Skip("TEST_DATABASE_URL not set - skipping real-Postgres integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `
		DROP TABLE IF EXISTS accounting_period_outbox, accounting_period_idempotency, period_state_history, accounting_periods CASCADE;
		DROP FUNCTION IF EXISTS accounting_period_guard_update() CASCADE;
		DROP FUNCTION IF EXISTS accounting_period_forbid_delete() CASCADE;
		DROP FUNCTION IF EXISTS accounting_period_forbid_mutation() CASCADE;`)
	require.NoError(t, err)

	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")
	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	require.NoError(t, err)
	require.Len(t, migrations, 4)
	sort.Strings(migrations)
	for _, p := range migrations {
		sqlText, err := os.ReadFile(p)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(sqlText))
		require.NoError(t, err, "applying %s", filepath.Base(p))
	}
	return pool
}

// ── stubs ────────────────────────────────────────────────────────────────────

type okProv struct {
	mu    sync.Mutex
	calls int
}

func (p *okProv) Verify(context.Context, service.ProvenanceRequest) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return nil
}

type cal struct{}

func (cal) Resolve(context.Context, string, string, string, string) (*service.CalendarRef, error) {
	return &service.CalendarRef{CalendarID: "cal-1", VersionID: "v1"}, nil
}

func (cal) PeriodsPreview(_ context.Context, _, vid string, year int) (*service.CalendarPreview, error) {
	pv := &service.CalendarPreview{CalendarID: "cal-1", VersionID: vid, VersionNo: 1, LegalEntityID: "entity-a", FiscalYear: year}
	for m := 1; m <= 12; m++ {
		s := time.Date(year, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
		pv.Periods = append(pv.Periods, service.CalendarPeriod{PeriodKey: fmt.Sprintf("FY%d-P%02d", year, m), PeriodNo: m,
			StartDate: s.Format("2006-01-02"), EndDate: s.AddDate(0, 1, -1).Format("2006-01-02"), Kind: "NORMAL"})
	}
	return pv, nil
}

func newSvc(pool *pgxpool.Pool) (*service.Service, *okProv) {
	pv := &okProv{}
	svc := service.New(store.New(pool), cal{}, pv)
	return svc, pv
}

func meta(actor, tenant, key, reason string) service.Meta {
	return service.Meta{Actor: actor, TenantID: tenant, CorrelationID: "corr-1", IdempotencyKey: key, RequestHash: "h-" + key, Reason: reason}
}

func materialize(t *testing.T, svc *service.Service, tenant, key string) *service.MaterializeResult {
	t.Helper()
	res, _, err := svc.MaterializePeriods(context.Background(), service.MaterializeInput{
		Meta: meta("steward", tenant, key, "year start"), LegalEntityID: "entity-a", CalendarID: "cal-1", FiscalYear: 2026, CalendarVersionID: "v1"})
	require.NoError(t, err)
	return res
}

func command(svc *service.Service, tenant, actor, key, id string, cmd domain.Command, version int64, wf string) (*domain.Period, error) {
	in := service.CommandInput{Meta: meta(actor, tenant, key, "close"), PeriodID: id, Command: cmd, ExpectedVersion: version,
		Acc14WorkflowRef: wf, ControlSnapshotRef: "snap-" + wf}
	if cmd == domain.CmdAuthorizeReopen {
		exp := time.Now().UTC().Add(time.Hour)
		in.Reopen = &service.ReopenRequest{ExpiresAt: &exp}
	}
	p, _, err := svc.Command(context.Background(), in)
	return p, err
}

// tenantTx runs fn in a transaction with app.tenant_id installed (FORCE RLS
// hides every row from a bare query), always rolling back.
func tenantTx(t *testing.T, pool *pgxpool.Pool, tenant string, fn func(ctx context.Context, q func(sql string, args ...any) error, scan func(dst any, sql string, args ...any) error)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
	require.NoError(t, err)
	fn(ctx,
		func(sql string, args ...any) error { _, err := tx.Exec(ctx, sql, args...); return err },
		func(dst any, sql string, args ...any) error { return tx.QueryRow(ctx, sql, args...).Scan(dst) })
}

func count(t *testing.T, pool *pgxpool.Pool, tenant, table string) int {
	t.Helper()
	var n int
	tenantTx(t, pool, tenant, func(_ context.Context, _ func(string, ...any) error, scan func(any, string, ...any) error) {
		require.NoError(t, scan(&n, `SELECT count(*) FROM `+table))
	})
	return n
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestPg_LifecycleGateOutboxAndIdempotency(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	svc, _ := newSvc(pool)

	res := materialize(t, svc, "tenant-a", "m1")
	assert.Equal(t, 12, res.Created)
	again, replayed, err := svc.MaterializePeriods(ctx, service.MaterializeInput{
		Meta: meta("steward", "tenant-a", "m2", "again"), LegalEntityID: "entity-a", CalendarID: "cal-1", FiscalYear: 2026, CalendarVersionID: "v1"})
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, 0, again.Created)
	assert.Equal(t, 12, count(t, pool, "tenant-a", "accounting_periods"))

	var march domain.Period
	for _, p := range res.Periods {
		if p.PeriodKey == "FY2026-P03" {
			march = p
		}
	}
	gate := func(exception bool) *service.Resolution {
		r, err := svc.ResolvePeriodByDate(ctx, service.ResolveInput{TenantID: "tenant-a", LegalEntityID: "entity-a", Date: "2026-03-10", SoftCloseException: exception})
		require.NoError(t, err)
		return r
	}
	assert.True(t, gate(false).PostingAllowed)

	p, err := command(svc, "tenant-a", "closer", "c1", march.PeriodID, domain.CmdSoftClose, 1, "wf1")
	require.NoError(t, err)
	assert.Equal(t, domain.StateSoftClosed, p.State)
	assert.False(t, gate(false).PostingAllowed)
	assert.True(t, gate(true).PostingAllowed)

	// SoD, version conflict and invalid transition against real rows.
	_, err = command(svc, "tenant-a", "closer", "c2", march.PeriodID, domain.CmdHardClose, 2, "wf2")
	de, _ := domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeSoDDenied, de.Code)
	_, err = command(svc, "tenant-a", "approver", "c3", march.PeriodID, domain.CmdHardClose, 1, "wf3")
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeVersionConflict, de.Code)
	_, err = command(svc, "tenant-a", "approver", "c4", march.PeriodID, domain.CmdReclose, 2, "wf4")
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeInvalidTransition, de.Code)

	p, err = command(svc, "tenant-a", "approver", "c5", march.PeriodID, domain.CmdHardClose, 2, "wf5")
	require.NoError(t, err)
	assert.Equal(t, int64(3), p.Version)
	assert.False(t, gate(true).PostingAllowed)

	// Replay by Idempotency-Key returns the original and writes nothing.
	p2, rep, err := svc.Command(ctx, service.CommandInput{Meta: meta("approver", "tenant-a", "c5", "close"), PeriodID: march.PeriodID,
		Command: domain.CmdHardClose, ExpectedVersion: 2, Acc14WorkflowRef: "wf5", ControlSnapshotRef: "snap-wf5"})
	require.NoError(t, err)
	assert.True(t, rep)
	assert.Equal(t, p.Version, p2.Version)

	hist, err := svc.GetPeriodStateHistory(ctx, "tenant-a", march.PeriodID)
	require.NoError(t, err)
	require.Len(t, hist, 3)
	assert.Equal(t, "wf5", hist[2].Acc14WorkflowRef)
	assert.Equal(t, "snap-wf5", hist[2].ControlSnapshotRef)
	assert.Len(t, hist[2].DecisionFingerprint, 64)

	st, err := svc.StatusByKey(ctx, "tenant-a", "entity-a", "FY2026-P03")
	require.NoError(t, err)
	assert.Equal(t, "LOCKED", st)
	_, err = svc.StatusByKey(ctx, "tenant-a", "entity-a", "FY2099-P01")
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodePeriodNotFound, de.Code)
	_, err = svc.ResolvePeriodByDate(ctx, service.ResolveInput{TenantID: "tenant-a", LegalEntityID: "entity-a", Date: "2030-01-01"})
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodePeriodNotFound, de.Code, "no period: not allowed, never OPEN")

	u, err := svc.CalendarUsage(ctx, "tenant-a", "cal-1", "v1")
	require.NoError(t, err)
	require.NotNil(t, u.LatestPeriodEnd)
	assert.Equal(t, "2026-12-31", *u.LatestPeriodEnd)
	assert.True(t, u.HasPostedOrClosedPeriod)

	// Reopen window round-trips through the database and expires at read time.
	p, err = command(svc, "tenant-a", "approver2", "c6", march.PeriodID, domain.CmdAuthorizeReopen, 3, "wf6")
	require.NoError(t, err)
	require.NotNil(t, p.Reopen)
	assert.True(t, gate(false).PostingAllowed)
	expired := service.New(store.New(pool), cal{}, &okProv{}).WithClock(func() time.Time { return time.Now().UTC().Add(2 * time.Hour) })
	r, err := expired.ResolvePeriodByDate(ctx, service.ResolveInput{TenantID: "tenant-a", LegalEntityID: "entity-a", Date: "2026-03-10"})
	require.NoError(t, err)
	assert.False(t, r.PostingAllowed)
	assert.Equal(t, domain.ReasonReopenExpired, r.Reason)

	// 12 PeriodOpened + SoftClosed + HardClosed + Reopened; refusals (SoD) added a control event.
	var n int
	tenantTx(t, pool, "tenant-a", func(_ context.Context, _ func(string, ...any) error, scan func(any, string, ...any) error) {
		require.NoError(t, scan(&n, `SELECT count(*) FROM accounting_period_outbox WHERE event_type = 'PeriodOpened'`))
		assert.Equal(t, 12, n)
		require.NoError(t, scan(&n, `SELECT count(*) FROM accounting_period_outbox WHERE event_type IN ('PeriodSoftClosed','PeriodHardClosed','PeriodReopened')`))
		assert.Equal(t, 3, n)
		require.NoError(t, scan(&n, `SELECT count(*) FROM accounting_period_outbox WHERE event_type = 'PeriodCommandRejected'`))
		assert.Equal(t, 1, n, "the SoD refusal left a control event (its own transaction survived the rollback)")
	})
}

func TestPg_ImmutabilityTriggersAndStateGuard(t *testing.T) {
	pool := requireTestDB(t)
	svc, _ := newSvc(pool)
	res := materialize(t, svc, "tenant-a", "m1")
	id := res.Periods[0].PeriodID

	expectFail := func(name, wantSubstr, sql string, args ...any) {
		tenantTx(t, pool, "tenant-a", func(_ context.Context, exec func(string, ...any) error, _ func(any, string, ...any) error) {
			err := exec(sql, args...)
			require.Error(t, err, name)
			assert.Contains(t, err.Error(), wantSubstr, name)
		})
	}
	expectFail("boundary start", "immutable", `UPDATE accounting_periods SET start_date = start_date + 1`)
	expectFail("boundary end", "immutable", `UPDATE accounting_periods SET end_date = end_date + 1`)
	expectFail("period_key", "immutable", `UPDATE accounting_periods SET period_key = 'X'`)
	expectFail("calendar_version", "immutable", `UPDATE accounting_periods SET calendar_version_id = 'v9'`)
	expectFail("scope", "immutable", `UPDATE accounting_periods SET book_scope = 'B'`)
	expectFail("illegal OPEN -> HARD_CLOSED", "illegal period state transition", `UPDATE accounting_periods SET state = 'HARD_CLOSED', version = version + 1`)
	expectFail("OPEN -> RECLOSED", "illegal period state transition", `UPDATE accounting_periods SET state = 'RECLOSED', version = version + 1`)
	expectFail("legal transition without version bump", "bump version", `UPDATE accounting_periods SET state = 'SOFT_CLOSED'`)
	expectFail("no hard delete", "forbidden", `DELETE FROM accounting_periods`)
	expectFail("no truncate", "forbidden", `TRUNCATE accounting_periods CASCADE`)
	expectFail("history update", "append-only", `UPDATE period_state_history SET reason = 'edited'`)
	expectFail("history delete", "append-only", `DELETE FROM period_state_history`)
	expectFail("history truncate", "append-only", `TRUNCATE period_state_history`)
	expectFail("unknown state", "illegal period state transition", `UPDATE accounting_periods SET state = 'WEIRD'`)
	expectFail("reopen state forged without the lifecycle", "illegal period state transition",
		`UPDATE accounting_periods SET state = 'REOPEN_AUTHORIZED' WHERE period_id = $1`, id)
	expectFail("duplicate instance", "accounting_periods_unique_instance",
		`INSERT INTO accounting_periods (period_id, tenant_id, legal_entity_id, calendar_id, calendar_version_id, period_key, fiscal_year, period_no, start_date, end_date, kind)
		 SELECT gen_random_uuid(), tenant_id, legal_entity_id, calendar_id, calendar_version_id, period_key, fiscal_year, period_no, start_date, end_date, kind FROM accounting_periods LIMIT 1`)
	expectFail("transition history needs evidence", "period_state_history_evidence",
		`INSERT INTO period_state_history (history_id, tenant_id, period_id, from_state, to_state, command, requested_by, reason, expected_version, resulting_version)
		 VALUES (gen_random_uuid(), 'tenant-a', $1, 'OPEN', 'SOFT_CLOSED', 'SOFT_CLOSE', 'x', 'r', 1, 2)`, id)

	// Nothing above changed anything (all rolled back; the triggers themselves refused).
	assert.Equal(t, 12, count(t, pool, "tenant-a", "accounting_periods"))
	assert.Equal(t, 12, count(t, pool, "tenant-a", "period_state_history"))

	// The only legal UPDATE path: a lifecycle step with version + 1.
	tenantTx(t, pool, "tenant-a", func(_ context.Context, exec func(string, ...any) error, _ func(any, string, ...any) error) {
		require.NoError(t, exec(`UPDATE accounting_periods SET state = 'SOFT_CLOSED', version = version + 1 WHERE period_id = $1`, id))
	})
}

func TestPg_TenantIsolationUnderRLS(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	var super bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super))
	if super {
		t.Skip("TEST_DATABASE_URL connects as a superuser/BYPASSRLS role, which makes row-level security inert; use a plain role to run this test")
	}
	svc, _ := newSvc(pool)
	res := materialize(t, svc, "tenant-a", "m1")
	command(svc, "tenant-a", "closer", "c1", res.Periods[0].PeriodID, domain.CmdSoftClose, 1, "wf1") //nolint:errcheck

	// A bare query WITHOUT a tenant sees nothing at all (FORCE RLS): every table.
	for _, tbl := range []string{"accounting_periods", "period_state_history", "accounting_period_idempotency", "accounting_period_outbox"} {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n))
		assert.Zero(t, n, "%s without a tenant context", tbl)
		assert.Positive(t, count(t, pool, "tenant-a", tbl), "%s for the owning tenant", tbl)
		assert.Zero(t, count(t, pool, "tenant-b", tbl), "%s for another tenant", tbl)
	}

	// The service layer: B cannot see, resolve or operate on A's period.
	_, err := svc.GetPeriod(ctx, "tenant-b", res.Periods[0].PeriodID)
	de, _ := domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeNotFound, de.Code)
	_, err = svc.ResolvePeriodByDate(ctx, service.ResolveInput{TenantID: "tenant-b", LegalEntityID: "entity-a", Date: "2026-03-10"})
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodePeriodNotFound, de.Code)
	_, err = command(svc, "tenant-b", "approver", "x1", res.Periods[0].PeriodID, domain.CmdHardClose, 2, "wf9")
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeNotFound, de.Code)

	// WITH CHECK: tenant B's context cannot write a row labelled tenant A.
	tenantTx(t, pool, "tenant-b", func(_ context.Context, exec func(string, ...any) error, _ func(any, string, ...any) error) {
		err := exec(`INSERT INTO accounting_period_idempotency (tenant_id, idempotency_key, operation, request_hash, response) VALUES ('tenant-a','forged','op','h','{}')`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "row-level security")
	})

	// Tenant B can have its own periods for the same entity id and the same keys.
	resB := materialize(t, svc, "tenant-b", "m1")
	assert.Equal(t, 12, resB.Created)
	assert.Equal(t, 12, count(t, pool, "tenant-b", "accounting_periods"))
}

// The gate is read from the primary inside its own transaction: while two
// hard-closes race, every gate answer is internally consistent (state matches
// version), versions never go backwards, and once the winner has returned every
// later answer shows its result.
func TestPg_ConcurrentHardCloseExactlyOneWinsAndGateStaysConsistent(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	svc, _ := newSvc(pool)
	res := materialize(t, svc, "tenant-a", "m1")
	id := res.Periods[2].PeriodID // FY2026-P03
	_, err := command(svc, "tenant-a", "closer", "c1", id, domain.CmdSoftClose, 1, "wf-soft")
	require.NoError(t, err)

	stop := make(chan struct{})
	type obs struct {
		state   domain.State
		version int64
	}
	var observed []obs
	var readerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r, err := svc.ResolvePeriodByDate(ctx, service.ResolveInput{TenantID: "tenant-a", LegalEntityID: "entity-a", Date: "2026-03-10"})
			if err != nil {
				readerErr = err
				return
			}
			observed = append(observed, obs{r.State, r.Version})
		}
	}()

	start := make(chan struct{})
	results := make([]error, 2)
	var racers sync.WaitGroup
	for i, who := range []string{"approver-1", "approver-2"} {
		racers.Add(1)
		go func() {
			defer racers.Done()
			<-start
			_, results[i] = command(svc, "tenant-a", who, "race-"+who, id, domain.CmdHardClose, 2, "wf-hard-"+who)
		}()
	}
	close(start)
	racers.Wait()

	wins, conflicts := 0, 0
	for _, err := range results {
		if err == nil {
			wins++
			continue
		}
		de, ok := domain.AsError(err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, domain.CodeVersionConflict, de.Code)
		conflicts++
	}
	assert.Equal(t, 1, wins, "exactly one hard-close wins")
	assert.Equal(t, 1, conflicts)

	// After the winner returned, the very next gate read shows its result.
	r, err := svc.ResolvePeriodByDate(ctx, service.ResolveInput{TenantID: "tenant-a", LegalEntityID: "entity-a", Date: "2026-03-10"})
	require.NoError(t, err)
	assert.Equal(t, domain.StateHardClosed, r.State)
	assert.Equal(t, int64(3), r.Version)
	assert.False(t, r.PostingAllowed)
	close(stop)
	wg.Wait()
	require.NoError(t, readerErr)

	require.NotEmpty(t, observed)
	var last int64
	for _, o := range observed {
		assert.GreaterOrEqual(t, o.version, last, "gate versions never go backwards")
		last = o.version
		switch o.version {
		case 2:
			assert.Equal(t, domain.StateSoftClosed, o.state)
		case 3:
			assert.Equal(t, domain.StateHardClosed, o.state)
		default:
			t.Fatalf("unexpected version %d seen by the gate", o.version)
		}
	}

	hist, err := svc.GetPeriodStateHistory(ctx, "tenant-a", id)
	require.NoError(t, err)
	hardCloses := 0
	for _, h := range hist {
		if h.Command == domain.CmdHardClose {
			hardCloses++
		}
	}
	assert.Equal(t, 1, hardCloses, "one history row, one winner")
	tenantTx(t, pool, "tenant-a", func(_ context.Context, _ func(string, ...any) error, scan func(any, string, ...any) error) {
		var n int
		require.NoError(t, scan(&n, `SELECT count(*) FROM accounting_period_outbox WHERE event_type = 'PeriodHardClosed'`))
		assert.Equal(t, 1, n)
	})
}

func TestPg_ConcurrentMaterializeCreatesEachPeriodOnce(t *testing.T) {
	pool := requireTestDB(t)
	svc, _ := newSvc(pool)
	var wg sync.WaitGroup
	created := make([]int, 4)
	errs := make([]error, 4)
	start := make(chan struct{})
	for i := range created {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, _, err := svc.MaterializePeriods(context.Background(), service.MaterializeInput{
				Meta: meta("steward", "tenant-a", fmt.Sprintf("mk-%d", i), "year start"), LegalEntityID: "entity-a",
				CalendarID: "cal-1", FiscalYear: 2026, CalendarVersionID: "v1"})
			errs[i] = err
			if r != nil {
				created[i] = r.Created
			}
		}()
	}
	close(start)
	wg.Wait()
	total := 0
	for i := range created {
		require.NoError(t, errs[i])
		total += created[i]
	}
	assert.Equal(t, 12, total, "across 4 racing materialisations each period was created exactly once")
	assert.Equal(t, 12, count(t, pool, "tenant-a", "accounting_periods"))
	assert.Equal(t, 12, count(t, pool, "tenant-a", "accounting_period_outbox"))
}

func TestPg_ClaimOutboxPublishesOnce(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	st := store.New(pool)
	svc, _ := newSvc(pool)
	materialize(t, svc, "tenant-a", "m1")

	var got int
	require.NoError(t, st.ClaimOutbox(ctx, 100, func(recs []store.OutboxRecord) error { got = len(recs); return nil }))
	assert.Equal(t, 12, got)
	got = 0
	require.NoError(t, st.ClaimOutbox(ctx, 100, func(recs []store.OutboxRecord) error { got = len(recs); return nil }))
	assert.Equal(t, 0, got, "published rows are not claimed again")
	pending, _, err := st.OutboxDepth(ctx)
	require.NoError(t, err)
	assert.Zero(t, pending)
}
