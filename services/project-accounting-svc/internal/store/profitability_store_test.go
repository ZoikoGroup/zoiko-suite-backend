package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/project-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/project-accounting-svc/internal/middleware"
	"zoiko.io/project-accounting-svc/internal/store"
)

// TestPgStore_RefreshProfitabilityProjection_NoCostsNoRuns_ZeroValues
// proves a freshly-activated project with no cost entries and no
// recognition runs yet refreshes to real, valid zero figures — not an
// error.
func TestPgStore_RefreshProfitabilityProjection_NoCostsNoRuns_ZeroValues(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-PROF-1")

	proj, err := s.RefreshProfitabilityProjection(ctx, projectID, "preparer-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("RefreshProfitabilityProjection failed: %v", err)
	}
	if proj.Status != domain.ProfitabilityProjectionStatusCurrent {
		t.Fatalf("expected CURRENT, got %q", proj.Status)
	}
	if proj.Revenue != 0 || proj.Cost != 0 || proj.Margin != 0 {
		t.Fatalf("expected all-zero figures, got revenue=%v cost=%v margin=%v", proj.Revenue, proj.Cost, proj.Margin)
	}
	if proj.RevenueRunID != nil {
		t.Fatalf("expected no revenue run yet, got %v", proj.RevenueRunID)
	}
}

// TestPgStore_RefreshProfitabilityProjection_WithCostsAndRevenue proves
// the projection is a real, live read over PRJ-02's cost entries and
// PRJ-03's own calculated recognition run — never a caller-declared
// figure.
func TestPgStore_RefreshProfitabilityProjection_WithCostsAndRevenue(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newRecognitionReadyProject(t, s, ctx, tenantID, legalEntityID, "PRJ-PROF-2", 300)

	entry := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "cost-prof-1", 700)
	if err := s.CaptureProjectCost(ctx, entry); err != nil {
		t.Fatalf("CaptureProjectCost failed: %v", err)
	}

	contractValue := 1000.0
	run := &domain.RecognitionRun{
		RunID: uuid.New().String(), LegalEntityID: legalEntityID, ProjectID: projectID, FiscalPeriod: "2026-09",
		Status: domain.RecognitionRunStatusDraft, ContractValue: &contractValue,
		RevenueAccountCode: "4000", WIPAccountCode: "1300",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
	if err := s.CreateRecognitionRun(ctx, run); err != nil {
		t.Fatalf("CreateRecognitionRun failed: %v", err)
	}
	if err := s.FreezeAndCalculate(ctx, run.RunID, time.Now().UTC()); err != nil {
		t.Fatalf("FreezeAndCalculate failed: %v", err)
	}

	proj, err := s.RefreshProfitabilityProjection(ctx, projectID, "preparer-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("RefreshProfitabilityProjection failed: %v", err)
	}
	if proj.Cost != 700 {
		t.Fatalf("expected cost 700 (real ITD sum), got %v", proj.Cost)
	}
	if proj.Revenue != 700 {
		t.Fatalf("expected revenue 700 (0.7 percent_complete * 1000 contract_value), got %v", proj.Revenue)
	}
	if proj.Margin != 0 {
		t.Fatalf("expected margin 0 (700 revenue - 700 cost), got %v", proj.Margin)
	}
	if proj.RevenueRunID == nil || *proj.RevenueRunID != run.RunID {
		t.Fatalf("expected revenue_run_id to point at the real calculated run, got %v", proj.RevenueRunID)
	}
}

// TestPgStore_GetProjectProfitability_NoProjection_Refused proves reading
// a project with no projection built yet is a real, named error, not a
// zero-value fabrication.
func TestPgStore_GetProjectProfitability_NoProjection_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-PROF-3")

	if _, err := s.GetProjectProfitability(ctx, projectID); err != domain.ErrProjectionNotBuilt {
		t.Fatalf("expected ErrProjectionNotBuilt, got %v", err)
	}
}

// TestPgStore_BuildProfitabilitySnapshot_StaleProjection_Refused is the
// real proof of negative path #1, "Stale project margin shown as
// certified," enforced at BUILD time: a new cost entry landing after the
// last refresh marks the projection STALE on the very next freshness
// check, and BuildProfitabilitySnapshot refuses to build from it.
func TestPgStore_BuildProfitabilitySnapshot_StaleProjection_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-PROF-4")

	if _, err := s.RefreshProfitabilityProjection(ctx, projectID, "preparer-1", time.Now().UTC()); err != nil {
		t.Fatalf("RefreshProfitabilityProjection failed: %v", err)
	}

	// A new cost fact lands AFTER the projection was refreshed.
	time.Sleep(10 * time.Millisecond)
	entry := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "cost-prof-stale-1", 200)
	if err := s.CaptureProjectCost(ctx, entry); err != nil {
		t.Fatalf("CaptureProjectCost failed: %v", err)
	}

	if _, err := s.BuildProfitabilitySnapshot(ctx, projectID, "preparer-1", time.Now().UTC()); err != domain.ErrProjectionStale {
		t.Fatalf("expected ErrProjectionStale, got %v", err)
	}
}

// TestPgStore_CertifyProfitabilitySnapshot_StaleAtCertifyTime_Refused
// proves the SAME negative path is re-checked at CERTIFY time against
// live data, not just relied upon from the build-time check — a snapshot
// built while fresh can still go stale before it is certified.
func TestPgStore_CertifyProfitabilitySnapshot_StaleAtCertifyTime_Refused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-PROF-5")

	if _, err := s.RefreshProfitabilityProjection(ctx, projectID, "preparer-1", time.Now().UTC()); err != nil {
		t.Fatalf("RefreshProfitabilityProjection failed: %v", err)
	}
	snap, err := s.BuildProfitabilitySnapshot(ctx, projectID, "preparer-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("BuildProfitabilitySnapshot failed: %v", err)
	}

	// A new cost fact lands AFTER the snapshot was built (the projection
	// itself is refreshed separately in a real deployment; certify must
	// catch this even though nobody re-ran RefreshProfitabilityProjection).
	time.Sleep(10 * time.Millisecond)
	entry := newDraftCostEntry(projectID, domain.CostSourceTypeAP, "cost-prof-stale-2", 50)
	if err := s.CaptureProjectCost(ctx, entry); err != nil {
		t.Fatalf("CaptureProjectCost failed: %v", err)
	}

	if _, err := s.CertifyProfitabilitySnapshot(ctx, snap.SnapshotID, "certifier-1", time.Now().UTC()); err != domain.ErrSnapshotStaleAtCertification {
		t.Fatalf("expected ErrSnapshotStaleAtCertification, got %v", err)
	}
}

// TestPgStore_ProfitabilitySnapshot_FieldsImmutable is the real proof of
// negative path #2, "Manual edit changes margin without source fact" —
// the reject-mutation trigger.
func TestPgStore_ProfitabilitySnapshot_FieldsImmutable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-PROF-6")

	if _, err := s.RefreshProfitabilityProjection(ctx, projectID, "preparer-1", time.Now().UTC()); err != nil {
		t.Fatalf("RefreshProfitabilityProjection failed: %v", err)
	}
	snap, err := s.BuildProfitabilitySnapshot(ctx, projectID, "preparer-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("BuildProfitabilitySnapshot failed: %v", err)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE project_profitability_snapshots SET margin = 999999 WHERE snapshot_id = $1`, snap.SnapshotID); err == nil {
		t.Fatalf("expected the reject-mutation trigger to refuse mutating margin, got no error")
	}
}

// TestPgStore_ProfitabilitySnapshot_FullLifecycle exercises
// Refresh -> Build (RECONCILED) -> Certify (CERTIFIED) end to end
// against real Postgres.
func TestPgStore_ProfitabilitySnapshot_FullLifecycle(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	projectID := newActiveTestProject(t, s, ctx, tenantID, legalEntityID, "PRJ-PROF-7")

	if _, err := s.RefreshProfitabilityProjection(ctx, projectID, "preparer-1", time.Now().UTC()); err != nil {
		t.Fatalf("RefreshProfitabilityProjection failed: %v", err)
	}
	snap, err := s.BuildProfitabilitySnapshot(ctx, projectID, "preparer-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("BuildProfitabilitySnapshot failed: %v", err)
	}
	if snap.Status != domain.ProfitabilitySnapshotStatusReconciled {
		t.Fatalf("expected RECONCILED, got %q", snap.Status)
	}

	certified, err := s.CertifyProfitabilitySnapshot(ctx, snap.SnapshotID, "certifier-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("CertifyProfitabilitySnapshot failed: %v", err)
	}
	if certified.Status != domain.ProfitabilitySnapshotStatusCertified {
		t.Fatalf("expected CERTIFIED, got %q", certified.Status)
	}

	got, err := s.GetProfitabilitySnapshot(ctx, snap.SnapshotID)
	if err != nil {
		t.Fatalf("GetProfitabilitySnapshot failed: %v", err)
	}
	if got.Status != domain.ProfitabilitySnapshotStatusCertified {
		t.Fatalf("expected persisted CERTIFIED, got %q", got.Status)
	}

	if _, err := s.CertifyProfitabilitySnapshot(ctx, snap.SnapshotID, "certifier-1", time.Now().UTC()); err != domain.ErrInvalidSnapshotTransition {
		t.Fatalf("expected ErrInvalidSnapshotTransition on re-certify, got %v", err)
	}
}
