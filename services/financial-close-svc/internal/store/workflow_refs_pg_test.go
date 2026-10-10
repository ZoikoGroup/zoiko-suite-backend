package store_test

// Real-Postgres coverage of migration 000015 (close_workflow_refs): append-only
// triggers, CHECK constraints, FORCE RLS tenant isolation, the committed-before-
// return property REF-05's callback depends on, and the down/up round trip.
// Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

func seedPeriodForRefs(t *testing.T, s *store.PgStore, tenant string) *domain.FiscalPeriod {
	t.Helper()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	fp := &domain.FiscalPeriod{
		FiscalPeriodID: uuid.NewString(), TenantID: tenant, LegalEntityID: "le-" + tenant[:8], PeriodName: "2026-07",
		PeriodStart: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), PeriodEnd: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), CloseStatus: "OPEN",
	}
	if _, err := s.CreateFiscalPeriod(ctx, fp); err != nil {
		t.Fatalf("CreateFiscalPeriod: %v", err)
	}
	return fp
}

func newRef(fp *domain.FiscalPeriod, cmd string) *domain.WorkflowRef {
	id, _ := uuid.NewV7()
	return &domain.WorkflowRef{
		RefID: id.String(), LegalEntityID: fp.LegalEntityID, FiscalPeriodID: fp.FiscalPeriodID, PeriodName: fp.PeriodName,
		PeriodKey: "FY2026-P07", Command: cmd, Status: "APPROVED", ControlSnapshotRef: strings.Repeat("a1", 32),
		RequestedBy: "financial-close-svc", Reason: "test",
	}
}

func TestPgStore_WorkflowRefs_RoundTripAndTenantIsolation_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)
	fp := seedPeriodForRefs(t, s, tenantA)

	wr := newRef(fp, "SOFT_CLOSE")
	if err := s.CreateWorkflowRef(ctxA, wr); err != nil {
		t.Fatalf("CreateWorkflowRef: %v", err)
	}
	if wr.TenantID != tenantA || wr.CreatedAt.IsZero() {
		t.Fatalf("tenant/created_at not populated: %+v", wr)
	}

	got, err := s.GetWorkflowRef(ctxA, wr.RefID)
	if err != nil {
		t.Fatalf("GetWorkflowRef: %v", err)
	}
	if got.PeriodKey != "FY2026-P07" || got.Command != "SOFT_CLOSE" || got.Status != "APPROVED" ||
		got.ControlSnapshotRef != wr.ControlSnapshotRef || got.RequestedBy != "financial-close-svc" || got.FiscalPeriodID != fp.FiscalPeriodID {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	// Another tenant, unknown and malformed ids are all ErrWorkflowRefNotFound.
	for name, c := range map[string]struct {
		ctx context.Context
		id  string
	}{
		"other tenant": {ctxB, wr.RefID},
		"unknown":      {ctxA, uuid.NewString()},
		"malformed":    {ctxA, "not-a-uuid"},
	} {
		if _, err := s.GetWorkflowRef(c.ctx, c.id); !errors.Is(err, domain.ErrWorkflowRefNotFound) {
			t.Errorf("%s: expected ErrWorkflowRefNotFound, got %v", name, err)
		}
	}
	if _, err := s.GetWorkflowRef(context.Background(), wr.RefID); !errors.Is(err, domain.ErrIdentityMissing) {
		t.Errorf("no tenant: expected ErrIdentityMissing, got %v", err)
	}
	if err := s.CreateWorkflowRef(context.Background(), newRef(fp, "HARD_CLOSE")); !errors.Is(err, domain.ErrIdentityMissing) {
		t.Errorf("create without tenant: %v", err)
	}

	// Tenant B cannot write a row for tenant A's period under its own scope:
	// the row is stamped with B's tenant, so A's period FK still resolves but
	// reads by A never see it.
	other := newRef(fp, "HARD_CLOSE")
	if err := s.CreateWorkflowRef(ctxB, other); err == nil {
		if _, err := s.GetWorkflowRef(ctxA, other.RefID); !errors.Is(err, domain.ErrWorkflowRefNotFound) {
			t.Error("a row written under tenant B must be invisible to tenant A")
		}
	}

	// FORCE RLS itself (not just the explicit tenant predicate): only provable
	// when the connecting role is subject to RLS.
	var bypass bool
	if err := pool.QueryRow(context.Background(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatal(err)
	}
	if bypass {
		t.Log("connected role is superuser/BYPASSRLS: skipping the raw RLS assertion (explicit tenant predicate asserted above)")
		return
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM close_workflow_refs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("FORCE RLS: a session with no tenant set must see 0 rows, saw %d", n)
	}
}

// REF-05 reads the row over HTTP while the command is in flight, from a
// DIFFERENT connection: CreateWorkflowRef must have committed on return.
func TestPgStore_WorkflowRefs_CommittedOnReturn_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	fp := seedPeriodForRefs(t, s, tenant)
	wr := newRef(fp, "HARD_CLOSE")
	if err := s.CreateWorkflowRef(ctx, wr); err != nil {
		t.Fatal(err)
	}

	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	var cmd string
	if err := tx.QueryRow(context.Background(), `SELECT command FROM close_workflow_refs WHERE ref_id = $1`, wr.RefID).Scan(&cmd); err != nil || cmd != "HARD_CLOSE" {
		t.Fatalf("row not visible from another connection right after return: %v %q", err, cmd)
	}
}

func TestPgStore_WorkflowRefs_AppendOnlyAndChecks_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	fp := seedPeriodForRefs(t, s, tenant)
	wr := newRef(fp, "RECLOSE")
	if err := s.CreateWorkflowRef(ctx, wr); err != nil {
		t.Fatal(err)
	}

	// Append-only: UPDATE / DELETE / TRUNCATE are rejected by trigger. Each runs
	// inside a tenant-scoped tx so RLS does not mask the trigger.
	for _, stmt := range []string{
		`UPDATE close_workflow_refs SET status = 'REJECTED'`,
		`DELETE FROM close_workflow_refs`,
		`TRUNCATE close_workflow_refs`,
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
		_, err = tx.Exec(ctx, stmt)
		_ = tx.Rollback(ctx)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q must be rejected by the append-only trigger, got %v", stmt, err)
		}
	}
	if got, err := s.GetWorkflowRef(ctx, wr.RefID); err != nil || got.Status != "APPROVED" {
		t.Fatalf("row must be intact: %v %+v", err, got)
	}

	// CHECK constraints.
	for name, mut := range map[string]func(*domain.WorkflowRef){
		"bad command":    func(w *domain.WorkflowRef) { w.Command = "REOPEN" },
		"bad status":     func(w *domain.WorkflowRef) { w.Status = "PENDING" },
		"short hash":     func(w *domain.WorkflowRef) { w.ControlSnapshotRef = "abc" },
		"upper hash":     func(w *domain.WorkflowRef) { w.ControlSnapshotRef = strings.Repeat("AB", 32) },
		"empty hash":     func(w *domain.WorkflowRef) { w.ControlSnapshotRef = "" },
		"unknown period": func(w *domain.WorkflowRef) { w.FiscalPeriodID = uuid.NewString() },
	} {
		bad := newRef(fp, "SOFT_CLOSE")
		mut(bad)
		if err := s.CreateWorkflowRef(ctx, bad); err == nil {
			t.Errorf("%s: expected the constraint to reject the row", name)
		}
	}
	// Both statuses and all four commands are accepted.
	for _, cmd := range []string{"SOFT_CLOSE", "HARD_CLOSE", "AUTHORIZE_REOPEN", "RECLOSE"} {
		if err := s.CreateWorkflowRef(ctx, newRef(fp, cmd)); err != nil {
			t.Errorf("%s rejected: %v", cmd, err)
		}
	}
	rej := newRef(fp, "SOFT_CLOSE")
	rej.Status = "REJECTED"
	if err := s.CreateWorkflowRef(ctx, rej); err != nil {
		t.Errorf("REJECTED status must be storable: %v", err)
	}
	// Duplicate ref id.
	if err := s.CreateWorkflowRef(ctx, wr); err == nil {
		t.Error("duplicate ref_id must be rejected")
	}
}

func TestPgStore_WorkflowRefs_MigrationDownUp_RealDB(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	exists := func(pool *pgxpool.Pool) (table, fn bool) {
		_ = pool.QueryRow(ctx, `SELECT to_regclass('public.close_workflow_refs') IS NOT NULL`).Scan(&table)
		_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'reject_close_workflow_ref_mutation')`).Scan(&fn)
		return
	}
	if tb, fn := exists(pool); !tb || !fn {
		t.Fatalf("after up: table=%v function=%v", tb, fn)
	}
	if _, err := pool.Exec(ctx, read("000015_close_workflow_refs.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if tb, fn := exists(pool); tb || fn {
		t.Fatalf("after down: table=%v function=%v (must both be gone)", tb, fn)
	}
	// Down is idempotent.
	if _, err := pool.Exec(ctx, read("000015_close_workflow_refs.down.sql")); err != nil {
		t.Fatalf("second down: %v", err)
	}
	if _, err := pool.Exec(ctx, read("000015_close_workflow_refs.up.sql")); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if tb, fn := exists(pool); !tb || !fn {
		t.Fatalf("after re-up: table=%v function=%v", tb, fn)
	}
}
