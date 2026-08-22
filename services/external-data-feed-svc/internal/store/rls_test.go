package store_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/external-data-feed-svc/internal/domain"
	"zoiko.io/external-data-feed-svc/internal/middleware"
	"zoiko.io/external-data-feed-svc/internal/store"
)

// Row-level security tests for data_feed_subscriptions and data_feed_events (migration
// 002_add_rls.sql), plus the PgStore tenant predicates added alongside it.
//
// These run as a purpose-created NOSUPERUSER NOBYPASSRLS role.
// TEST_DATABASE_URL points at `postgres`, a SUPERUSER, and a superuser
// bypasses row-level security unconditionally — FORCE included — so an
// isolation assertion made over that connection would prove nothing about
// the policy.

func openAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS data_feed_events, data_feed_subscriptions CASCADE;`)

	// Every migration in filename order, never one hardcoded name — a
	// suite that names migrations individually silently skips new ones,
	// which would leave the migration under test unapplied and the run
	// green for the wrong reason.
	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var migs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") && !strings.Contains(e.Name(), ".down.") {
			migs = append(migs, e.Name())
		}
	}
	if len(migs) == 0 {
		t.Fatalf("no migrations found in %s", migDir)
	}
	sort.Strings(migs)
	for _, name := range migs {
		sqlBytes, err := os.ReadFile(filepath.Join(migDir, name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
	return pool
}

func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	const appRole = "zoiko_app_test"
	const appPassword = "zoiko_app_test_pw"

	if _, err := admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+appRole+`') THEN
			CREATE ROLE `+appRole+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	for _, stmt := range []string{
		`ALTER ROLE ` + appRole + ` WITH LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ` + appRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + appRole,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("grant (%s): %v", stmt, err)
		}
	}

	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.User = url.UserPassword(appRole, appPassword)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", appRole, err)
	}
	t.Cleanup(pool.Close)

	var isSuper, bypassRLS bool
	if err := pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuper, &bypassRLS); err != nil {
		t.Fatalf("verify privileges: %v", err)
	}
	if isSuper || bypassRLS {
		t.Fatalf("%s must be NOSUPERUSER and NOBYPASSRLS, got rolsuper=%v rolbypassrls=%v", appRole, isSuper, bypassRLS)
	}
	return pool
}

func TestRLS_EnabledAndForced(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)

	for _, table := range []string{"data_feed_subscriptions", "data_feed_events"} {
		var enabled, forced bool
		if err := admin.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table,
		).Scan(&enabled, &forced); err != nil {
			t.Fatalf("read pg_class for %s: %v", table, err)
		}
		if !enabled {
			t.Errorf("%s: migration 002 must ENABLE row level security", table)
		}
		if !forced {
			t.Errorf("%s: migration 002 must FORCE row level security", table)
		}
	}
}

// TestRLS_PgStore_TenantIsolation exercises the real PgStore as an
// ordinary role: tenant B must not reach tenant A's subscription, and
// tenant A must still reach its own.
func TestRLS_PgStore_TenantIsolation(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	sub := &domain.DataFeedSubscription{
		LegalEntityID: "le-a", Provider: "BLOOMBERG", FeedType: "MARKET_DATA",
		Symbol: "AAPL", Status: "ACTIVE",
	}
	if err := s.CreateSubscription(ctxA, sub); err != nil {
		t.Fatalf("create tenant A's subscription: %v", err)
	}

	if got, err := s.GetSubscriptionByID(ctxB, sub.FeedID); err == nil {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's subscription: %+v", got)
	}

	list, err := s.ListSubscriptions(ctxB, "")
	if err != nil {
		t.Fatalf("list as tenant B: %v", err)
	}
	for _, x := range list {
		if x.TenantID != "tenant-b" {
			t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered list returned tenant %q's row", x.TenantID)
		}
	}

	own, err := s.GetSubscriptionByID(ctxA, sub.FeedID)
	if err != nil {
		t.Fatalf("tenant A must still read its own subscription: %v", err)
	}
	if own.Provider != "BLOOMBERG" {
		t.Fatalf("unexpected subscription returned for tenant A: %+v", own)
	}
}

// TestRLS_PgStore_ListEvents_NoFeedFilter_DoesNotLeak covers this
// service's worst self-disabling filter: ListEvents' ONLY predicate was
// `($1 = '' OR feed_id = $1)` on feed_id itself, so calling it with no
// feed_id returned up to 500 of EVERY tenant's feed events — including the
// payload, i.e. the actual data flowing through their subscriptions.
func TestRLS_PgStore_ListEvents_NoFeedFilter_DoesNotLeak(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	ctxB := middleware.WithTenant(context.Background(), "tenant-b")

	subA := &domain.DataFeedSubscription{
		LegalEntityID: "le-a", Provider: "BLOOMBERG", FeedType: "MARKET_DATA", Symbol: "AAPL", Status: "ACTIVE",
	}
	if err := s.CreateSubscription(ctxA, subA); err != nil {
		t.Fatalf("create tenant A's subscription: %v", err)
	}
	if err := s.IngestEvent(ctxA, &domain.DataFeedEvent{
		FeedID: subA.FeedID, EventType: "PRICE_TICK",
		Payload: map[string]any{"price": 123.45, "confidential": "tenant-a-only"},
	}); err != nil {
		t.Fatalf("ingest tenant A's event: %v", err)
	}

	// Tenant B, no feed filter at all — the exact call that used to leak.
	events, err := s.ListEvents(ctxB, "")
	if err != nil {
		t.Fatalf("list events as tenant B: %v", err)
	}
	for _, ev := range events {
		if ev.TenantID != "tenant-b" {
			t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered ListEvents returned tenant %q's event %s (payload=%v)", ev.TenantID, ev.EventID, ev.Payload)
		}
	}
	if len(events) != 0 {
		t.Fatalf("expected tenant B to see no events, got %d: %+v", len(events), events)
	}

	// Sanity: tenant A does see its own event.
	ownEvents, err := s.ListEvents(ctxA, "")
	if err != nil {
		t.Fatalf("list events as tenant A: %v", err)
	}
	if len(ownEvents) != 1 {
		t.Fatalf("expected tenant A to see its own 1 event, got %d", len(ownEvents))
	}
}

// TestRLS_WithCheckRefusesForeignTenantWrite covers the write side: USING
// alone governs visibility, so without WITH CHECK a caller could insert a
// row attributed to another tenant that it then cannot read back.
func TestRLS_WithCheckRefusesForeignTenantWrite(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)

	conn, err := appPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'tenant-b', false)"); err != nil {
		t.Fatalf("set tenant scope: %v", err)
	}
	_, err = conn.Exec(ctx, `
		INSERT INTO data_feed_subscriptions
			(feed_id, tenant_id, legal_entity_id, provider, feed_type, status)
		VALUES ('forged-1', 'tenant-a', 'le-a', 'FORGED', 'MARKET_DATA', 'ACTIVE')`)
	if err == nil {
		t.Fatal("WITH CHECK must refuse an insert attributed to another tenant")
	}
}
