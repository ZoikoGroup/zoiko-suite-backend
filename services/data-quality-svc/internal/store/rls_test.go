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

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/store"
)

// Real-Postgres tests for data-quality-svc (DATA-02), run as a
// purpose-created NOSUPERUSER NOBYPASSRLS role.

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
		DROP TABLE IF EXISTS dq_certifications CASCADE;
		DROP FUNCTION IF EXISTS enforce_dq_certification_immutability() CASCADE;
		DROP TABLE IF EXISTS dq_issues CASCADE;
		DROP FUNCTION IF EXISTS enforce_dq_issue_assign_only() CASCADE;
		DROP TABLE IF EXISTS dq_results CASCADE;
		DROP FUNCTION IF EXISTS enforce_dq_result_immutability() CASCADE;
		DROP TABLE IF EXISTS dq_runs CASCADE;
		DROP FUNCTION IF EXISTS enforce_dq_run_lifecycle() CASCADE;
		DROP TABLE IF EXISTS dq_rule_set_versions CASCADE;
		DROP FUNCTION IF EXISTS enforce_dq_rule_set_version_immutability() CASCADE;
		DROP TABLE IF EXISTS dq_rule_sets CASCADE;
		DROP FUNCTION IF EXISTS enforce_dq_rule_set_immutability() CASCADE;
		DROP TABLE IF EXISTS idempotency_keys CASCADE;
		DROP TABLE IF EXISTS outbox_events CASCADE;`)

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

func strp(s string) *string { return &s }

// publishRuleSet publishes a rule set with the given rules under a fresh
// name, so tests don't collide on dq_rule_sets' (tenant_id, name) uniqueness.
func (f *fixture) publishRuleSet(org, name string, rules []domain.RuleDefinition) *domain.DQRuleSetVersion {
	f.t.Helper()
	v, err := f.s.PublishRuleSetVersion(f.ctx, org, name, rules, "test-operator", f.claim("PublishRuleSetVersion", name))
	if err != nil {
		f.t.Fatalf("publish rule set version: %v", err)
	}
	return v
}
