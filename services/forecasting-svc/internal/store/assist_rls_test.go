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

	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/store"
)

// Real-Postgres tests for the AI-05 Forecast Assist governed advisory
// layer (ZS-SVC-N-001 §4/§13 Wave 8), additive to forecasting-svc, run
// as a purpose-created NOSUPERUSER NOBYPASSRLS role.

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
		DROP TABLE IF EXISTS idempotency_keys CASCADE;
		DROP TABLE IF EXISTS planner_decisions CASCADE;
		DROP TABLE IF EXISTS evidence_references CASCADE;
		DROP TABLE IF EXISTS suggested_ranges CASCADE;
		DROP TABLE IF EXISTS suggested_drivers CASCADE;
		DROP TABLE IF EXISTS forecast_assist_jobs CASCADE;
		DROP TABLE IF EXISTS forecast_model_releases CASCADE;
		DROP TABLE IF EXISTS forecast_projections CASCADE;
		DROP TABLE IF EXISTS forecast_models CASCADE;
		DROP FUNCTION IF EXISTS enforce_forecast_job_lifecycle() CASCADE;
		DROP FUNCTION IF EXISTS reject_immutable_row() CASCADE;
		DROP FUNCTION IF EXISTS reject_delete() CASCADE;`)

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

func (f *fixture) claim(op, resource string) domain.AssistIdempotencyClaim {
	f.mu.Lock()
	f.keys++
	n := f.keys
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", op, resource, n)))
	return domain.AssistIdempotencyClaim{
		OwnerScope: domain.AssistSellerScope, PrincipalID: "test-operator", Key: fmt.Sprintf("key-%d", n),
		Operation: op, RequestSHA256: hex.EncodeToString(sum[:]), ResourceID: resource,
	}
}

func (f *fixture) registerRelease(org, domainName, provider, version string) *domain.ForecastModelRelease {
	f.t.Helper()
	rel, err := f.s.RegisterForecastModelRelease(f.ctx, org, domain.RegisterForecastModelReleaseRequest{
		DomainName: domainName, ModelProvider: provider, ModelVersion: version,
	}, "test-operator")
	if err != nil {
		f.t.Fatalf("register forecast model release: %v", err)
	}
	return rel
}

func baseDriversRequest(domainName string) domain.SuggestDriversRequest {
	return domain.SuggestDriversRequest{
		DomainName: domainName, ModelProvider: "internal-forecast", ModelVersion: "v1.0",
		PlanVersion: "plan-2026-q1", PlanningPurpose: "quarterly-planning",
		Drivers:  []domain.DriverInput{{DriverName: "headcount_growth", SuggestedValue: 0.05, Rationale: "hiring plan trend"}},
		Evidence: []domain.EvidenceInput{{SourceRef: "hr-system-export-2026-01", Description: "headcount trend export"}},
	}
}

func (f *fixture) suggestDrivers(org string, req domain.SuggestDriversRequest) *domain.ForecastAssistJob {
	f.t.Helper()
	job, err := f.s.SuggestDrivers(f.ctx, org, req, "test-operator", f.claim("SuggestDrivers", req.JobID+req.PlanVersion))
	if err != nil {
		f.t.Fatalf("suggest drivers: %v", err)
	}
	return job
}
