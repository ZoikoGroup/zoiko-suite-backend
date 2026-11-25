package store_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/carta-svc/internal/domain"
	"zoiko.io/carta-svc/internal/store"
	"zoiko.io/carta-svc/internal/testdb"
)

func newTenant() string { return "tenant-" + uuid.NewString() }

func evaluate(le, subject string, trust int) *domain.CartaAssessment {
	return domain.EvaluateAccess(&domain.EvaluateRequest{
		LegalEntityID: le,
		Context: domain.AccessContext{
			SubjectID: subject, SubjectType: "USER", DeviceTrustLevel: trust,
			IPAddress: "10.0.0.1", IsKnownLocation: false, ResourceSensitivity: "RESTRICTED",
			ActionRequested: "READ", TimeOfDayHour: 3,
		},
	}, "")
}

// asTenant runs fn as the pool's role with app.tenant_id installed, the way
// the store does, so raw statements are subject to the same RLS policy.
func asTenant(t *testing.T, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Decision records survive a restart: written through one pool, read back
// through a brand-new pool and store, field for field.
func TestPgStore_PersistsAcrossRestart(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	ctx := context.Background()
	tenant := newTenant()

	asm := evaluate("le-1", "subject-1", 20)
	if err := store.NewPgStore(app).SaveAssessment(ctx, tenant, asm); err != nil {
		t.Fatalf("save: %v", err)
	}

	restarted, err := pgxpool.New(ctx, pools.AppDSN)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer restarted.Close()
	got, err := store.NewPgStore(restarted).GetAssessmentByID(ctx, tenant, asm.ID)
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if !reflect.DeepEqual(got, asm) {
		t.Fatalf("after restart:\n got  %+v\n want %+v", got, asm)
	}
	list, err := store.NewPgStore(restarted).ListAssessments(ctx, tenant, "le-1", "")
	if err != nil || len(list) != 1 || list[0].ID != asm.ID {
		t.Fatalf("list after restart = %+v, %v", list, err)
	}
}

// Another tenant can neither read nor list an assessment, and cannot update
// or delete it either: under its own app.tenant_id the row is invisible
// (0 rows affected). The owning tenant cannot rewrite it, because decision
// records are append-only.
func TestPgStore_CrossTenantReadUpdateDeleteDenied(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	ctx := context.Background()
	s := store.NewPgStore(app)
	tenantA, tenantB := newTenant(), newTenant()

	asm := evaluate("le-1", "subject-1", 90)
	if err := s.SaveAssessment(ctx, tenantA, asm); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := s.GetAssessmentByID(ctx, tenantB, asm.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant get: err = %v, want ErrNotFound", err)
	}
	if list, err := s.ListAssessments(ctx, tenantB, "le-1", ""); err != nil || len(list) != 0 {
		t.Fatalf("cross-tenant list = %d rows, %v; want 0", len(list), err)
	}

	for _, stmt := range []string{
		`UPDATE carta_assessments SET decision = 'ALLOW' WHERE id = $1`,
		`DELETE FROM carta_assessments WHERE id = $1`,
	} {
		err := asTenant(t, app, tenantB, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, stmt, asm.ID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("%q as another tenant affected %d rows, want 0", stmt, tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%q as another tenant: %v", stmt, err)
		}
	}

	err := asTenant(t, app, tenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE carta_assessments SET decision = 'ALLOW' WHERE id = $1`, asm.ID)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("owner-tenant UPDATE: err = %v, want append-only refusal (42501)", err)
	}
	got, err := s.GetAssessmentByID(ctx, tenantA, asm.ID)
	if err != nil || got.Decision != asm.Decision {
		t.Fatalf("record changed: %+v, %v", got, err)
	}
}

// The policy itself, as a NOSUPERUSER NOBYPASSRLS role: no tenant installed
// means no rows, and an insert stamped with a tenant other than the installed
// one is refused by WITH CHECK.
func TestPgStore_RLSBindsOrdinaryRole(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	ctx := context.Background()
	tenant := newTenant()
	if err := store.NewPgStore(app).SaveAssessment(ctx, tenant, evaluate("le-1", "s", 50)); err != nil {
		t.Fatalf("save: %v", err)
	}

	var n int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM carta_assessments`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("no tenant installed: app role saw %d rows, want 0", n)
	}
	// Negative control: the owner (superuser) does see it, so the 0 above is
	// the policy and not an empty table.
	if err := pools.Admin.QueryRow(ctx, `SELECT count(*) FROM carta_assessments WHERE tenant_id = $1`, tenant).Scan(&n); err != nil || n != 1 {
		t.Fatalf("owner count = %d, %v; want 1", n, err)
	}

	err := asTenant(t, app, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO carta_assessments (id, tenant_id, legal_entity_id, subject_id, context,
			    trust_score, risk_level, decision, risk_factors, assessment_timestamp)
			VALUES ($1, $2, 'le', 's', '{}', 10, 'LOW', 'ALLOW', '[]', now())`,
			uuid.NewString(), newTenant())
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("insert for another tenant: err = %v, want RLS WITH CHECK violation (42501)", err)
	}
}

// A failed write leaves nothing behind: the CHECK constraint refuses an
// out-of-range score and the transaction rolls back.
func TestPgStore_FailedSaveRollsBack(t *testing.T) {
	pools := testdb.Open(t)
	s := store.NewPgStore(pools.Store())
	ctx := context.Background()
	tenant := newTenant()

	bad := evaluate("le-1", "s", 50)
	bad.TrustScore = 101
	if err := s.SaveAssessment(ctx, tenant, bad); err == nil {
		t.Fatal("expected a constraint violation for trust_score 101")
	}
	if bad.ID != "" {
		t.Fatalf("failed save must not assign an id, got %q", bad.ID)
	}
	if list, err := s.ListAssessments(ctx, tenant, "le-1", ""); err != nil || len(list) != 0 {
		t.Fatalf("after failed save: %d rows, %v; want 0", len(list), err)
	}
}

// Subject narrows within tenant + legal entity; newest first.
func TestPgStore_ListScopesAndOrders(t *testing.T) {
	pools := testdb.Open(t)
	s := store.NewPgStore(pools.Store())
	ctx := context.Background()
	tenant := newTenant()

	older := evaluate("le-1", "alice", 90)
	older.AssessmentTimestamp = time.Now().Add(-time.Hour)
	newer := evaluate("le-1", "alice", 10)
	other := evaluate("le-1", "bob", 90)
	otherLE := evaluate("le-2", "alice", 90)
	for _, a := range []*domain.CartaAssessment{older, newer, other, otherLE} {
		if err := s.SaveAssessment(ctx, tenant, a); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	list, err := s.ListAssessments(ctx, tenant, "le-1", "alice")
	if err != nil || len(list) != 2 || list[0].ID != newer.ID || list[1].ID != older.ID {
		t.Fatalf("alice in le-1 = %+v, %v", list, err)
	}
	if all, _ := s.ListAssessments(ctx, tenant, "le-1", ""); len(all) != 3 {
		t.Fatalf("le-1 = %d rows, want 3", len(all))
	}
	if none, _ := s.ListAssessments(ctx, tenant, "", ""); len(none) != 0 {
		t.Fatalf("empty legal entity must match nothing, got %d", len(none))
	}
}

// Every down migration reverses its up. Run in a private schema inside a
// transaction that is rolled back, so the tables other tests are using in
// public are never dropped, not even transiently: up, then down in reverse,
// then up again.
func TestMigrations_DownThenUp(t *testing.T) {
	pools := testdb.Open(t)
	ctx := context.Background()
	tx, err := pools.Admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `CREATE SCHEMA migration_roundtrip; SET LOCAL search_path TO migration_roundtrip`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	ups, downs := testdb.MigrationFiles(t, ".up.sql"), testdb.MigrationFiles(t, ".down.sql")
	if len(ups) != len(downs) {
		t.Fatalf("%d up migrations but %d down", len(ups), len(downs))
	}
	for _, f := range ups {
		execFile(t, tx, f)
	}
	for i := len(downs) - 1; i >= 0; i-- {
		execFile(t, tx, downs[i])
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'migration_roundtrip'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after down: %d tables remain, %v", n, err)
	}
	for _, f := range ups {
		execFile(t, tx, f)
	}
}

func execFile(t *testing.T, tx pgx.Tx, path string) {
	t.Helper()
	sql, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := tx.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("exec %s: %v", path, err)
	}
}
