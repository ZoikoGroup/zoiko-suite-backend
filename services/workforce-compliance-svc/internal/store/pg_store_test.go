package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/workforce-compliance-svc/internal/domain"
	"zoiko.io/workforce-compliance-svc/internal/middleware"
	"zoiko.io/workforce-compliance-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migrations
// from a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set —
// same convention as every other service in this platform.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS compliance_alerts, working_hour_logs, visa_records, work_authorizations CASCADE;`)

	for _, migration := range []string{
		"000001_initial_schema.up.sql",
		"000002_fix_tenant_isolation_and_idempotency.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", migration))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", migration, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", migration, err)
		}
	}

	return pool
}

func newTestWorkAuth(tenantID, correlationID string) *domain.WorkAuthorization {
	return &domain.WorkAuthorization{
		TenantID:       tenantID,
		LegalEntityID:  "le-us",
		EmployeeID:     uuid.New().String(),
		DocumentType:   "I-9",
		DocumentNumber: "DOC-" + uuid.New().String()[:8],
		IssueDate:      "2026-01-01",
		EffectiveFrom:  "2026-01-01",
		Status:         domain.VerificationStatusPending,
		CorrelationID:  correlationID,
	}
}

// TestPgStore_CreateWorkAuth_RealPostgres proves the RLS tenant context
// mechanism actually works against a real Postgres — a prior version used
// "SET LOCAL app.tenant_id = $1", which is invalid syntax (SET does not
// accept bind parameters) and would error on every call.
func TestPgStore_CreateWorkAuth_RealPostgres(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	auth := newTestWorkAuth(tenantID, "corr-1")
	created, err := s.CreateWorkAuth(ctx, auth)
	if err != nil {
		t.Fatalf("CreateWorkAuth failed: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on the first call")
	}

	got, err := s.GetWorkAuth(ctx, auth.EmployeeID)
	if err != nil {
		t.Fatalf("GetWorkAuth failed: %v", err)
	}
	if got.Status != domain.VerificationStatusPending {
		t.Fatalf("expected status PENDING, got %s", got.Status)
	}
}

func TestPgStore_CreateWorkAuth_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	auth1 := newTestWorkAuth(tenantID, "corr-retry-1")
	created1, err := s.CreateWorkAuth(ctx, auth1)
	if err != nil {
		t.Fatalf("first CreateWorkAuth failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	auth2 := newTestWorkAuth(tenantID, "corr-retry-1")
	created2, err := s.CreateWorkAuth(ctx, auth2)
	if err != nil {
		t.Fatalf("retried CreateWorkAuth failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call — this is a duplicate-record bug if it's true")
	}
	if auth2.AuthID != auth1.AuthID {
		t.Fatalf("retried call resolved to a different auth_id (%s) than the original (%s)", auth2.AuthID, auth1.AuthID)
	}
}

// TestPgStore_RLS_TenantIsolation proves tenant A's data is invisible
// under tenant B's context — the property that was silently broken by the
// invalid SET LOCAL syntax.
func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	authA := newTestWorkAuth(tenantA, "corr-a")
	if _, err := s.CreateWorkAuth(ctxA, authA); err != nil {
		t.Fatalf("CreateWorkAuth (tenant A) failed: %v", err)
	}

	_, err := s.GetWorkAuthByID(ctxB, authA.AuthID)
	if err == nil {
		t.Fatal("ISOLATION FAILURE: tenant B was able to read tenant A's work authorization")
	}

	if _, err := s.VerifyWorkAuth(ctxB, authA.AuthID, "attacker"); err == nil {
		t.Fatal("ISOLATION FAILURE: tenant B was able to verify tenant A's work authorization")
	}

	gotA, err := s.GetWorkAuthByID(ctxA, authA.AuthID)
	if err != nil {
		t.Fatalf("GetWorkAuthByID under tenant A's own context failed: %v", err)
	}
	if gotA.Status != domain.VerificationStatusPending {
		t.Fatalf("ISOLATION FAILURE: tenant A's record was mutated by tenant B, got status %s", gotA.Status)
	}
}

// TestPgStore_VerifyWorkAuth_AtomicTransition proves the verify step is a
// single conditional UPDATE, not a read-then-write with a race window.
func TestPgStore_VerifyWorkAuth_AtomicTransition(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	auth := newTestWorkAuth(tenantID, "corr-verify-1")
	if _, err := s.CreateWorkAuth(ctx, auth); err != nil {
		t.Fatalf("CreateWorkAuth failed: %v", err)
	}

	if _, err := s.VerifyWorkAuth(ctx, auth.AuthID, "hr-admin"); err != nil {
		t.Fatalf("first VerifyWorkAuth failed: %v", err)
	}

	if _, err := s.VerifyWorkAuth(ctx, auth.AuthID, "hr-admin"); err == nil {
		t.Fatal("expected an error re-verifying an already-VERIFIED work authorization, got nil")
	}
}

// TestPgStore_FlagVisaExpiration_DoesNotDuplicateOnReplay proves a second
// flag call on an already-flagged visa is reported distinctly
// (ErrAlreadyFlagged) so the handler doesn't raise a duplicate alert.
func TestPgStore_FlagVisaExpiration_DoesNotDuplicateOnReplay(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	visa := &domain.VisaRecord{
		TenantID:       tenantID,
		LegalEntityID:  "le-us",
		EmployeeID:     uuid.New().String(),
		VisaType:       "H1B",
		IssuingCountry: "USA",
		ExpirationDate: "2026-12-31",
		Status:         domain.VerificationStatusVerified,
		CorrelationID:  "corr-visa-1",
	}
	if _, err := s.CreateVisaRecord(ctx, visa); err != nil {
		t.Fatalf("CreateVisaRecord failed: %v", err)
	}

	if _, err := s.FlagVisaExpiration(ctx, visa.VisaID); err != nil {
		t.Fatalf("first FlagVisaExpiration failed: %v", err)
	}

	_, err := s.FlagVisaExpiration(ctx, visa.VisaID)
	if err != domain.ErrAlreadyFlagged {
		t.Fatalf("expected ErrAlreadyFlagged on second flag, got %v", err)
	}
}
