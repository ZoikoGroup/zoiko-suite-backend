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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
	"zoiko.io/reconciliation-engine-svc/internal/store"
)

// Real-Postgres tests for reconciliation-engine-svc (DATA-07), run as a
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
		DROP TABLE IF EXISTS reconciliation_run_transitions CASCADE;
		DROP TABLE IF EXISTS outbox_events CASCADE;
		DROP TABLE IF EXISTS idempotency_keys CASCADE;
		DROP TABLE IF EXISTS certifications CASCADE;
		DROP TABLE IF EXISTS reconciliation_exceptions CASCADE;
		DROP TABLE IF EXISTS match_results CASCADE;
		DROP TABLE IF EXISTS population_items CASCADE;
		DROP TABLE IF EXISTS population_snapshots CASCADE;
		DROP TABLE IF EXISTS reconciliation_runs CASCADE;
		DROP TABLE IF EXISTS reconciliation_definitions CASCADE;
		DROP FUNCTION IF EXISTS enforce_exception_resolution() CASCADE;
		DROP FUNCTION IF EXISTS reject_immutable_row() CASCADE;
		DROP FUNCTION IF EXISTS enforce_run_lifecycle() CASCADE;
		DROP FUNCTION IF EXISTS enforce_run_creation() CASCADE;
		DROP FUNCTION IF EXISTS enforce_definition_versioning() CASCADE;`)

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

const (
	entityA = "aaaaaaaa-0000-0000-0000-00000000000a"
	entityB = "bbbbbbbb-0000-0000-0000-00000000000b"
)

// testSpecs are the two population sources every fixture definition names.
func testSpecs() []domain.PopulationSpec {
	return []domain.PopulationSpec{
		{Side: "Bank", System: "BANK", Population: "bank-transactions"},
		{Side: "Ledger", System: "GL", Population: "account-postings", Params: map[string]string{"account_codes": "1100"}},
	}
}

func defineRequest(name string, toleranceMinor int64) domain.DefineReconciliationRequest {
	scale := 2
	return domain.DefineReconciliationRequest{
		LegalEntityID: entityA, Name: name, Populations: testSpecs(), MatchKeyFields: []string{"ref_id"},
		AmountScale: &scale, MaterialityAmountMinorUnits: 100000,
		Tolerance: domain.TolerancePolicy{Currencies: map[string]domain.CurrencyTolerance{"USD": {AbsoluteMinorUnits: &toleranceMinor}}},
	}
}

func (f *fixture) defineReconciliation(org, name string, toleranceMinor int64) *domain.ReconciliationDefinition {
	f.t.Helper()
	d, err := f.s.DefineReconciliation(f.ctx, org, defineRequest(name, toleranceMinor), "test-operator")
	if err != nil {
		f.t.Fatalf("define reconciliation %s: %v", name, err)
	}
	return d
}

// testPop is one side's items as a test writes them; frozen() turns it into
// what the population client hands StartRun after reading a source.
type testPop struct {
	Side         string
	SourceSystem string
	Items        []domain.PopulationItemInput
}

func frozen(p testPop) domain.FrozenPopulation {
	declared := map[string]string{}
	sums := map[string]int64{}
	for _, it := range p.Items {
		sums[it.Currency] += it.AmountMinorUnits
	}
	for c, v := range sums {
		declared[c] = fmt.Sprintf("%d.%02d", v/100, v%100)
	}
	ref := "BANK/bank-transactions"
	if p.Side == "Ledger" {
		ref = "GL/account-postings?account_codes=1100"
	}
	return domain.FrozenPopulation{Side: p.Side, SourceSystem: p.SourceSystem, SpecRef: ref, Watermark: "wm-" + p.Side,
		DeclaredRowCount: int64(len(p.Items)), DeclaredTotals: declared, Items: p.Items}
}

func frozenAll(pops []testPop) []domain.FrozenPopulation {
	out := make([]domain.FrozenPopulation, len(pops))
	for i, p := range pops {
		out[i] = frozen(p)
	}
	return out
}

func (f *fixture) startRunE(org string, def *domain.ReconciliationDefinition, populations []testPop) (*domain.ReconciliationRun, error) {
	return f.s.StartRun(f.ctx, org, domain.StartRunRequest{DefinitionID: def.DefinitionID, PeriodID: "2026-09"}, def.Version,
		frozenAll(populations), "test-operator", f.claim("StartRun", def.DefinitionID))
}

func (f *fixture) startRun(org string, def *domain.ReconciliationDefinition, populations []testPop) *domain.ReconciliationRun {
	f.t.Helper()
	r, err := f.startRunE(org, def, populations)
	if err != nil {
		f.t.Fatalf("start run: %v", err)
	}
	return r
}

var itemSeq int64

func item(refID string, amountMinor int64) domain.PopulationItemInput {
	n := atomic.AddInt64(&itemSeq, 1)
	return domain.PopulationItemInput{SourceRecordID: fmt.Sprintf("rec-%06d", n), RefID: refID, AmountMinorUnits: amountMinor,
		Currency: "USD", OccurredAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
}
