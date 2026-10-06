package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/obligation-tracking-svc/internal/domain"
	"zoiko.io/obligation-tracking-svc/internal/middleware"
	"zoiko.io/obligation-tracking-svc/internal/store"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	requireThrowawayDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS obligations CASCADE;`)

	for _, name := range []string{
		"000001_initial_schema.up.sql",
		"000002_validated_lifecycle_and_tenant_isolation.up.sql",
	} {
		sql, err := os.ReadFile(filepath.Join(base, "../../deployments/migrations", name))
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", name, err)
		}
	}

	return pool
}

func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs obligations.", dbName)
	}
}

func tenantCtx(tenantID string) context.Context {
	return middleware.WithTenant(context.Background(), tenantID)
}

func newObligation(legalEntityID, createdBy string) *domain.Obligation {
	return &domain.Obligation{
		LegalEntityID:  legalEntityID,
		SourceType:     "CONTRACT",
		SourceID:       "ctr-123",
		Title:          "Deliver Q1 SLA report",
		ObligationType: domain.ObligationTypeContractual,
		RiskLevel:      domain.RiskLevelMedium,
		DueDate:        "2026-04-15",
		EffectiveFrom:  "2026-01-01",
		CreatedBy:      createdBy,
	}
}

func TestPgStore_ManualObligation_StartsAtPlanned(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	o := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if o.Status != domain.ObligationStatusPlanned {
		t.Fatalf("status = %s, want PLANNED", o.Status)
	}
}

// AI extraction cannot activate an obligation without validation (LEG-07
// §9.1).
func TestPgStore_ExtractedObligation_StartsAtCandidateAndRequiresValidation(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	o := newObligation("le-us", "ai-pipeline")
	o.ExtractedByAI = true
	if err := s.CreateObligation(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if o.Status != domain.ObligationStatusCandidate {
		t.Fatalf("status = %s, want CANDIDATE", o.Status)
	}

	if _, err := s.Schedule(ctx, o.ObligationID, &domain.ScheduleObligationRequest{
		TriggerDescription: "x", CalculationMethod: "y", ScheduledBy: "legal-ops-1",
	}); !errors.Is(err, domain.ErrWrongStatus) {
		t.Fatalf("scheduling an unvalidated candidate returned %v, want ErrWrongStatus", err)
	}

	validated, err := s.ValidateExtractedObligation(ctx, o.ObligationID, "legal-ops-1")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if validated.Status != domain.ObligationStatusPlanned {
		t.Fatalf("status after validate = %s, want PLANNED", validated.Status)
	}
}

// Schedule refuses an ambiguous due-date basis (LEG-07 §9).
func TestPgStore_Schedule_RefusesAmbiguousDueDateBasis(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	o := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Schedule(ctx, o.ObligationID, &domain.ScheduleObligationRequest{ScheduledBy: "legal-ops-1"}); !errors.Is(err, domain.ErrAmbiguousDueDateBasis) {
		t.Fatalf("schedule with no trigger/calculation returned %v, want ErrAmbiguousDueDateBasis", err)
	}
}

func TestPgStore_FullLifecycle_ReachesSatisfied(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	o := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	active, err := s.Schedule(ctx, o.ObligationID, &domain.ScheduleObligationRequest{
		TriggerDescription: "Contract signature + 90 days", CalculationMethod: "fixed offset", ScheduledBy: "legal-ops-1",
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if active.Status != domain.ObligationStatusActive {
		t.Fatalf("status after schedule = %s, want ACTIVE", active.Status)
	}

	due, err := s.MarkDue(ctx, o.ObligationID, "scheduler")
	if err != nil {
		t.Fatalf("mark due: %v", err)
	}
	if due.Status != domain.ObligationStatusDue {
		t.Fatalf("status after mark due = %s, want DUE", due.Status)
	}

	inProgress, err := s.MarkInProgress(ctx, o.ObligationID, "obligation-owner-1")
	if err != nil {
		t.Fatalf("mark in progress: %v", err)
	}
	if inProgress.Status != domain.ObligationStatusInProgress {
		t.Fatalf("status after mark in progress = %s, want IN_PROGRESS", inProgress.Status)
	}

	satisfied, err := s.Complete(ctx, o.ObligationID, "obligation-owner-1")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if satisfied.Status != domain.ObligationStatusSatisfied {
		t.Fatalf("status after complete = %s, want SATISFIED", satisfied.Status)
	}
}

// Waiver is distinct from satisfaction and requires documented authority
// (LEG-07 §9.1).
func TestPgStore_Waive_RequiresAuthorityReference(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	o := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Schedule(ctx, o.ObligationID, &domain.ScheduleObligationRequest{
		TriggerDescription: "x", CalculationMethod: "y", ScheduledBy: "legal-ops-1",
	}); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	if _, err := s.Waive(ctx, o.ObligationID, &domain.WaiveObligationRequest{WaivedBy: "legal-ops-1"}); !errors.Is(err, domain.ErrWaiverAuthorityRequired) {
		t.Fatalf("waive with no authority reference returned %v, want ErrWaiverAuthorityRequired", err)
	}

	waived, err := s.Waive(ctx, o.ObligationID, &domain.WaiveObligationRequest{WaivedBy: "legal-ops-1", WaiverAuthorityReference: "delegation-doc-42"})
	if err != nil {
		t.Fatalf("waive with authority reference: %v", err)
	}
	if waived.Status != domain.ObligationStatusWaived {
		t.Fatalf("status = %s, want WAIVED", waived.Status)
	}
}

// Breach records the fact only — no automatic financial accrual, payment or
// legal remedy (LEG-07 §9.1). This test proves RecordBreach doesn't touch
// anything beyond the obligation row itself (no outbound calls exist in
// the implementation to assert against, so the assertion is simply that
// the obligation reaches BREACHED with its own attribution and nothing more).
func TestPgStore_RecordBreach_RecordsFactOnly(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	o := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Schedule(ctx, o.ObligationID, &domain.ScheduleObligationRequest{
		TriggerDescription: "x", CalculationMethod: "y", ScheduledBy: "legal-ops-1",
	}); err != nil {
		t.Fatalf("schedule: %v", err)
	}

	breached, err := s.RecordBreach(ctx, o.ObligationID, &domain.RecordBreachRequest{BreachedBy: "obligation-owner-1", BreachNote: "missed deliverable"})
	if err != nil {
		t.Fatalf("record breach: %v", err)
	}
	if breached.Status != domain.ObligationStatusBreached {
		t.Fatalf("status = %s, want BREACHED", breached.Status)
	}
	if breached.BreachedBy == nil || *breached.BreachedBy != "obligation-owner-1" {
		t.Fatalf("breached_by = %v, want obligation-owner-1", breached.BreachedBy)
	}
}

// A write must not be able to conclude another tenant's obligation, and the
// policy must actually apply — FORCE ROW LEVEL SECURITY plus this store's
// own explicit tenant_id predicates (migration 000002) are what make that
// true for this superuser connection.
func TestPgStore_Transitions_AreTenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)

	o := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(tenantCtx("tenant-a"), o); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.GetObligation(tenantCtx("tenant-b"), o.ObligationID); !errors.Is(err, domain.ErrObligationNotFound) {
		t.Fatalf("cross-tenant read returned %v, want ErrObligationNotFound", err)
	}
	if _, err := s.Schedule(tenantCtx("tenant-b"), o.ObligationID, &domain.ScheduleObligationRequest{
		TriggerDescription: "x", CalculationMethod: "y", ScheduledBy: "legal-ops-1",
	}); !errors.Is(err, domain.ErrObligationNotFound) {
		t.Fatalf("cross-tenant schedule returned %v, want ErrObligationNotFound", err)
	}
}

func TestPgStore_WithoutTenant_IsRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := context.Background()

	if err := s.CreateObligation(ctx, newObligation("le-us", "drafter-1")); !errors.Is(err, domain.ErrTenantMissing) {
		t.Fatalf("create without tenant returned %v, want ErrTenantMissing", err)
	}
}
