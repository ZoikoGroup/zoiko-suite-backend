package store_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/middleware"
	"zoiko.io/exception-escalation-svc/internal/store"
)

// A manual recalculation needs a reason, and the change is recorded: the previous due date,
// the previous rule, the new values, who and why. Before this the due date was overwritten
// in place and the old value was gone.
func TestPgStore_Recalculate_RequiresReasonAndKeepsHistory(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool, store.WithEventRegion("local"))
	due := time.Now().UTC().Add(100 * time.Hour)
	d, err := s.CreateDeadline(ctx(), domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "Quarterly review", DueAt: due,
		CalcRule: "rule-1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)

	_, err = s.Recalculate(ctx(), domain.RecalculateParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "mgr-1", DueAt: due.Add(24 * time.Hour)})
	require.ErrorIs(t, err, domain.ErrDeadlineReasonRequired)
	same, err := s.GetDeadline(ctx(), d.DeadlineID)
	require.NoError(t, err)
	require.WithinDuration(t, due, same.DueAt, time.Second, "a refused recalculation must not move the date")

	second := due.Add(48 * time.Hour)
	_, err = s.Recalculate(ctx(), domain.RecalculateParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "mgr-1",
		DueAt: due.Add(24 * time.Hour), CalcRule: "rule-2", Reason: "vendor delay"})
	require.NoError(t, err)
	_, err = s.Recalculate(ctx(), domain.RecalculateParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "mgr-2",
		DueAt: second, CalcRule: "rule-3", Reason: "audit request"})
	require.NoError(t, err)

	res, err := s.ExplainCalculation(ctx(), "default", d.DeadlineID)
	require.NoError(t, err)
	require.Len(t, res.Changes, 2)
	require.WithinDuration(t, due, res.Changes[0].PreviousDueAt, time.Second)
	require.Equal(t, "rule-1", res.Changes[0].PreviousRule)
	require.Equal(t, "vendor delay", res.Changes[0].Reason)
	require.Equal(t, "mgr-1", res.Changes[0].ChangedBy)
	require.WithinDuration(t, due.Add(24*time.Hour), res.Changes[1].PreviousDueAt, time.Second)
	require.Equal(t, "audit request", res.Changes[1].Reason)

	// The history is append-only.
	_, err = pool.Exec(context.Background(), `UPDATE deadline_due_history SET reason = 'x'`)
	require.Error(t, err)
	_, err = pool.Exec(context.Background(), `DELETE FROM deadline_due_history`)
	require.Error(t, err)
}

// Replaying the same source version of a mirrored deadline is idempotent even after a human
// completed the local row; only a CHANGED source version over an acted-on row is a conflict.
func TestPgStore_MirrorAuthoritativeDeadline_ReplayAfterCompletionIsNotAConflict(t *testing.T) {
	pool := requireTestDB(t)
	s := store.NewPgStore(pool, store.WithEventRegion("local"))
	due := time.Now().UTC().Add(48 * time.Hour)
	p := domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-1", Title: "VAT return", DueAt: due,
		SourceType: "TAX", SourceRef: "vat-replay-1", SourceVersion: "v1", CreatedByPrincipalID: "creator-1",
	}
	first, _, err := s.MirrorAuthoritativeDeadline(ctx(), p)
	require.NoError(t, err)
	_, err = s.CompleteDeadline(ctx(), domain.CompleteDeadlineParams{DeadlineID: first.DeadlineID, TenantID: "default", ActorPrincipalID: "owner-1"})
	require.NoError(t, err)

	again, superseded, err := s.MirrorAuthoritativeDeadline(ctx(), p)
	require.NoError(t, err, "a replay of the same version must not conflict")
	require.Equal(t, first.DeadlineID, again.DeadlineID)
	require.Empty(t, superseded)
	require.Equal(t, domain.DeadlineStatusCompleted, again.Status)

	p.SourceVersion = "v2"
	_, _, err = s.MirrorAuthoritativeDeadline(ctx(), p)
	require.ErrorIs(t, err, domain.ErrDeadlineSourceConflict)
}

// BIZ-08 tenant isolation, run as a NOSUPERUSER NOBYPASSRLS role so row-level security is
// actually in force: tenant B cannot read, list, change or escalate tenant A's deadline, A's
// data is unchanged, and a raw count under B's context sees no rows.
func TestPgStore_Deadline_TenantIsolation(t *testing.T) {
	admin := requireTestDB(t)
	app := nonSuperuserPool(t, admin)
	s := store.NewPgStore(app, store.WithEventRegion("local"))
	ctxA := ctx()
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	due := time.Now().UTC().Add(1 * time.Hour)
	d, err := s.CreateDeadline(ctxA, domain.CreateDeadlineParams{
		TenantID: "default", LegalEntityID: "le-iso", Title: "Isolation", DueAt: due, CalcRule: "r", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	m, _, err := s.MirrorAuthoritativeDeadline(ctxA, domain.MirrorAuthoritativeDeadlineParams{
		TenantID: "default", LegalEntityID: "le-iso", Title: "Mirror", DueAt: due,
		SourceType: "TAX", SourceRef: "iso-1", SourceVersion: "v1", CreatedByPrincipalID: "creator-1",
	})
	require.NoError(t, err)
	_, err = s.Recalculate(ctxA, domain.RecalculateParams{DeadlineID: d.DeadlineID, TenantID: "default", ActorPrincipalID: "m", DueAt: due.Add(time.Hour), Reason: "r"})
	require.NoError(t, err)

	_, err = s.GetDeadline(ctxB, d.DeadlineID)
	require.ErrorIs(t, err, domain.ErrDeadlineNotFound)
	_, err = s.GetSourceDeadline(ctxB, "tenant-b", m.DeadlineID)
	require.ErrorIs(t, err, domain.ErrDeadlineNotFound)
	_, err = s.ExplainCalculation(ctxB, "tenant-b", d.DeadlineID)
	require.ErrorIs(t, err, domain.ErrDeadlineNotFound)
	up, _, err := s.ListUpcoming(ctxB, domain.ListUpcomingParams{TenantID: "tenant-b", LegalEntityID: "le-iso"})
	require.NoError(t, err)
	require.Empty(t, up)
	od, _, err := s.ListOverdue(ctxB, domain.ListOverdueParams{TenantID: "tenant-b", LegalEntityID: "le-iso"})
	require.NoError(t, err)
	require.Empty(t, od)

	_, err = s.AssignOwner(ctxB, domain.AssignOwnerParams{DeadlineID: d.DeadlineID, TenantID: "tenant-b", ActorPrincipalID: "x", OwnerPrincipalID: "x"})
	require.ErrorIs(t, err, domain.ErrDeadlineNotFound)
	_, err = s.CompleteDeadline(ctxB, domain.CompleteDeadlineParams{DeadlineID: d.DeadlineID, TenantID: "tenant-b", ActorPrincipalID: "x"})
	require.ErrorIs(t, err, domain.ErrDeadlineNotFound)
	_, err = s.Recalculate(ctxB, domain.RecalculateParams{DeadlineID: d.DeadlineID, TenantID: "tenant-b", ActorPrincipalID: "x", DueAt: due, Reason: "x"})
	require.ErrorIs(t, err, domain.ErrDeadlineNotFound)
	_, err = s.Waive(ctxB, domain.WaiveParams{DeadlineID: d.DeadlineID, TenantID: "tenant-b", ActorPrincipalID: "x", Reason: "x"})
	require.ErrorIs(t, err, domain.ErrDeadlineNotFound)
	_, err = s.CancelDeadline(ctxB, domain.CancelDeadlineParams{DeadlineID: d.DeadlineID, TenantID: "tenant-b", ActorPrincipalID: "x", Reason: "x"})
	require.ErrorIs(t, err, domain.ErrDeadlineNotFound)
	_, err = s.Escalate(ctxB, domain.EscalateParams{DeadlineID: d.DeadlineID, TenantID: "tenant-b", ActorPrincipalID: "x", EscalatedToRole: "x", Reason: "x"})
	require.ErrorIs(t, err, domain.ErrDeadlineNotFound)

	// A's data is unchanged.
	got, err := s.GetDeadline(ctxA, d.DeadlineID)
	require.NoError(t, err)
	require.Equal(t, domain.DeadlineStatusScheduled, got.Status)
	require.Empty(t, got.OwnerPrincipalID)
	require.WithinDuration(t, due.Add(time.Hour), got.DueAt, time.Second)

	// Raw probe under B's tenant context, no tenant predicate: the database itself shows B nothing.
	tx, err := app.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'tenant-b', true)`)
	require.NoError(t, err)
	for _, table := range []string{"deadlines", "deadline_escalations", "deadline_due_history"} {
		var n int
		require.NoError(t, tx.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n))
		require.Zero(t, n, "RLS leak: tenant B context sees rows in %s", table)
	}
}

// nonSuperuserPool returns a pool connected as a NOSUPERUSER NOBYPASSRLS role that can use the
// tables the admin pool just migrated.
func nonSuperuserPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	bg := context.Background()
	_, err := admin.Exec(bg, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'biz08_app') THEN
			CREATE ROLE biz08_app NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;
	ALTER ROLE biz08_app LOGIN PASSWORD 'biz08' NOSUPERUSER NOBYPASSRLS;
	GRANT USAGE ON SCHEMA public TO biz08_app;
	GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO biz08_app;`)
	require.NoError(t, err)
	cfg := admin.Config().ConnConfig
	u := url.URL{Scheme: "postgres", User: url.UserPassword("biz08_app", "biz08"), Host: cfg.Host + ":" + itoa(int(cfg.Port)), Path: "/" + cfg.Database, RawQuery: "sslmode=disable"}
	app, err := pgxpool.New(bg, u.String())
	require.NoError(t, err)
	t.Cleanup(app.Close)
	return app
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
