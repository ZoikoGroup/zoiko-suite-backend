package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/middleware"
	"zoiko.io/obligations-svc/internal/store"
)

func getTestPool(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real PostgreSQL integration test")
	}
	// setupTestDB drops every table this service's migrations create. Refuse
	// a database whose name does not say it is a test database, so a
	// TEST_DATABASE_URL pointed at a real one cannot wipe it.
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL does not parse: %v", err)
	}
	if db := strings.ToLower(cfg.ConnConfig.Database); !strings.Contains(db, "test") {
		t.Fatalf("refusing to run destructive store tests against database %q: its name must contain \"test\"", cfg.ConnConfig.Database)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("failed to connect to TEST_DATABASE_URL: %v", err)
	}
	return pool
}

// rlsPool connects as a NOSUPERUSER, NOBYPASSRLS role that is not the table
// owner — the runtime role's shape (create-app-roles.sh). The superuser pool
// bypasses row-level security, which is how the overdue sweep's empty tenant
// list went unnoticed: it only works as a superuser.
func rlsPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	const role = "obligations_rls_test"
	for _, stmt := range []string{
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + role + `') THEN
				CREATE ROLE ` + role + ` LOGIN PASSWORD 'rls' NOSUPERUSER NOBYPASSRLS;
			END IF;
		END $$;`,
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + role,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("rls role setup: %v", err)
		}
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = "rls"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// setupTestDB rebuilds the schema by replaying EVERY migration in order, not
// just the initial one. Applying a subset produces a schema no deployed
// database has ever had — here it omitted the tenant_id column that 000003
// adds, so the store's tenant predicates could not compile against it and the
// whole suite failed the moment TEST_DATABASE_URL was set.
func setupTestDB(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()

	// Every up migration in order — DISCOVERED, not listed, so migration 000004
	// cannot reintroduce this. Sorting by filename is what orders them, which is
	// what the zero-padded numeric prefix is for.
	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")

	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("failed to glob migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatalf("no migrations found in %s", migDir)
	}
	sort.Strings(migrations)

	scripts := make([]string, len(migrations))
	var tables []string
	seen := map[string]bool{}
	for i, path := range migrations {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", filepath.Base(path), err)
		}
		scripts[i] = string(sql)
		for _, m := range createTableRe.FindAllStringSubmatch(scripts[i], -1) {
			if name := strings.ToLower(m[1]); !seen[name] {
				seen[name] = true
				tables = append(tables, name)
			}
		}
	}

	// Every table any migration creates must be dropped first, or the second
	// test in a run replays a CREATE TABLE against one that already exists.
	// The list is discovered from the migrations for the same reason they are:
	// a hand-kept list missed applicability_decisions, and later
	// idempotency_keys and event_outbox (000009/000010), failing every test
	// after the first.
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+strings.Join(tables, ", ")+" CASCADE;"); err != nil {
		t.Fatalf("failed to drop tables %v: %v", tables, err)
	}

	for i, path := range migrations {
		if _, err := pool.Exec(ctx, scripts[i]); err != nil {
			t.Fatalf("failed to execute migration %s: %v", filepath.Base(path), err)
		}
	}
}

// createTableRe finds the tables a migration creates, for setupTestDB's drop.
var createTableRe = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)

// testTenantID and otherTenantID are the two scopes the suite runs under.
// Obligations are tenant-scoped as of migration 000003.
const (
	testTenantID  = "aaaaaaaa-0000-0000-0000-000000000001"
	otherTenantID = "bbbbbbbb-0000-0000-0000-000000000002"
)

// tenantCtx carries the tenant the way the store reads it: from the context,
// which in production only the tenant middleware populates. A bare
// context.Background() here is an unscoped call, which the store now refuses —
// so these tests must build the context the middleware would have built.
func tenantCtx(tenantID string) context.Context {
	return middleware.WithTenant(context.Background(), tenantID)
}

func validParams() domain.CreateObligationParams {
	return domain.CreateObligationParams{
		LegalEntityID:        "00000000-0000-0000-0000-000000000001",
		JurisdictionID:       "00000000-0000-0000-0000-000000000002",
		ObligationSourceType: "JURISDICTION_RULE",
		ObligationSourceID:   "rule-1",
		ObligationCode:       "OBL-2026-001",
		ObligationType:       "FILING",
		DueDate:              time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		SeverityLevel:        "HIGH",
		ResponsibleFunction:  "Tax",
		SourceReference:      "IN-GST-FILING-RULE-07",
		CreatedByPrincipalID: "admin-1",
	}
}

func TestPgStore_CreateObligation_IdempotencyAnd409(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	params := validParams()

	// 1. Initial creation
	o1, created, err := s.CreateObligation(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on create: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on initial insert")
	}
	if o1.ObligationCode != "OBL-2026-001" {
		t.Errorf("expected code OBL-2026-001, got %s", o1.ObligationCode)
	}
	if o1.ObligationStatus != "OPEN" {
		t.Errorf("expected default status OPEN, got %s", o1.ObligationStatus)
	}

	// 2. Identical retry (idempotent no-op)
	o2, created, err := s.CreateObligation(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on identical retry")
	}
	if o2.ObligationID != o1.ObligationID {
		t.Errorf("expected ID %s, got %s", o1.ObligationID, o2.ObligationID)
	}

	// 3. Same code, different attribute -> 409
	conflicting := params
	conflicting.SeverityLevel = "CRITICAL"
	conflicting.DueDate = params.DueDate.Add(24 * time.Hour)
	_, _, err = s.CreateObligation(ctx, conflicting)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestPgStore_FindObligationByID_NotFound(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	_, err := s.FindObligationByID(tenantCtx(testTenantID), "00000000-0000-0000-0000-000000000099")
	if !errors.Is(err, domain.ErrObligationNotFound) {
		t.Fatalf("expected ErrObligationNotFound, got %v", err)
	}
}

func TestPgStore_ListObligations_Filters(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	p1 := validParams()
	p1.ObligationCode = "OBL-A"
	p1.LegalEntityID = "00000000-0000-0000-0000-0000000000a1"
	if _, _, err := s.CreateObligation(ctx, p1); err != nil {
		t.Fatalf("create p1: %v", err)
	}

	p2 := validParams()
	p2.ObligationCode = "OBL-B"
	p2.LegalEntityID = "00000000-0000-0000-0000-0000000000b2"
	if _, _, err := s.CreateObligation(ctx, p2); err != nil {
		t.Fatalf("create p2: %v", err)
	}

	results, err := s.ListObligations(ctx, domain.ListObligationsFilter{LegalEntityID: p1.LegalEntityID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].ObligationCode != "OBL-A" {
		t.Fatalf("expected exactly OBL-A for legal_entity_id filter, got %+v", results)
	}

	all, err := s.ListObligations(ctx, domain.ListObligationsFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 obligations with no filter, got %d", len(all))
	}
}

func TestPgStore_UpdateObligationStatus_LegalTransitionsAndIdempotency(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	o, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// OPEN -> IN_PROGRESS: legal
	updated, transitioned, err := s.UpdateObligationStatus(ctx, o.ObligationID, "IN_PROGRESS", 0, "actor-1", "corr-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !transitioned || updated.ObligationStatus != "IN_PROGRESS" {
		t.Fatalf("expected transition to IN_PROGRESS, got status=%s transitioned=%v", updated.ObligationStatus, transitioned)
	}

	// IN_PROGRESS -> IN_PROGRESS: idempotent no-op
	_, transitioned, err = s.UpdateObligationStatus(ctx, o.ObligationID, "IN_PROGRESS", 0, "actor-1", "corr-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transitioned {
		t.Errorf("expected idempotent no-op, got transitioned=true")
	}

	// IN_PROGRESS -> CLOSED: legal, closed_at must be stamped
	closed, transitioned, err := s.UpdateObligationStatus(ctx, o.ObligationID, "CLOSED", 0, "actor-1", "corr-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !transitioned || closed.ObligationStatus != "CLOSED" {
		t.Fatalf("expected transition to CLOSED, got status=%s", closed.ObligationStatus)
	}
	if closed.ClosedAt == nil {
		t.Errorf("expected closed_at to be stamped on transition to CLOSED")
	}

	// CLOSED -> anything: illegal, CLOSED is terminal
	_, _, err = s.UpdateObligationStatus(ctx, o.ObligationID, "OPEN", 0, "actor-1", "corr-1")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition out of terminal CLOSED, got %v", err)
	}
}

func TestPgStore_UpdateObligationStatus_IllegalSkipTransition(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	o, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// OPEN -> OVERDUE is legal (system-driven overdue sweep can skip IN_PROGRESS)
	_, transitioned, err := s.UpdateObligationStatus(ctx, o.ObligationID, "OVERDUE", 0, "actor-1", "corr-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !transitioned {
		t.Fatalf("expected OPEN -> OVERDUE to be legal")
	}

	// OVERDUE -> IN_PROGRESS is illegal (only CLOSED is reachable from OVERDUE)
	_, _, err = s.UpdateObligationStatus(ctx, o.ObligationID, "IN_PROGRESS", 0, "actor-1", "corr-1")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for OVERDUE -> IN_PROGRESS, got %v", err)
	}
}

func TestPgStore_FilingRequirement_CreateAndList(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	o, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatalf("create obligation: %v", err)
	}

	f, err := s.CreateFilingRequirement(ctx, domain.CreateFilingRequirementParams{
		ObligationID:      o.ObligationID,
		FilingType:        "ANNUAL_RETURN",
		FilingAuthority:   "IRS",
		SubmissionChannel: "E_FILE",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.FilingStatus != "PENDING" {
		t.Errorf("expected default filing_status PENDING, got %s", f.FilingStatus)
	}

	list, err := s.ListFilingRequirements(ctx, o.ObligationID, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(list) != 1 || list[0].FilingRequirementID != f.FilingRequirementID {
		t.Fatalf("expected exactly the one filing requirement, got %+v", list)
	}
}

func TestPgStore_CreateFilingRequirement_ObligationNotFound(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	_, err := s.CreateFilingRequirement(tenantCtx(testTenantID), domain.CreateFilingRequirementParams{
		ObligationID:      "00000000-0000-0000-0000-000000000099",
		FilingType:        "ANNUAL_RETURN",
		FilingAuthority:   "IRS",
		SubmissionChannel: "E_FILE",
	})
	if !errors.Is(err, domain.ErrObligationNotFound) {
		t.Fatalf("expected ErrObligationNotFound, got %v", err)
	}
}

// TestPgStore_ObligationCode_IsScopedPerTenant pins the defect migration 000003
// closes. obligation_code carried a GLOBAL unique index and creation is
// idempotent on it, so a second tenant registering an ordinary code was handed
// the FIRST tenant's obligation as a successful create — their legal entity,
// their due date, their source reference — through the documented happy path.
func TestPgStore_ObligationCode_IsScopedPerTenant(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())

	// The same ordinary code, registered by two unrelated tenants.
	shared := validParams()
	shared.ObligationCode = "VAT-Q1-2026"

	first, created, err := s.CreateObligation(tenantCtx(testTenantID), shared)
	if err != nil || !created {
		t.Fatalf("first tenant create: created=%v err=%v", created, err)
	}

	second, created, err := s.CreateObligation(tenantCtx(otherTenantID), shared)
	if err != nil {
		t.Fatalf("second tenant create: %v", err)
	}
	if !created {
		t.Fatal("second tenant was handed the first tenant's obligation as an idempotent no-op")
	}
	if second.ObligationID == first.ObligationID {
		t.Fatalf("both tenants share obligation %s — the code is not tenant-scoped", first.ObligationID)
	}

	// Neither tenant can read the other's by id: another tenant's record reads
	// as not-found, so its existence cannot be probed.
	if _, err := s.FindObligationByID(tenantCtx(otherTenantID), first.ObligationID); !errors.Is(err, domain.ErrObligationNotFound) {
		t.Fatalf("expected ErrObligationNotFound reading across tenants, got %v", err)
	}
	if _, err := s.FindObligationByID(tenantCtx(testTenantID), second.ObligationID); !errors.Is(err, domain.ErrObligationNotFound) {
		t.Fatalf("expected ErrObligationNotFound reading across tenants, got %v", err)
	}
}

// TestPgStore_EventsCommitWithTheMutation pins the transactional-outbox
// guarantee: every obligation event is the row written in the same
// transaction as the mutation it describes, and an idempotent replay or
// no-op adds none. The envelope is checked for the tenant, actor and
// correlation the relay delivers unchanged.
func TestPgStore_EventsCommitWithTheMutation(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	type outboxRow struct {
		eventType     string
		correlationID string
		payload       []byte
	}
	rows := func() []outboxRow {
		t.Helper()
		r, err := pool.Query(context.Background(),
			`SELECT event_type, correlation_id, event_payload FROM event_outbox ORDER BY created_at, outbox_id`)
		if err != nil {
			t.Fatalf("query outbox: %v", err)
		}
		defer r.Close()
		var all []outboxRow
		for r.Next() {
			var row outboxRow
			if err := r.Scan(&row.eventType, &row.correlationID, &row.payload); err != nil {
				t.Fatalf("scan outbox: %v", err)
			}
			all = append(all, row)
		}
		return all
	}

	params := validParams()
	params.CorrelationID = "corr-create"
	created, isNew, err := s.CreateObligation(ctx, params)
	if err != nil || !isNew {
		t.Fatalf("create: created=%v err=%v", isNew, err)
	}

	all := rows()
	if len(all) != 1 || all[0].eventType != "obligation.created" {
		t.Fatalf("expected exactly one obligation.created row, got %+v", all)
	}
	var env struct {
		TenantID      string `json:"tenant_id"`
		EventID       string `json:"event_id"`
		CorrelationID string `json:"correlation_id"`
		ActorID       string `json:"actor_id"`
		Payload       struct {
			ObligationID string `json:"obligation_id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(all[0].payload, &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if env.TenantID != testTenantID || env.EventID == "" || env.CorrelationID != "corr-create" {
		t.Errorf("created envelope wrong: %+v", env)
	}
	if env.Payload.ObligationID != created.ObligationID {
		t.Errorf("payload obligation_id %q, want %q", env.Payload.ObligationID, created.ObligationID)
	}

	// An idempotent replay must not enqueue a second created event.
	if _, isNew, err = s.CreateObligation(ctx, params); err != nil || isNew {
		t.Fatalf("replay: created=%v err=%v", isNew, err)
	}
	if got := len(rows()); got != 1 {
		t.Fatalf("idempotent replay added events: %d rows", got)
	}

	// A real transition enqueues obligation.updated with the actor and
	// correlation the handler verified.
	if _, transitioned, err := s.UpdateObligationStatus(ctx, created.ObligationID, "IN_PROGRESS", 0, "principal-9", "corr-status"); err != nil || !transitioned {
		t.Fatalf("transition: transitioned=%v err=%v", transitioned, err)
	}
	// An idempotent no-op enqueues nothing.
	if _, transitioned, err := s.UpdateObligationStatus(ctx, created.ObligationID, "IN_PROGRESS", 0, "principal-9", "corr-noop"); err != nil || transitioned {
		t.Fatalf("no-op: transitioned=%v err=%v", transitioned, err)
	}
	all = rows()
	if len(all) != 2 || all[1].eventType != "obligation.updated" {
		t.Fatalf("expected obligation.updated after transition, got %+v", all)
	}
	if all[1].correlationID != "corr-status" {
		t.Errorf("correlation on updated row = %q, want corr-status", all[1].correlationID)
	}
	env = struct {
		TenantID      string `json:"tenant_id"`
		EventID       string `json:"event_id"`
		CorrelationID string `json:"correlation_id"`
		ActorID       string `json:"actor_id"`
		Payload       struct {
			ObligationID string `json:"obligation_id"`
		} `json:"payload"`
	}{}
	if err := json.Unmarshal(all[1].payload, &env); err != nil {
		t.Fatalf("updated envelope is not JSON: %v", err)
	}
	if env.ActorID != "principal-9" || env.TenantID != testTenantID {
		t.Errorf("updated envelope wrong: %+v", env)
	}

	// Linking and unlinking a workflow are mutations too.
	if _, err := s.UpdateObligationWorkflow(ctx, created.ObligationID, "00000000-0000-0000-0000-0000000000a1", "principal-9", "corr-link"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := s.UpdateObligationWorkflow(ctx, created.ObligationID, "", "principal-9", "corr-unlink"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	all = rows()
	if len(all) != 4 || all[2].eventType != "obligation.updated" || all[3].eventType != "obligation.updated" {
		t.Fatalf("expected link and unlink to enqueue obligation.updated, got %+v", all)
	}
	if all[2].correlationID != "corr-link" || all[3].correlationID != "corr-unlink" {
		t.Errorf("link/unlink correlations: %q, %q", all[2].correlationID, all[3].correlationID)
	}
}
