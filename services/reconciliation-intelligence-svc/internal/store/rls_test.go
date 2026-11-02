//go:build integration

// Row-level security as the application actually connects: a NOSUPERUSER
// NOBYPASSRLS role (TEST_RLS_ROLE, default recon_intel_rls_test). The
// superuser in TEST_DATABASE_URL bypasses RLS, FORCE included, so isolation
// asserted over it would prove nothing.
package store_test

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/eventing/outbox"
	"zoiko.io/reconciliation-intelligence-svc/internal/idempotency"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

var rlsTables = []string{"reconciliation_jobs", "reconciliation_unmatched_items", "eventing_outbox", "idempotency_keys"}

func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	role := os.Getenv("TEST_RLS_ROLE")
	if role == "" {
		role = "recon_intel_rls_test"
	}
	password := role + "_pw"
	for _, stmt := range []string{
		`DO $do$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '` + role + `') THEN
				CREATE ROLE ` + role + ` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
			END IF;
		END $do$;`,
		`ALTER ROLE ` + role + ` WITH LOGIN PASSWORD '` + password + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("role setup (%s): %v", stmt, err)
		}
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var isSuper, bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&isSuper, &bypass); err != nil {
		t.Fatal(err)
	}
	if isSuper || bypass {
		t.Fatalf("%s must be NOSUPERUSER NOBYPASSRLS", role)
	}
	return pool
}

func TestRLS_TenantTablesForcedWithCheck(t *testing.T) {
	admin := openTestPool(t)
	ctx := context.Background()
	for _, table := range rlsTables {
		var enabled, forced bool
		if err := admin.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&enabled, &forced); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if !enabled || !forced {
			t.Errorf("%s: RLS enabled=%v forced=%v, want both", table, enabled, forced)
		}
		var policies, noCheck int
		if err := admin.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE with_check IS NULL) FROM pg_policies WHERE tablename = $1`, table).Scan(&policies, &noCheck); err != nil {
			t.Fatal(err)
		}
		if policies == 0 || noCheck != 0 {
			t.Errorf("%s: %d policies, %d without WITH CHECK", table, policies, noCheck)
		}
	}
}

func TestRLS_AppRoleIsolatesJobsAndSuggestions(t *testing.T) {
	admin := openTestPool(t)
	app := appRolePool(t, admin)
	ctx := context.Background()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()

	s := store.NewPgStore(app, store.WithEventRegion("uk"))
	job, items := newTestJob("le-1")
	if err := s.CreateJob(ctx, tenantA, job, items, "principal-1", "corr-1"); err != nil {
		t.Fatalf("create job as app role: %v", err)
	}
	if _, err := s.GetJobByID(ctx, tenantA, job.ID); err != nil {
		t.Fatalf("own tenant must read its job: %v", err)
	}
	if got, err := s.GetJobByID(ctx, tenantB, job.ID); err == nil && got != nil {
		t.Fatal("another tenant must not read the job")
	}

	count := func(tenant, table string) int {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if tenant != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, table := range []string{"reconciliation_jobs", "reconciliation_unmatched_items"} {
		if count(tenantA, table) == 0 {
			t.Errorf("%s: invisible to its own tenant", table)
		}
		if n := count(tenantB, table); n != 0 {
			t.Errorf("%s: %d rows visible to another tenant", table, n)
		}
		if n := count("", table); n != 0 {
			t.Errorf("%s: %d rows visible without a tenant", table, n)
		}
	}

	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantB); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE reconciliation_jobs SET tenant_id = $1 WHERE id = $2`, tenantB, job.ID); err != nil {
		t.Fatal(err)
	}
	var stillA int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM reconciliation_jobs WHERE id = $1 AND tenant_id = $2`, job.ID, tenantA).Scan(&stillA); err != nil {
		t.Fatal(err)
	}
	if stillA != 1 {
		t.Fatal("tenant B moved tenant A's job")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_jobs (tenant_id, legal_entity_id, job_name, source_system_a, source_system_b) VALUES ($1, 'le', 'x', 'a', 'b')`, tenantA); err == nil {
		t.Fatal("WITH CHECK must refuse a row labelled with another tenant")
	}
}

func TestIdempotencyPgStore_ReplayIsTenantScoped(t *testing.T) {
	admin := openTestPool(t)
	app := appRolePool(t, admin)
	ctx := context.Background()
	idem := idempotency.NewPgStore(app)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	const endpoint, key = "POST /v1/reconciliations/analyze", "key-1"

	rec, err := idem.Claim(ctx, tenantA, endpoint, key, "fp-1")
	if err != nil || rec != nil {
		t.Fatalf("first claim must be granted: rec=%v err=%v", rec, err)
	}
	if err := idem.Complete(ctx, tenantA, endpoint, key, 201, []byte(`{"id":"job-1"}`)); err != nil {
		t.Fatal(err)
	}
	rec, err = idem.Claim(ctx, tenantA, endpoint, key, "fp-1")
	if err != nil || rec == nil || rec.ResponseStatus != 201 || string(rec.ResponseBody) != `{"id":"job-1"}` {
		t.Fatalf("same request must replay the stored answer: rec=%+v err=%v", rec, err)
	}
	if _, err := idem.Claim(ctx, tenantA, endpoint, key, "fp-other"); err != idempotency.ErrFingerprintMismatch {
		t.Fatalf("same key, different request: want ErrFingerprintMismatch, got %v", err)
	}
	rec, err = idem.Claim(ctx, tenantB, endpoint, key, "fp-1")
	if err != nil || rec != nil {
		t.Fatalf("the same key in another tenant is a fresh claim: rec=%v err=%v", rec, err)
	}
}

type capturingWriter struct{ msgs []outbox.Message }

func (w *capturingWriter) WriteMessages(_ context.Context, msgs ...outbox.Message) error {
	w.msgs = append(w.msgs, msgs...)
	return nil
}

func (w *capturingWriter) Probe(context.Context) error { return nil }

// With RLS now FORCEd on eventing_outbox (000009), the shared relay must
// still drain it as the non-superuser app role via its app.outbox_relay
// escape hatch, while a tenant request sees only its own events.
func TestOutboxRLS_RelayDeliversUnderAppRoleTenantsIsolated(t *testing.T) {
	admin := openTestPool(t)
	app := appRolePool(t, admin)
	ctx := context.Background()
	tenantA := uuid.NewString()

	job, items := newTestJob("le-1")
	if err := store.NewPgStore(app, store.WithEventRegion("uk")).CreateJob(ctx, tenantA, job, items, "principal-1", "corr-1"); err != nil {
		t.Fatal(err)
	}

	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	var other int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1`, tenantA).Scan(&other); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if other != 0 {
		t.Fatalf("another tenant sees %d of tenant A's outbox rows", other)
	}

	w := &capturingWriter{}
	relay, err := outbox.NewRelay(app, w, outbox.DefaultConfig(), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		res, err := relay.DrainOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.Claimed == 0 {
			break
		}
	}
	var pending int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1 AND publish_state <> 'published'`, tenantA).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if len(w.msgs) == 0 || pending != 0 {
		t.Fatalf("relay under the app role delivered %d messages, %d still unpublished", len(w.msgs), pending)
	}
}
