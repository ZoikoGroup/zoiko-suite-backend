package store_test

// Real-Postgres suite. Same gating convention as the sibling services: skipped
// unless TEST_DATABASE_URL points at a Postgres this machine can use (and
// FAILED instead of skipped under CI / REQUIRE_DB_TESTS, since a skipped
// integration suite reports ok having verified nothing).
//
// Run it as a NOSUPERUSER NOBYPASSRLS role that OWNS the schema: FORCE row-level
// security applies to the owner but never to a superuser, so a superuser DSN
// makes TestPg_RLS skip and proves nothing about isolation. Tables with FORCE
// RLS also return zero rows to a bare query that has not set app.tenant_id, so
// counts below are taken inside a transaction after set_config('app.tenant_id').
//
// STATUS: see RELEASE_CERTIFICATE.md for what has actually been executed.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/fiscal-calendar-svc/internal/domain"
	"zoiko.io/fiscal-calendar-svc/internal/service"
	"zoiko.io/fiscal-calendar-svc/internal/store"
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
		DROP TABLE IF EXISTS fiscal_calendar_outbox, fiscal_calendar_idempotency, fiscal_calendar_status_history,
			calendar_transition_plans, fiscal_calendar_versions, fiscal_calendars CASCADE;
		DROP FUNCTION IF EXISTS calendar_transition_plans_guard() CASCADE;
		DROP FUNCTION IF EXISTS fiscal_calendar_versions_guard() CASCADE;
		DROP FUNCTION IF EXISTS fiscal_calendars_guard() CASCADE;
		DROP FUNCTION IF EXISTS fiscal_calendar_forbid_delete() CASCADE;
		DROP FUNCTION IF EXISTS fiscal_calendar_forbid_mutation() CASCADE;`)
	require.NoError(t, err)

	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")
	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, migrations)
	sort.Strings(migrations)
	for _, p := range migrations {
		sqlText, err := os.ReadFile(p)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(sqlText))
		require.NoError(t, err, "applying %s", filepath.Base(p))
	}
	return pool
}

func notSuperuser(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var super bool
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super))
	if super {
		t.Skip("TEST_DATABASE_URL connects as a superuser/BYPASSRLS role, which makes row-level security inert; use a plain role to run this test")
	}
}

type history struct{ res *service.PeriodHistory }

func (h history) CalendarUsage(context.Context, string, string, string) (*service.PeriodHistory, error) {
	return h.res, nil
}

func newSvc(pool *pgxpool.Pool) (*service.Service, *store.PgStore) {
	st := store.New(pool)
	return service.New(st).WithPeriodHistory(history{res: &service.PeriodHistory{}}), st
}

func meta(actor, tenant, key, reason string) service.Meta {
	return service.Meta{Actor: actor, TenantID: tenant, LegalEntityID: "entity-1", CorrelationID: "corr-1", IdempotencyKey: key, RequestHash: "h-" + key, Reason: reason}
}

func spec(from string, month int) service.VersionSpec {
	d, _ := domain.ParseDate(from)
	return service.VersionSpec{Pattern: json.RawMessage(`{"type":"CALENDAR_MONTHS"}`), StartMonth: month, StartDay: 1, EffectiveFrom: d}
}

func createCal(t *testing.T, svc *service.Service, tenant, key, code, scope, from string) *service.CalendarWithVersion {
	t.Helper()
	res, _, err := svc.CreateFiscalCalendar(context.Background(), service.CreateCalendarInput{
		Meta: meta("proposer-1", tenant, key, "initial"), Code: code, Scope: scope, VersionSpec: spec(from, 1),
	})
	require.NoError(t, err)
	return res
}

func approveActivate(t *testing.T, svc *service.Service, tenant, key string, v *domain.FiscalCalendarVersion) *domain.FiscalCalendarVersion {
	t.Helper()
	ctx := context.Background()
	a, _, err := svc.ApproveVersion(ctx, service.VersionCommandInput{Meta: meta("controller-1", tenant, key+"-a", "ok"), VersionID: v.VersionID, ExpectedVersion: v.Version})
	require.NoError(t, err)
	act, _, err := svc.ActivateVersion(ctx, service.VersionCommandInput{Meta: meta("controller-1", tenant, key+"-b", "go"), VersionID: a.VersionID, ExpectedVersion: a.Version})
	require.NoError(t, err)
	return act
}

// inTenant runs fn in a transaction with app.tenant_id set, then rolls back.
func inTenant(t *testing.T, pool *pgxpool.Pool, tenant string, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
	require.NoError(t, err)
	fn(tx)
}

func count(t *testing.T, tx pgx.Tx, table string) int {
	t.Helper()
	var n int
	require.NoError(t, tx.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n))
	return n
}

// execFails runs one statement in its own tenant transaction and reports the error.
func execFails(t *testing.T, pool *pgxpool.Pool, tenant, sql string, args ...any) error {
	t.Helper()
	var err error
	inTenant(t, pool, tenant, func(tx pgx.Tx) {
		_, err = tx.Exec(context.Background(), sql, args...)
	})
	return err
}

func TestPg_LifecycleSupersessionOutboxAndIdempotency(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	svc, _ := newSvc(pool)

	c := createCal(t, svc, "tenant-a", "k1", "MAIN", "STATUTORY", "2026-01-01")

	// Replay under the same key returns the original; nothing duplicates.
	again, replayed, err := svc.CreateFiscalCalendar(ctx, service.CreateCalendarInput{
		Meta: meta("proposer-1", "tenant-a", "k1", "initial"), Code: "MAIN", Scope: "STATUTORY", VersionSpec: spec("2026-01-01", 1),
	})
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, c.Calendar.CalendarID, again.Calendar.CalendarID)

	// SoD: the proposer cannot approve.
	_, _, err = svc.ApproveVersion(ctx, service.VersionCommandInput{Meta: meta("proposer-1", "tenant-a", "k2", "ok"), VersionID: c.Version.VersionID, ExpectedVersion: 1})
	de, _ := domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeSoDDenied, de.Code)

	v1 := approveActivate(t, svc, "tenant-a", "k3", c.Version)
	assert.Equal(t, domain.VersionActive, v1.Status)

	// Propose and activate a successor effective 2027-04-01 with a different year start.
	v2d, _, err := svc.ProposeCalendarChange(ctx, service.ProposeChangeInput{
		Meta: meta("proposer-1", "tenant-a", "k4", "shift year"), CalendarID: c.Calendar.CalendarID, ExpectedVersion: 2, VersionSpec: spec("2027-04-01", 4),
	})
	require.NoError(t, err)
	assert.Equal(t, 2, v2d.VersionNo)
	v2 := approveActivate(t, svc, "tenant-a", "k5", v2d)
	assert.Equal(t, domain.VersionActive, v2.Status)

	vs, err := svc.ListVersions(ctx, "tenant-a", c.Calendar.CalendarID)
	require.NoError(t, err)
	require.Len(t, vs, 2)
	assert.Equal(t, domain.VersionSuperseded, vs[0].Status)
	assert.Equal(t, "2027-04-01", vs[0].EffectiveTo.String())
	assert.Equal(t, v2.VersionID, vs[0].SupersededByVersion)
	assert.Equal(t, "2026-01-01", vs[0].EffectiveFrom.String(), "history keeps its original start")

	view, err := svc.GetCalendar(ctx, "tenant-a", c.Calendar.CalendarID, ptr(domain.NewDate(2026, 7, 1)))
	require.NoError(t, err)
	assert.Equal(t, v1.VersionID, view.Version.VersionID)
	res, err := svc.Resolve(ctx, "tenant-a", "entity-1", "STATUTORY", domain.NewDate(2027, 5, 1))
	require.NoError(t, err)
	assert.Equal(t, v2.VersionID, res.VersionID)
	_, err = svc.Resolve(ctx, "tenant-a", "entity-1", "STATUTORY", domain.NewDate(2025, 5, 1))
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeNotFound, de.Code)

	pv, err := svc.PreviewPeriods(ctx, "tenant-a", v2.VersionID, 2027)
	require.NoError(t, err)
	assert.Equal(t, "2027-04-01", pv.Periods[0].StartDate.String())
	assert.Equal(t, "2028-03-31", pv.Periods[11].EndDate.String())

	// One event per state change, written in the same transaction. FORCE RLS: count as the tenant.
	inTenant(t, pool, "tenant-a", func(tx pgx.Tx) {
		// Created, Activated(v1), ChangeProposed, Superseded(v1), Activated(v2).
		assert.Equal(t, 5, count(t, tx, "fiscal_calendar_outbox"))
		// v1: >DRAFT, DRAFT>APPROVED, APPROVED>ACTIVE, ACTIVE>SUPERSEDED; v2: >DRAFT, DRAFT>APPROVED, APPROVED>ACTIVE.
		assert.Equal(t, 7, count(t, tx, "fiscal_calendar_status_history"))
		assert.Equal(t, 2, count(t, tx, "fiscal_calendar_versions"))
	})
}

func ptr[T any](v T) *T { return &v }

func TestPg_ExclusionConstraintBacksTheServiceCheck(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	svc, _ := newSvc(pool)
	c := createCal(t, svc, "tenant-a", "k1", "MAIN", "STATUTORY", "2026-01-01")
	approveActivate(t, svc, "tenant-a", "k2", c.Version)

	// The service refuses an overlapping activation with a typed error.
	c2 := createCal(t, svc, "tenant-a", "k3", "SECOND", "STATUTORY", "2026-06-01")
	a, _, err := svc.ApproveVersion(ctx, service.VersionCommandInput{Meta: meta("controller-1", "tenant-a", "k4", "ok"), VersionID: c2.Version.VersionID, ExpectedVersion: 1})
	require.NoError(t, err)
	_, _, err = svc.ActivateVersion(ctx, service.VersionCommandInput{Meta: meta("controller-1", "tenant-a", "k5", "go"), VersionID: a.VersionID, ExpectedVersion: a.Version})
	de, _ := domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeInvalidTransition, de.Code)

	// ...and the database refuses it even when the service check is bypassed.
	err = execFails(t, pool, "tenant-a", `UPDATE fiscal_calendar_versions SET status = 'ACTIVE', activated_by = 'x', activated_at = now(), version = version + 1
		WHERE version_id = $1::uuid`, a.VersionID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fcv_one_in_force_per_interval")

	// A different scope does not conflict (parallel basis).
	c3 := createCal(t, svc, "tenant-a", "k6", "MGMT", "MANAGEMENT", "2026-01-01")
	assert.Equal(t, domain.VersionActive, approveActivate(t, svc, "tenant-a", "k7", c3.Version).Status)
}

func TestPg_ConcurrentOverlappingActivations_ExactlyOneWins(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	svc, _ := newSvc(pool)
	var approved []*domain.FiscalCalendarVersion
	for i, code := range []string{"A", "B", "C", "D"} {
		c := createCal(t, svc, "tenant-a", "c"+code, code, "STATUTORY", "2026-01-01")
		a, _, err := svc.ApproveVersion(ctx, service.VersionCommandInput{Meta: meta("controller-1", "tenant-a", "a"+code, "ok"), VersionID: c.Version.VersionID, ExpectedVersion: 1})
		require.NoError(t, err, i)
		approved = append(approved, a)
	}
	var wg sync.WaitGroup
	results := make([]error, len(approved))
	for i, a := range approved {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, results[i] = svc.ActivateVersion(ctx, service.VersionCommandInput{
				Meta: meta("controller-1", "tenant-a", "act"+a.VersionID, "go"), VersionID: a.VersionID, ExpectedVersion: a.Version})
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
			continue
		}
		de, ok := domain.AsError(err)
		require.True(t, ok, "unexpected error: %v", err)
		assert.Equal(t, domain.CodeInvalidTransition, de.Code)
	}
	assert.Equal(t, 1, wins, "one in-force version per entity/scope/date")
}

func TestPg_ImmutabilityTriggersAndChecks(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	svc, _ := newSvc(pool)
	c := createCal(t, svc, "tenant-a", "k1", "MAIN", "STATUTORY", "2026-01-01")
	a, _, err := svc.ApproveVersion(ctx, service.VersionCommandInput{Meta: meta("controller-1", "tenant-a", "k2", "ok"), VersionID: c.Version.VersionID, ExpectedVersion: 1})
	require.NoError(t, err)
	vid := a.VersionID
	// A second, DRAFT version (for the self-approval check below).
	_, _, err = svc.ProposeCalendarChange(ctx, service.ProposeChangeInput{
		Meta: meta("proposer-1", "tenant-a", "k2b", "later"), CalendarID: c.Calendar.CalendarID, ExpectedVersion: 1, VersionSpec: spec("2027-01-01", 1)})
	require.NoError(t, err)

	bad := func(name, sql string, args ...any) {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, execFails(t, pool, "tenant-a", sql, args...))
		})
	}
	// An APPROVED version's definition never changes.
	bad("pattern", `UPDATE fiscal_calendar_versions SET pattern = '{"type":"WEEK_PATTERN"}', version = version + 1 WHERE version_id = $1::uuid`, vid)
	bad("effective_from", `UPDATE fiscal_calendar_versions SET effective_from = '2020-01-01', version = version + 1 WHERE version_id = $1::uuid`, vid)
	bad("start month", `UPDATE fiscal_calendar_versions SET fiscal_year_start_month = 4, version = version + 1 WHERE version_id = $1::uuid`, vid)
	bad("effective_to on approved", `UPDATE fiscal_calendar_versions SET effective_to = '2030-01-01', version = version + 1 WHERE version_id = $1::uuid`, vid)
	bad("proposer", `UPDATE fiscal_calendar_versions SET proposed_by = 'someone', version = version + 1 WHERE version_id = $1::uuid`, vid)
	bad("approver rewritten", `UPDATE fiscal_calendar_versions SET approved_by = 'someone', version = version + 1 WHERE version_id = $1::uuid`, vid)
	bad("status backwards", `UPDATE fiscal_calendar_versions SET status = 'DRAFT', version = version + 1 WHERE version_id = $1::uuid`, vid)
	bad("status skip", `UPDATE fiscal_calendar_versions SET status = 'SUPERSEDED', effective_to = '2030-01-01', superseded_by_version_id = version_id, version = version + 1 WHERE version_id = $1::uuid`, vid)
	bad("version must increase", `UPDATE fiscal_calendar_versions SET status = 'ACTIVE', activated_by = 'x', activated_at = now() WHERE version_id = $1::uuid`, vid)
	bad("delete version", `DELETE FROM fiscal_calendar_versions WHERE version_id = $1::uuid`, vid)
	bad("delete calendar", `DELETE FROM fiscal_calendars WHERE calendar_id = $1::uuid`, c.Calendar.CalendarID)
	bad("calendar scope", `UPDATE fiscal_calendars SET scope = 'OTHER', version = version + 1 WHERE calendar_id = $1::uuid`, c.Calendar.CalendarID)
	bad("calendar code", `UPDATE fiscal_calendars SET code = 'OTHER', version = version + 1 WHERE calendar_id = $1::uuid`, c.Calendar.CalendarID)
	bad("history update", `UPDATE fiscal_calendar_status_history SET reason = 'edited'`)
	bad("history delete", `DELETE FROM fiscal_calendar_status_history`)
	// Database-level SoD: nobody approves their own proposal.
	bad("self approval", `UPDATE fiscal_calendar_versions SET status = 'APPROVED', approved_by = proposed_by, approved_at = now(), version = version + 1
		WHERE version_id = (SELECT version_id FROM fiscal_calendar_versions WHERE status = 'DRAFT' LIMIT 1)`)
	// Shape checks.
	bad("start day 31", `INSERT INTO fiscal_calendar_versions (version_id, calendar_id, tenant_id, legal_entity_id, scope, version_no, pattern,
		fiscal_year_start_month, fiscal_year_start_day, effective_from, status, proposed_by, proposal_reason)
		SELECT gen_random_uuid(), calendar_id, tenant_id, legal_entity_id, scope, 9, '{"type":"CALENDAR_MONTHS"}', 1, 31, '2030-01-01', 'DRAFT', 'p', 'r' FROM fiscal_calendars`)
	bad("inverted window", `INSERT INTO fiscal_calendar_versions (version_id, calendar_id, tenant_id, legal_entity_id, scope, version_no, pattern,
		fiscal_year_start_month, fiscal_year_start_day, effective_from, effective_to, status, proposed_by, proposal_reason)
		SELECT gen_random_uuid(), calendar_id, tenant_id, legal_entity_id, scope, 9, '{"type":"CALENDAR_MONTHS"}', 1, 1, '2030-01-01', '2029-01-01', 'DRAFT', 'p', 'r' FROM fiscal_calendars`)
	bad("scope mismatch with calendar", `INSERT INTO fiscal_calendar_versions (version_id, calendar_id, tenant_id, legal_entity_id, scope, version_no, pattern,
		fiscal_year_start_month, fiscal_year_start_day, effective_from, status, proposed_by, proposal_reason)
		SELECT gen_random_uuid(), calendar_id, tenant_id, legal_entity_id, 'FORGED', 9, '{"type":"CALENDAR_MONTHS"}', 1, 1, '2030-01-01', 'DRAFT', 'p', 'r' FROM fiscal_calendars`)
	bad("duplicate version_no", `INSERT INTO fiscal_calendar_versions (version_id, calendar_id, tenant_id, legal_entity_id, scope, version_no, pattern,
		fiscal_year_start_month, fiscal_year_start_day, effective_from, status, proposed_by, proposal_reason)
		SELECT gen_random_uuid(), calendar_id, tenant_id, legal_entity_id, scope, 1, '{"type":"CALENDAR_MONTHS"}', 1, 1, '2030-01-01', 'DRAFT', 'p', 'r' FROM fiscal_calendars`)

	// The legitimate transition still works under all those guards.
	act, _, err := svc.ActivateVersion(ctx, service.VersionCommandInput{Meta: meta("controller-1", "tenant-a", "k3", "go"), VersionID: vid, ExpectedVersion: a.Version})
	require.NoError(t, err)
	assert.Equal(t, domain.VersionActive, act.Status)
	// A superseded version is frozen; effective_to is only set by supersession.
	bad("effective_to on active", `UPDATE fiscal_calendar_versions SET effective_to = '2030-01-01', version = version + 1 WHERE version_id = $1::uuid`, vid)
	bad("calendar status backwards", `UPDATE fiscal_calendars SET status = 'DRAFT', version = version + 1 WHERE calendar_id = $1::uuid`, c.Calendar.CalendarID)

	// The refusals come from the intended guards, not incidental errors.
	for want, sql := range map[string]string{
		"fcv_approved_has_independent_approver": `UPDATE fiscal_calendar_versions SET status = 'APPROVED', approved_by = proposed_by, approved_at = now(), version = version + 1 WHERE status = 'DRAFT'`,
		"is immutable":                          `UPDATE fiscal_calendar_versions SET pattern = '{"type":"X"}', version = version + 1`,
		"forbidden":                             `DELETE FROM fiscal_calendar_versions`,
		"only be set once":                      `UPDATE fiscal_calendar_versions SET effective_to = '2031-01-01', version = version + 1`,
	} {
		err := execFails(t, pool, "tenant-a", sql)
		require.Error(t, err, want)
		assert.Contains(t, err.Error(), want)
	}
}

func TestPg_TransitionPlanGuards_AndHistoryRequirement(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	st := store.New(pool)
	d := domain.NewDate(2026, 9, 30)
	svc := service.New(st).WithPeriodHistory(history{res: &service.PeriodHistory{HasPostedOrClosedPeriods: true, LatestPeriodEnd: &d}})

	c := createCal(t, svc, "tenant-a", "k1", "MAIN", "STATUTORY", "2026-01-01")
	v1 := approveActivate(t, svc, "tenant-a", "k2", c.Version)
	v2d, _, err := svc.ProposeCalendarChange(ctx, service.ProposeChangeInput{
		Meta: meta("proposer-1", "tenant-a", "k3", "shift"), CalendarID: c.Calendar.CalendarID, ExpectedVersion: 2, VersionSpec: spec("2026-07-01", 4)})
	require.NoError(t, err)
	v2, _, err := svc.ApproveVersion(ctx, service.VersionCommandInput{Meta: meta("controller-1", "tenant-a", "k4", "ok"), VersionID: v2d.VersionID, ExpectedVersion: 1})
	require.NoError(t, err)

	_, _, err = svc.ActivateVersion(ctx, service.VersionCommandInput{Meta: meta("controller-1", "tenant-a", "k5", "go"), VersionID: v2.VersionID, ExpectedVersion: v2.Version})
	de, _ := domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeTransitionPlanRequired, de.Code)

	plan, _, err := svc.CreateTransitionPlan(ctx, service.CreatePlanInput{
		Meta: meta("proposer-1", "tenant-a", "k6", "plan"), ToVersionID: v2.VersionID, FromVersionID: v1.VersionID, ExpectedVersion: v2.Version,
		ImpactAssessment: json.RawMessage(`{"summary":"x"}`), Mapping: json.RawMessage(`{"FY2026-P07":["FY2026-P07"]}`), AffectsPostedPeriods: true})
	require.NoError(t, err)
	// A second live plan for the same to-version is refused by the service and by the unique index.
	_, _, err = svc.CreateTransitionPlan(ctx, service.CreatePlanInput{
		Meta: meta("proposer-1", "tenant-a", "k7", "plan"), ToVersionID: v2.VersionID, FromVersionID: v1.VersionID, ExpectedVersion: v2.Version,
		ImpactAssessment: json.RawMessage(`{"summary":"x"}`), Mapping: json.RawMessage(`{"a":["b"]}`), AffectsPostedPeriods: true})
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeDuplicateCandidate, de.Code)

	assert.Error(t, execFails(t, pool, "tenant-a", `UPDATE calendar_transition_plans SET mapping = '{"x":["y"]}', version = version + 1 WHERE plan_id = $1::uuid`, plan.PlanID), "plan content is immutable")
	assert.Error(t, execFails(t, pool, "tenant-a", `UPDATE calendar_transition_plans SET status = 'APPROVED', decided_by = proposed_by, decided_at = now(), version = version + 1 WHERE plan_id = $1::uuid`, plan.PlanID), "self-approval of a plan")
	assert.Error(t, execFails(t, pool, "tenant-a", `DELETE FROM calendar_transition_plans`))

	ap, _, err := svc.DecideTransitionPlan(ctx, service.PlanDecisionInput{Meta: meta("controller-1", "tenant-a", "k8", "ok"), PlanID: plan.PlanID, ExpectedVersion: plan.Version, Approve: true})
	require.NoError(t, err)
	assert.Equal(t, domain.PlanApproved, ap.Status)
	assert.Error(t, execFails(t, pool, "tenant-a", `UPDATE calendar_transition_plans SET status = 'REJECTED', decided_by = 'z', decided_at = now(), version = version + 1 WHERE plan_id = $1::uuid`, plan.PlanID), "a decided plan is frozen")

	act, _, err := svc.ActivateVersion(ctx, service.VersionCommandInput{Meta: meta("controller-1", "tenant-a", "k9", "go"), VersionID: v2.VersionID, ExpectedVersion: v2.Version})
	require.NoError(t, err)
	assert.Equal(t, domain.VersionActive, act.Status)
	inTenant(t, pool, "tenant-a", func(tx pgx.Tx) {
		var from string
		require.NoError(t, tx.QueryRow(ctx, `SELECT effective_to::text FROM fiscal_calendar_versions WHERE version_id = $1::uuid`, v1.VersionID).Scan(&from))
		assert.Equal(t, "2026-07-01", from)
	})
}

func TestPg_TenantIsolationUnderRLS(t *testing.T) {
	pool := requireTestDB(t)
	notSuperuser(t, pool)
	ctx := context.Background()
	svc, _ := newSvc(pool)
	c := createCal(t, svc, "tenant-a", "k1", "MAIN", "STATUTORY", "2026-01-01")
	v := approveActivate(t, svc, "tenant-a", "k2", c.Version)

	// Through the service: tenant-b sees nothing of tenant-a.
	_, err := svc.GetCalendar(ctx, "tenant-b", c.Calendar.CalendarID, nil)
	de, _ := domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeNotFound, de.Code)
	_, err = svc.PreviewPeriods(ctx, "tenant-b", v.VersionID, 2026)
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeNotFound, de.Code)
	_, _, err = svc.ApproveVersion(ctx, service.VersionCommandInput{Meta: meta("controller-9", "tenant-b", "kb", "x"), VersionID: v.VersionID, ExpectedVersion: v.Version})
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeNotFound, de.Code)
	_, err = svc.Resolve(ctx, "tenant-b", "entity-1", "STATUTORY", domain.NewDate(2026, 5, 1))
	de, _ = domain.AsError(err)
	require.NotNil(t, de)
	assert.Equal(t, domain.CodeNotFound, de.Code)

	tables := []string{"fiscal_calendars", "fiscal_calendar_versions", "calendar_transition_plans", "fiscal_calendar_status_history", "fiscal_calendar_idempotency", "fiscal_calendar_outbox"}
	// A query that FORGETS its tenant predicate still sees nothing from another tenant's context.
	inTenant(t, pool, "tenant-b", func(tx pgx.Tx) {
		for _, tbl := range tables {
			assert.Equal(t, 0, count(t, tx, tbl), "%s rows of tenant-a are invisible to tenant-b", tbl)
		}
	})
	// ...and with no tenant context at all (FORCE RLS applies to the owner too).
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM fiscal_calendars`).Scan(&n))
	assert.Equal(t, 0, n)
	// The owner's own tenant does see them.
	inTenant(t, pool, "tenant-a", func(tx pgx.Tx) {
		assert.Equal(t, 1, count(t, tx, "fiscal_calendars"))
		assert.Equal(t, 1, count(t, tx, "fiscal_calendar_versions"))
		assert.Greater(t, count(t, tx, "fiscal_calendar_outbox"), 0)
	})
	// WITH CHECK: a tenant cannot write rows for another tenant.
	err = execFails(t, pool, "tenant-b", `INSERT INTO fiscal_calendars (calendar_id, tenant_id, legal_entity_id, code, scope, status, created_by)
		VALUES (gen_random_uuid(), 'tenant-a', 'entity-1', 'FORGED', 'S', 'DRAFT', 'x')`)
	assert.Error(t, err)

	// Same code, scope and idempotency key in tenant-b are independent of tenant-a.
	cb := createCal(t, svc, "tenant-b", "k1", "MAIN", "STATUTORY", "2026-01-01")
	assert.NotEqual(t, c.Calendar.CalendarID, cb.Calendar.CalendarID)
	assert.Equal(t, domain.VersionActive, approveActivate(t, svc, "tenant-b", "k2", cb.Version).Status, "tenant-b's own in-force version does not collide with tenant-a's")
}

func TestPg_ClaimOutboxPublishesOnce(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	svc, st := newSvc(pool)
	createCal(t, svc, "tenant-a", "k1", "MAIN", "STATUTORY", "2026-01-01")

	var got int
	require.NoError(t, st.ClaimOutbox(ctx, 10, func(recs []store.OutboxRecord) error { got = len(recs); return nil }))
	assert.Equal(t, 1, got)
	got = 0
	require.NoError(t, st.ClaimOutbox(ctx, 10, func(recs []store.OutboxRecord) error { got = len(recs); return nil }))
	assert.Equal(t, 0, got, "published rows are not claimed again")
	pending, _, err := st.OutboxDepth(ctx)
	require.NoError(t, err)
	assert.Zero(t, pending)
}
