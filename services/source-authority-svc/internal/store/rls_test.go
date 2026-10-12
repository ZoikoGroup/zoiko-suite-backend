package store_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/source-authority-svc/internal/domain"
	svcmiddleware "zoiko.io/source-authority-svc/internal/middleware"
	"zoiko.io/source-authority-svc/internal/outbox"
	"zoiko.io/source-authority-svc/internal/store"
)

// Row-level security tests for source-authority-svc (migration 000002).
//
// These run as a purpose-created NOSUPERUSER NOBYPASSRLS role.
// TEST_DATABASE_URL points at `postgres`, a SUPERUSER, and a superuser
// bypasses row-level security unconditionally — FORCE included — so an
// isolation assertion made over that connection would prove nothing. This
// is the same gap the live deployment had: deployments/docker-compose.yml
// defaulted DATABASE_URL to the postgres superuser, which silently defeated
// migration 000002's FORCE ROW LEVEL SECURITY on normalized_facts — the
// genuine newly-discovered security gap this pass fixes (distinct from the
// audit's stale "RLS not forced" finding: the FORCE clause has existed
// since 000002 and is proven present below; the live bypass was the
// superuser connection, not a missing clause).
//
// source_authority_maps has no tenant_id and no RLS at all — deliberately,
// it is platform-wide reference data — so there is nothing to test for it
// here; TestRLS_NormalizedFactsEnabledAndForced below also asserts that
// source_authority_maps carries NO row-level security, pinning the
// intentional asymmetry between the two tables.

const (
	tenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	tenantB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

func openAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	requireThrowawayDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS normalized_facts, source_authority_maps, outbox_events CASCADE;`)

	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "deployments", "migrations")
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

func ctxFor(tenantID string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenantID)
}

func addFact(t *testing.T, s *store.PgStore, tenantID, fieldFamily, entityRef, sourceSystem, value string) {
	t.Helper()
	f := &domain.NormalizedFact{
		NormalizedFactID: uuid.NewString(), TenantID: tenantID, FieldFamily: fieldFamily, EntityRef: entityRef,
		SourceSystem: sourceSystem, SourceRecord: "rec-1", FactValue: json.RawMessage(value),
		ObservedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC(),
		AuthorityClass: domain.AuthorityClassAuthoritative, CreatedByPrincipalID: "p1",
		CorrelationID: strPtr(uuid.NewString()),
	}
	evt := outbox.Event{AggregateType: "NormalizedFact", AggregateID: f.NormalizedFactID, EventType: "normalized_fact.recorded", Payload: f, TenantID: &tenantID}
	if _, err := s.RecordFact(ctxFor(tenantID), f, evt); err != nil {
		t.Fatalf("record fact (tenant=%s): %v", tenantID, err)
	}
}

func TestRLS_NormalizedFactsEnabledAndForced(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)

	var enabled, forced bool
	if err := admin.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'normalized_facts'`,
	).Scan(&enabled, &forced); err != nil {
		t.Fatalf("read pg_class for normalized_facts: %v", err)
	}
	if !enabled {
		t.Error("normalized_facts: migration 000002 must ENABLE row level security")
	}
	if !forced {
		t.Error("normalized_facts: migration 000002 must FORCE row level security — " +
			"without FORCE, a superuser or table-owner connection (which is what this service's " +
			"old DATABASE_URL default used) bypasses the policy entirely")
	}

	// source_authority_maps is deliberately the opposite: platform-wide
	// reference data, no tenant column, no RLS at all. Pinned here so a
	// future change does not silently add RLS without a tenant column to
	// scope it by (which would just lock everyone out) or silently drop
	// FORCE from normalized_facts without this test catching it.
	var mapsEnabled bool
	if err := admin.QueryRow(ctx,
		`SELECT relrowsecurity FROM pg_class WHERE relname = 'source_authority_maps'`,
	).Scan(&mapsEnabled); err != nil {
		t.Fatalf("read pg_class for source_authority_maps: %v", err)
	}
	if mapsEnabled {
		t.Error("source_authority_maps must NOT have row-level security — it is platform-wide " +
			"reference data with no tenant_id column to scope a policy by")
	}
}

// TestRLS_TenantCannotReadAnotherTenantsFacts is the headline isolation
// property this migration exists for. Before migration 000002,
// normalized_facts had no tenant column at all, and GET /resolve composed
// one tenant's precedence over every tenant's facts.
func TestRLS_TenantCannotReadAnotherTenantsFacts(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	addFact(t, s, tenantA, "PAYROLL_GROSS_PAY", "emp-1", "ADP", `"120000"`)

	facts, err := s.ListNormalizedFacts(ctxFor(tenantB), domain.ListNormalizedFactsFilter{
		TenantID: tenantB, FieldFamily: "PAYROLL_GROSS_PAY", EntityRef: "emp-1", Limit: 100,
	})
	if err != nil {
		t.Fatalf("list as tenant B: %v", err)
	}
	if len(facts) != 0 {
		t.Fatalf("ISOLATION FAILURE: tenant B read %d of tenant A's facts", len(facts))
	}

	// Tenant A still sees its own.
	ownFacts, err := s.ListNormalizedFacts(ctxFor(tenantA), domain.ListNormalizedFactsFilter{
		TenantID: tenantA, FieldFamily: "PAYROLL_GROSS_PAY", EntityRef: "emp-1", Limit: 100,
	})
	if err != nil {
		t.Fatalf("list as tenant A: %v", err)
	}
	if len(ownFacts) != 1 {
		t.Fatalf("tenant A must still see its own fact, got %d", len(ownFacts))
	}
}

// TestRLS_ResolveDoesNotComposeAnotherTenantsFacts is gaps_test.go's
// TestResolve_DoesNotReadAnotherTenantsFacts, proven at the real-database
// layer against a genuinely non-superuser connection rather than the
// in-memory stub.
func TestRLS_ResolveDoesNotComposeAnotherTenantsFacts(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	// The precedence rule is platform-wide and created once.
	m := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(), FieldFamily: "PAYROLL_GROSS_PAY", SourceSystem: "ADP",
		PrecedenceRank: 1, ConflictRoute: "x", EffectiveFrom: time.Now().UTC().Add(-time.Hour),
		CreatedByPrincipalID: "p1", CorrelationID: strPtr(uuid.NewString()),
	}
	if _, err := s.CreateSourceAuthorityMap(context.Background(), m, outbox.Event{
		AggregateType: "SourceAuthorityMap", AggregateID: m.SourceAuthorityMapID,
		EventType: "source_authority_map.created", Payload: m,
	}); err != nil {
		t.Fatalf("create map: %v", err)
	}

	addFact(t, s, tenantA, "PAYROLL_GROSS_PAY", "emp-1", "ADP", `"120000"`)

	res, err := s.ResolveAuthoritativeFact(ctxFor(tenantB), tenantB, "PAYROLL_GROSS_PAY", "emp-1")
	if err != nil {
		t.Fatalf("resolve as tenant B: %v", err)
	}
	if res.AuthoritativeFact != nil {
		t.Fatalf("ISOLATION FAILURE: resolve returned tenant A's fact to tenant B: %+v", res.AuthoritativeFact)
	}
	if len(res.ConflictingFacts) != 0 {
		t.Fatalf("ISOLATION FAILURE: resolve leaked tenant A's facts as conflicts to tenant B")
	}

	// Tenant A's own resolve still works correctly.
	resA, err := s.ResolveAuthoritativeFact(ctxFor(tenantA), tenantA, "PAYROLL_GROSS_PAY", "emp-1")
	if err != nil {
		t.Fatalf("resolve as tenant A: %v", err)
	}
	if resA.AuthoritativeFact == nil || resA.AuthoritativeFact.SourceSystem != "ADP" {
		t.Fatalf("tenant A's own resolve must still work, got %+v", resA.AuthoritativeFact)
	}
}

// TestRLS_SourceAuthorityMapsSharedAcrossTenants pins the deliberate
// opposite of isolation: the precedence register is platform-wide and every
// tenant must see the same rows, over the SAME non-superuser connection
// that isolates normalized_facts. If a future change added a tenant_id to
// this table without updating ListSourceAuthorityMaps, this would catch it.
func TestRLS_SourceAuthorityMapsSharedAcrossTenants(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	m := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(), FieldFamily: "SHARED", SourceSystem: "ADP",
		PrecedenceRank: 1, ConflictRoute: "x", EffectiveFrom: time.Now().UTC().Add(-time.Hour),
		CreatedByPrincipalID: "p1", CorrelationID: strPtr(uuid.NewString()),
	}
	if _, err := s.CreateSourceAuthorityMap(ctxFor(tenantA), m, outbox.Event{
		AggregateType: "SourceAuthorityMap", AggregateID: m.SourceAuthorityMapID,
		EventType: "source_authority_map.created", Payload: m,
	}); err != nil {
		t.Fatalf("create map: %v", err)
	}

	maps, err := s.ListSourceAuthorityMaps(ctxFor(tenantB), domain.ListSourceAuthorityMapsFilter{FieldFamily: "SHARED"})
	if err != nil {
		t.Fatalf("list as tenant B: %v", err)
	}
	if len(maps) != 1 {
		t.Fatalf("precedence rules are platform reference data and must be visible to every tenant, got %d", len(maps))
	}
}
