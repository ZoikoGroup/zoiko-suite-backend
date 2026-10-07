package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/data-ingestion-svc/internal/domain"
	"zoiko.io/data-ingestion-svc/internal/store"
)

// Real-Postgres tests for data-ingestion-svc (DATA-01), run as a
// purpose-created NOSUPERUSER NOBYPASSRLS role — the same doctrine as
// every other service in this repo: a superuser connection bypasses RLS
// unconditionally, so an isolation assertion made over one would prove
// nothing.

const (
	orgA = "11111111-1111-1111-1111-111111111111"
	orgB = "22222222-2222-2222-2222-222222222222"
)

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

	_, _ = pool.Exec(ctx, `
		DROP TABLE IF EXISTS landed_records CASCADE;
		DROP FUNCTION IF EXISTS enforce_landed_record_immutability() CASCADE;
		DROP TABLE IF EXISTS quarantine_items CASCADE;
		DROP FUNCTION IF EXISTS enforce_quarantine_immutability() CASCADE;
		DROP TABLE IF EXISTS landing_objects CASCADE;
		DROP FUNCTION IF EXISTS enforce_landing_immutability() CASCADE;
		DROP TABLE IF EXISTS source_checkpoints CASCADE;
		DROP FUNCTION IF EXISTS enforce_checkpoint_no_delete() CASCADE;
		DROP TABLE IF EXISTS ingestion_runs CASCADE;
		DROP FUNCTION IF EXISTS enforce_run_lifecycle(), update_ingestion_run_updated_at() CASCADE;
		DROP TABLE IF EXISTS idempotency_keys CASCADE;
		DROP TABLE IF EXISTS outbox_events CASCADE;`)

	// Every migration in filename order, never one hardcoded name — a
	// suite that names migrations individually silently skips new ones.
	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var migs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
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
		t.Fatalf("connect as app role: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type fixture struct {
	t     *testing.T
	ctx   context.Context
	admin *pgxpool.Pool
	app   *pgxpool.Pool
	s     *store.PgStore
	mu    sync.Mutex
	keys  int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	return &fixture{t: t, ctx: context.Background(), admin: admin, app: app, s: store.NewPgStore(app)}
}

func (f *fixture) claim(op, resource string) domain.IdempotencyClaim {
	f.mu.Lock()
	f.keys++
	n := f.keys
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", op, resource, n)))
	return domain.IdempotencyClaim{
		OwnerScope: domain.SellerScope, PrincipalID: "test-operator", Key: fmt.Sprintf("key-%d", n),
		Operation: op, RequestSHA256: hex.EncodeToString(sum[:]), ResourceID: resource,
	}
}

// claimWithKey lets a test reuse the exact same idempotency key across two
// calls (to test replay), instead of the auto-incrementing default above.
func (f *fixture) claimWithKey(op, resource, key string) domain.IdempotencyClaim {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s", op, resource, key)))
	return domain.IdempotencyClaim{
		OwnerScope: domain.SellerScope, PrincipalID: "test-operator", Key: key,
		Operation: op, RequestSHA256: hex.EncodeToString(sum[:]), ResourceID: resource,
	}
}

// countAsTenant runs a raw COUNT query as the app role, having first
// declared app.tenant_id on that connection — the same RLS scoping the
// store package applies internally. A raw f.app.QueryRow without this
// would silently return 0 regardless of what's actually in the table
// (RLS filters every row out, not an error), making a "== 0" assertion
// pass for the wrong reason.
func (f *fixture) countAsTenant(tenantID, query string, args ...interface{}) int {
	f.t.Helper()
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		f.t.Fatalf("declare tenant: %v", err)
	}
	var count int
	if err := tx.QueryRow(f.ctx, query, args...).Scan(&count); err != nil {
		f.t.Fatalf("count query: %v", err)
	}
	return count
}

func (f *fixture) startRun(org, sourceID string) *domain.IngestionRun {
	f.t.Helper()
	run := &domain.IngestionRun{
		SourceID: sourceID, CreatedBy: "test-operator",
		ResidencyRegion: "us-east", Classification: "internal", Purpose: "analytics",
	}
	got, err := f.s.StartIngestion(f.ctx, org, run, f.claim("StartIngestion", sourceID))
	if err != nil {
		f.t.Fatalf("start ingestion: %v", err)
	}
	return got
}
