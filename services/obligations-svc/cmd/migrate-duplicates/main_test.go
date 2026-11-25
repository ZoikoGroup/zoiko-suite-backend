package main

// Real-Postgres, real-shape proof for the WFC-05 data backfill: this test
// seeds tables shaped exactly like obligation-tracking-svc's and
// filing-tracker-svc's own schemas (in two throwaway databases on the same
// disposable test instance, since in production they are three separate
// services' own databases), runs migrateTracking/migrateFiling against a
// stub tenant-entity-registry-svc, and asserts the real rows that land in
// obligations-svc's own schema — not a mock of the mapping logic.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/middleware"
	"zoiko.io/obligations-svc/internal/registry"
	"zoiko.io/obligations-svc/internal/store"
)

const (
	testTenantID      = "aaaaaaaa-0000-0000-0000-000000000001"
	testLegalEntityID = "00000000-0000-0000-0000-000000000001"
	testJurisdiction  = "00000000-0000-0000-0000-00000000000a"
)

// baseTestDSN returns TEST_DATABASE_URL with its dbname replaced, so the
// three logical services this tool migrates between can each get a real,
// separate database on the one disposable test Postgres instance already
// used by internal/store's own tests.
func baseTestDSN(t *testing.T, dbname string) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real PostgreSQL integration test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL does not parse: %v", err)
	}
	if !strings.Contains(strings.ToLower(cfg.ConnConfig.Database), "test") {
		t.Fatalf("refusing to run against database %q: its name must contain \"test\"", cfg.ConnConfig.Database)
	}
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close()
	// DROP + CREATE rather than IF NOT EXISTS: each test run starts these
	// fixture databases empty, same discipline as setupTestDB for the target.
	_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+dbname+` WITH (FORCE)`)
	if _, err := admin.Exec(context.Background(), `CREATE DATABASE `+dbname); err != nil {
		t.Fatalf("create database %s: %v", dbname, err)
	}
	t.Cleanup(func() {
		admin2, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			return
		}
		defer admin2.Close()
		_, _ = admin2.Exec(context.Background(), `DROP DATABASE IF EXISTS `+dbname+` WITH (FORCE)`)
	})

	parts := strings.SplitN(dsn, "/", 4)
	// dsn looks like postgres://user:pass@host:port/dbname?params — swap the
	// path segment only.
	last := parts[len(parts)-1]
	q := ""
	if i := strings.Index(last, "?"); i >= 0 {
		q = last[i:]
	}
	parts[len(parts)-1] = dbname + q
	return strings.Join(parts, "/")
}

func mustPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// setupTargetSchema replays every obligations-svc migration, same approach
// internal/store's own tests use.
func setupTargetSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")
	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	if err != nil || len(migrations) == 0 {
		t.Fatalf("glob migrations: %v (%d found)", err, len(migrations))
	}
	sort.Strings(migrations)
	for _, path := range migrations {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(path), err)
		}
	}
}

const trackingFixtureSchema = `
CREATE TABLE obligations (
    obligation_id    TEXT NOT NULL,
    tenant_id        TEXT NOT NULL,
    legal_entity_id  TEXT NOT NULL,
    source_type      TEXT NOT NULL,
    source_id        TEXT NOT NULL,
    title            TEXT NOT NULL,
    description      TEXT,
    obligation_type  TEXT NOT NULL,
    risk_level       TEXT NOT NULL DEFAULT 'MEDIUM',
    status           TEXT NOT NULL DEFAULT 'PENDING',
    due_date         DATE NOT NULL,
    assigned_to      TEXT,
    fulfilled_at     TIMESTAMPTZ,
    fulfilled_by     TEXT,
    fulfillment_note TEXT,
    effective_from   DATE NOT NULL,
    effective_to     DATE,
    created_by       TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    extracted_by_ai  BOOLEAN NOT NULL DEFAULT false,
    validated_at TIMESTAMPTZ, validated_by TEXT,
    source_clause_id TEXT, trigger_description TEXT, calculation_method TEXT,
    scheduled_at TIMESTAMPTZ, scheduled_by TEXT,
    due_marked_at TIMESTAMPTZ, due_marked_by TEXT,
    in_progress_at TIMESTAMPTZ, in_progress_by TEXT,
    satisfied_at TIMESTAMPTZ, satisfied_by TEXT,
    waived_at TIMESTAMPTZ, waived_by TEXT, waiver_authority_reference TEXT,
    breached_at TIMESTAMPTZ, breached_by TEXT, breach_note TEXT,
    disputed_at TIMESTAMPTZ, disputed_by TEXT, dispute_reason TEXT,
    superseded_by TEXT,
    PRIMARY KEY (obligation_id, tenant_id)
);`

const filingFixtureSchema = `
CREATE TABLE filing_requirements (
    filing_id              TEXT NOT NULL,
    tenant_id              TEXT NOT NULL,
    legal_entity_id        TEXT NOT NULL,
    jurisdiction_id        TEXT NOT NULL,
    filing_authority       TEXT NOT NULL,
    filing_type            TEXT NOT NULL,
    period_key             TEXT NOT NULL,
    due_date               DATE NOT NULL,
    status                 TEXT NOT NULL DEFAULT 'SCHEDULED',
    submission_reference   TEXT,
    submitted_at           TIMESTAMPTZ,
    submitted_by           TEXT,
    confirmation_reference TEXT,
    confirmed_at           TIMESTAMPTZ,
    rejection_reason       TEXT NOT NULL DEFAULT '',
    notes                  TEXT NOT NULL DEFAULT '',
    created_by             TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (filing_id, tenant_id)
);`

// stubRegistry answers PrimaryJurisdiction for exactly one legal entity,
// standing in for tenant-entity-registry-svc.
func stubRegistry(t *testing.T, legalEntityID, tenantID, jurisdictionID string) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, legalEntityID) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"legal_entity_id":"` + legalEntityID + `","tenant_id":"` + tenantID +
			`","primary_jurisdiction_id":"` + jurisdictionID + `"}`))
	}))
	t.Cleanup(srv.Close)
	return registry.New(srv.URL, zap.NewNop())
}

func TestMigrateTracking_RealShapeEndToEnd(t *testing.T) {
	trackingDSN := baseTestDSN(t, "tracking_fixture_test")
	targetDSN := baseTestDSN(t, "target_fixture_test")

	trackingPool := mustPool(t, trackingDSN)
	if _, err := trackingPool.Exec(context.Background(), trackingFixtureSchema); err != nil {
		t.Fatalf("seed tracking schema: %v", err)
	}

	targetPool := mustPool(t, targetDSN)
	setupTargetSchema(t, targetPool)
	s := store.New(targetPool, zap.NewNop())

	fulfilledAt := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	fulfilledBy, fulfillmentNote := "ops-1", "paid in full, receipt #882"
	if _, err := trackingPool.Exec(context.Background(), `
		INSERT INTO obligations (
			obligation_id, tenant_id, legal_entity_id, source_type, source_id, title, obligation_type,
			risk_level, status, due_date, assigned_to, fulfilled_at, fulfilled_by, fulfillment_note,
			effective_from, created_by, created_at, updated_at
		) VALUES (
			'ot-src-1', $1, $2, 'CONTRACT_CLAUSE', 'clause-9', 'Quarterly royalty filing', 'CONTRACTUAL',
			'HIGH', 'SATISFIED', '2025-03-31', 'alex', $3, $4, $5,
			'2024-01-01', 'legacy-admin', '2024-01-01T00:00:00Z', $3
		)`, testTenantID, testLegalEntityID, fulfilledAt, fulfilledBy, fulfillmentNote); err != nil {
		t.Fatalf("seed source row: %v", err)
	}
	// A row this tool must refuse to guess at: an unmapped status.
	if _, err := trackingPool.Exec(context.Background(), `
		INSERT INTO obligations (
			obligation_id, tenant_id, legal_entity_id, source_type, source_id, title, obligation_type,
			status, due_date, effective_from, created_by
		) VALUES ('ot-src-2', $1, $2, 'CONTRACT_CLAUSE', 'clause-10', 'Unknown', 'CONTRACTUAL',
			'SOME_FUTURE_STATUS', '2025-06-30', '2024-01-01', 'legacy-admin')`,
		testTenantID, testLegalEntityID); err != nil {
		t.Fatalf("seed unmapped-status row: %v", err)
	}

	reg := stubRegistry(t, testLegalEntityID, testTenantID, testJurisdiction)
	rep := &report{}
	migrateTracking(context.Background(), trackingPool, reg, s, flags{apply: true}, rep)

	if rep.migratedObligations != 1 {
		t.Fatalf("expected exactly 1 migrated (the unmapped-status row must be skipped), got %d; skipped=%v",
			rep.migratedObligations, rep.skipped)
	}
	if len(rep.skipped) != 1 || !strings.Contains(rep.skipped[0], "unmapped status") {
		t.Fatalf("expected the unmapped-status row reported, got %v", rep.skipped)
	}

	wantID := targetID("obligation-tracking-svc", "ot-src-1")
	got, err := s.FindObligationByID(middleware.WithTenant(context.Background(), testTenantID), wantID)
	if err != nil {
		t.Fatalf("migrated row not found: %v", err)
	}
	if got.ObligationStatus != domain.ExtendedStatusFulfilled {
		t.Fatalf("SATISFIED must map to FULFILLED, got %s", got.ObligationStatus)
	}
	if got.LifecycleModel != domain.LifecycleModelExtended || got.ClockClass != domain.ClockClassContractual {
		t.Fatalf("expected EXTENDED/CONTRACTUAL, got %s/%s", got.LifecycleModel, got.ClockClass)
	}
	if got.JurisdictionID != testJurisdiction {
		t.Fatalf("expected the registry-resolved jurisdiction %s, got %s", testJurisdiction, got.JurisdictionID)
	}
	if got.FulfillmentEvidenceRef == nil || *got.FulfillmentEvidenceRef != fulfillmentNote {
		t.Fatalf("expected fulfillment_note preserved as fulfillment_evidence_ref, got %+v", got.FulfillmentEvidenceRef)
	}
	if got.ResponsibleFunction != "alex" {
		t.Fatalf("expected assigned_to preserved as responsible_function, got %q", got.ResponsibleFunction)
	}

	// Re-running the whole tool must not duplicate the migrated row.
	rep2 := &report{}
	migrateTracking(context.Background(), trackingPool, reg, s, flags{apply: true}, rep2)
	if rep2.migratedObligations != 0 {
		t.Fatalf("expected a repeat run to migrate 0 new rows, got %d", rep2.migratedObligations)
	}

	// A legal entity the registry cannot resolve is skipped, not defaulted.
	if _, err := trackingPool.Exec(context.Background(), `
		INSERT INTO obligations (
			obligation_id, tenant_id, legal_entity_id, source_type, source_id, title, obligation_type,
			status, due_date, effective_from, created_by
		) VALUES ('ot-src-3', $1, 'no-such-entity', 'CONTRACT_CLAUSE', 'clause-11', 'x', 'CONTRACTUAL',
			'ACTIVE', '2025-06-30', '2024-01-01', 'legacy-admin')`, testTenantID); err != nil {
		t.Fatalf("seed unresolvable-entity row: %v", err)
	}
	rep3 := &report{}
	migrateTracking(context.Background(), trackingPool, reg, s, flags{apply: true}, rep3)
	if rep3.migratedObligations != 0 {
		t.Fatalf("expected the unresolvable-entity row skipped, got %d migrated", rep3.migratedObligations)
	}
	found := false
	for _, sk := range rep3.skipped {
		if strings.Contains(sk, "ot-src-3") && strings.Contains(sk, "jurisdiction lookup") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ot-src-3 reported as a jurisdiction-lookup failure, got %v", rep3.skipped)
	}
}

func TestMigrateFiling_RealShapeEndToEnd(t *testing.T) {
	filingDSN := baseTestDSN(t, "filing_fixture_test")
	targetDSN := baseTestDSN(t, "target_fixture_test2")

	filingPool := mustPool(t, filingDSN)
	if _, err := filingPool.Exec(context.Background(), filingFixtureSchema); err != nil {
		t.Fatalf("seed filing schema: %v", err)
	}

	targetPool := mustPool(t, targetDSN)
	setupTargetSchema(t, targetPool)
	s := store.New(targetPool, zap.NewNop())

	if _, err := filingPool.Exec(context.Background(), `
		INSERT INTO filing_requirements (
			filing_id, tenant_id, legal_entity_id, jurisdiction_id, filing_authority, filing_type, period_key,
			due_date, status, created_by
		) VALUES ('fts-1', $1, $2, $3, 'HMRC', 'VAT', '2025-Q2', '2025-07-31', 'CONFIRMED', 'ops-2')`,
		testTenantID, testLegalEntityID, testJurisdiction); err != nil {
		t.Fatalf("seed filing row: %v", err)
	}

	rep := &report{}
	migrateFiling(context.Background(), filingPool, s, flags{apply: true}, rep)
	if rep.migratedObligations != 1 || rep.migratedFilings != 1 {
		t.Fatalf("expected 1 parent obligation + 1 filing migrated, got obligations=%d filings=%d skipped=%v",
			rep.migratedObligations, rep.migratedFilings, rep.skipped)
	}

	tctx := middleware.WithTenant(context.Background(), testTenantID)
	parentID := targetID("filing-tracker-svc", "parent", "fts-1")
	parent, err := s.FindObligationByID(tctx, parentID)
	if err != nil {
		t.Fatalf("parent obligation not found: %v", err)
	}
	if parent.JurisdictionID != testJurisdiction {
		t.Fatalf("expected the filing's own jurisdiction_id used directly (no lookup needed), got %s", parent.JurisdictionID)
	}

	filingID := targetID("filing-tracker-svc", "filing", "fts-1")
	f, err := s.FindFilingRequirement(tctx, parentID, filingID)
	if err != nil {
		t.Fatalf("filing requirement not found: %v", err)
	}
	if f.FilingStatus != "CONFIRMED" {
		t.Fatalf("expected filing_status preserved verbatim, got %s", f.FilingStatus)
	}
	if f.PeriodKey == nil || *f.PeriodKey != "2025-Q2" {
		t.Fatalf("expected period_key preserved, got %+v", f.PeriodKey)
	}

	// Idempotent rerun.
	rep2 := &report{}
	migrateFiling(context.Background(), filingPool, s, flags{apply: true}, rep2)
	if rep2.migratedObligations != 0 || rep2.migratedFilings != 0 {
		t.Fatalf("expected a repeat run to migrate nothing new, got obligations=%d filings=%d",
			rep2.migratedObligations, rep2.migratedFilings)
	}

	// An unmapped filing_status is skipped, not inserted with a guess.
	if _, err := filingPool.Exec(context.Background(), `
		INSERT INTO filing_requirements (
			filing_id, tenant_id, legal_entity_id, jurisdiction_id, filing_authority, filing_type, period_key,
			due_date, status, created_by
		) VALUES ('fts-2', $1, $2, $3, 'HMRC', 'VAT', '2025-Q3', '2025-10-31', 'SOME_FUTURE_STATUS', 'ops-2')`,
		testTenantID, testLegalEntityID, testJurisdiction); err != nil {
		t.Fatalf("seed unmapped-status filing row: %v", err)
	}
	rep3 := &report{}
	migrateFiling(context.Background(), filingPool, s, flags{apply: true}, rep3)
	if rep3.migratedFilings != 0 {
		t.Fatalf("expected the unmapped-status filing row skipped, got %d migrated", rep3.migratedFilings)
	}
	if len(rep3.skipped) != 1 || !strings.Contains(rep3.skipped[0], "unmapped filing_status") {
		t.Fatalf("expected fts-2 reported as an unmapped filing_status, got %v", rep3.skipped)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	filingDSN := baseTestDSN(t, "filing_fixture_dryrun_test")
	targetDSN := baseTestDSN(t, "target_fixture_dryrun_test")

	filingPool := mustPool(t, filingDSN)
	if _, err := filingPool.Exec(context.Background(), filingFixtureSchema); err != nil {
		t.Fatalf("seed filing schema: %v", err)
	}
	targetPool := mustPool(t, targetDSN)
	setupTargetSchema(t, targetPool)
	s := store.New(targetPool, zap.NewNop())

	if _, err := filingPool.Exec(context.Background(), `
		INSERT INTO filing_requirements (
			filing_id, tenant_id, legal_entity_id, jurisdiction_id, filing_authority, filing_type, period_key,
			due_date, status, created_by
		) VALUES ('fts-9', $1, $2, $3, 'HMRC', 'VAT', '2025-Q4', '2026-01-31', 'SCHEDULED', 'ops-2')`,
		testTenantID, testLegalEntityID, testJurisdiction); err != nil {
		t.Fatalf("seed filing row: %v", err)
	}

	rep := &report{}
	migrateFiling(context.Background(), filingPool, s, flags{apply: false}, rep)
	if rep.migratedObligations != 1 || rep.migratedFilings != 1 {
		t.Fatalf("dry run should still count what it WOULD do: obligations=%d filings=%d", rep.migratedObligations, rep.migratedFilings)
	}

	tctx := middleware.WithTenant(context.Background(), testTenantID)
	parentID := targetID("filing-tracker-svc", "parent", "fts-9")
	if _, err := s.FindObligationByID(tctx, parentID); !errors.Is(err, domain.ErrObligationNotFound) {
		t.Fatalf("a dry run must write nothing to the target, got err=%v", err)
	}
}
