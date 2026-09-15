package store_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/store"
)

func mustParseDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}

func newAuditPopulationForTest(t *testing.T, s *store.PgStore, ctx context.Context, engagementID, correlationID string) *domain.AuditPopulation {
	t.Helper()
	p, _, err := s.DefinePopulation(ctx, domain.DefinePopulationParams{
		EngagementID: engagementID, TenantID: testTenantID,
		SourceSystem: "general-ledger-svc", SourceObject: "journal_entries", FilterSpec: "fiscal_year=2026",
		Watermark: "2026-12-31T23:59:59Z", PeriodStart: mustParseDate(t, "2026-01-01"), PeriodEnd: mustParseDate(t, "2026-12-31"),
		CreatedByPrincipalID: "auditor-1", CorrelationID: correlationID,
	})
	if err != nil {
		t.Fatalf("define population: %v", err)
	}
	return p
}

// TestPgStore_AuditPopulation_FreezeRequiresValidatedReconciliation is the
// real proof of AUD-NEG-008 "count matches source but control total
// differs -> quarantine, not a silent freeze": ValidatePopulation's own
// mismatch check quarantines the population, and FreezePopulation's CAS
// precondition (only VALIDATING may freeze) then refuses a quarantined one.
func TestPgStore_AuditPopulation_FreezeRequiresValidatedReconciliation(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-pop-1")
	pop := newAuditPopulationForTest(t, s, ctx, eng.EngagementID, "pop-define-1")

	built, _, err := s.BuildPopulation(ctx, domain.BuildPopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-build-1",
		ItemCount: 1000, ControlTotalAmount: 500000.00,
	})
	if err != nil || built.Status != domain.AuditPopulationBuilding {
		t.Fatalf("build population: %+v err=%v", built, err)
	}

	// Reconciliation mismatch: expected control total differs from the
	// built extract's own total -> quarantine, not a freeze.
	quarantined, changed, err := s.ValidatePopulation(ctx, domain.ValidatePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-validate-mismatch-1",
		ExpectedItemCount: 1000, ExpectedControlTotalAmount: 499999.00,
	})
	if err != nil || !changed {
		t.Fatalf("validate population (mismatch): changed=%v err=%v", changed, err)
	}
	if quarantined.Status != domain.AuditPopulationQuarantined {
		t.Fatalf("expected QUARANTINED after a reconciliation mismatch, got %q", quarantined.Status)
	}
	if quarantined.QuarantineReason == nil || *quarantined.QuarantineReason == "" {
		t.Fatal("expected a quarantine reason to be recorded")
	}

	// The quarantined population cannot be frozen — FreezePopulation's own
	// CAS precondition only accepts VALIDATING.
	if _, _, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-freeze-blocked-1",
	}); err != domain.ErrAuditPopulationInvalidState {
		t.Fatalf("expected ErrAuditPopulationInvalidState freezing a quarantined population, got %v", err)
	}

	// A second, freshly built+validated population (matching totals) can
	// freeze normally — proves the mechanism blocks the bad path
	// specifically, not freezing in general.
	pop2 := newAuditPopulationForTest(t, s, ctx, eng.EngagementID, "pop-define-2")
	if _, _, err := s.BuildPopulation(ctx, domain.BuildPopulationParams{
		PopulationID: pop2.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-build-2",
		ItemCount: 1000, ControlTotalAmount: 500000.00,
	}); err != nil {
		t.Fatalf("build population 2: %v", err)
	}
	validated, _, err := s.ValidatePopulation(ctx, domain.ValidatePopulationParams{
		PopulationID: pop2.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-validate-2",
		ExpectedItemCount: 1000, ExpectedControlTotalAmount: 500000.00,
	})
	if err != nil || validated.Status != domain.AuditPopulationValidating {
		t.Fatalf("validate population 2: %+v err=%v", validated, err)
	}
	frozen, _, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{
		PopulationID: pop2.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-freeze-2",
	})
	if err != nil || frozen.Status != domain.AuditPopulationFrozen {
		t.Fatalf("freeze population 2: %+v err=%v", frozen, err)
	}
	if frozen.Digest == nil || *frozen.Digest == "" {
		t.Fatal("expected a real digest on the frozen population")
	}
}

// TestPgStore_AuditPopulation_FrozenManifestIsImmutable is the real,
// negative-controlled proof of AUD-CTRL-008: once frozen, a population's
// manifest-defining fields cannot be mutated by any path other than the
// store's own controlled AddControlledDelta/SupersedePopulation — a raw
// UPDATE attempting to change the digest or item_count is refused by the
// migration's own trigger, disabled and re-enabled here to prove the
// trigger itself is the real mechanism.
func TestPgStore_AuditPopulation_FrozenManifestIsImmutable(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-pop-immutable-1")
	pop := newAuditPopulationForTest(t, s, ctx, eng.EngagementID, "pop-define-immutable-1")
	if _, _, err := s.BuildPopulation(ctx, domain.BuildPopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-build-immutable-1",
		ItemCount: 500, ControlTotalAmount: 250000.00,
	}); err != nil {
		t.Fatalf("build population: %v", err)
	}
	if _, _, err := s.ValidatePopulation(ctx, domain.ValidatePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-validate-immutable-1",
		ExpectedItemCount: 500, ExpectedControlTotalAmount: 250000.00,
	}); err != nil {
		t.Fatalf("validate population: %v", err)
	}
	frozen, _, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-freeze-immutable-1",
	})
	if err != nil {
		t.Fatalf("freeze population: %v", err)
	}

	// A pure status transition IS permitted (e.g. FROZEN -> SUPERSEDED via
	// the store's own SupersedePopulation) — confirmed separately below.
	// Here: a raw content mutation must be refused.
	if _, err := pool.Exec(ctx, `UPDATE audit_populations SET item_count = item_count + 1 WHERE population_id = $1`, frozen.PopulationID); err == nil {
		t.Fatal("expected the frozen-manifest-immutability trigger to refuse mutating item_count")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit_populations WHERE population_id = $1`, frozen.PopulationID); err == nil {
		t.Fatal("expected audit populations to never be deletable")
	}

	// Negative control: disable the trigger, confirm the same UPDATE now
	// succeeds (proving the trigger — not something else — was refusing
	// it), then re-enable and confirm refusal returns.
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_populations DISABLE TRIGGER trg_reject_frozen_population_manifest_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_populations SET item_count = item_count + 1 WHERE population_id = $1`, frozen.PopulationID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_populations ENABLE TRIGGER trg_reject_frozen_population_manifest_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_populations SET item_count = item_count + 1 WHERE population_id = $1`, frozen.PopulationID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}

	// The store's OWN legitimate status transition (FROZEN -> SUPERSEDED)
	// must still succeed despite the trigger — proves the trigger locks
	// manifest content only, not the status column itself.
	superseded, changed, err := s.SupersedePopulation(ctx, domain.SupersedePopulationParams{
		PopulationID: frozen.PopulationID, TenantID: testTenantID, ActorPrincipalID: "manager-1", CorrelationID: "pop-supersede-1", Reason: "framework scope changed",
	})
	if err != nil || !changed {
		t.Fatalf("supersede frozen population: changed=%v err=%v", changed, err)
	}
	if superseded.Status != domain.AuditPopulationSuperseded {
		t.Fatalf("expected SUPERSEDED, got %q", superseded.Status)
	}
}

// TestPgStore_AuditPopulation_AddControlledDelta_NeverMutatesOriginal is
// the real proof of AUD-NEG-010: a late item discovered after freeze
// creates a brand-new successor population rather than mutating the
// frozen original, and the original is marked SUPERSEDED with the linkage
// recorded both ways.
func TestPgStore_AuditPopulation_AddControlledDelta_NeverMutatesOriginal(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)
	eng := newAuditEngagementForTest(t, s, ctx, "eng-pop-delta-1")
	pop := newAuditPopulationForTest(t, s, ctx, eng.EngagementID, "pop-define-delta-1")
	if _, _, err := s.BuildPopulation(ctx, domain.BuildPopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-build-delta-1",
		ItemCount: 200, ControlTotalAmount: 100000.00,
	}); err != nil {
		t.Fatalf("build population: %v", err)
	}
	if _, _, err := s.ValidatePopulation(ctx, domain.ValidatePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-validate-delta-1",
		ExpectedItemCount: 200, ExpectedControlTotalAmount: 100000.00,
	}); err != nil {
		t.Fatalf("validate population: %v", err)
	}
	frozen, _, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-freeze-delta-1",
	})
	if err != nil {
		t.Fatalf("freeze population: %v", err)
	}
	originalDigest := *frozen.Digest
	originalItemCount := *frozen.ItemCount

	successor, created, err := s.AddControlledDelta(ctx, domain.AddControlledDeltaParams{
		PopulationID: frozen.PopulationID, TenantID: testTenantID, ActorPrincipalID: "auditor-1", CorrelationID: "pop-delta-1",
		Reason: "late journal entry discovered post-freeze", DeltaItemCount: 3, DeltaControlTotalAmount: 1500.00,
	})
	if err != nil || !created {
		t.Fatalf("add controlled delta: created=%v err=%v", created, err)
	}
	if successor.PopulationID == frozen.PopulationID {
		t.Fatal("expected a NEW population id for the successor, not the original")
	}
	if successor.PriorPopulationID == nil || *successor.PriorPopulationID != frozen.PopulationID {
		t.Fatal("expected the successor to link back to the original via prior_population_id")
	}
	if *successor.ItemCount != originalItemCount+3 {
		t.Fatalf("expected successor item_count = original + delta = %d, got %d", originalItemCount+3, *successor.ItemCount)
	}

	// The ORIGINAL row must be unchanged in content, only superseded in status.
	original, err := s.GetAuditPopulation(ctx, testTenantID, frozen.PopulationID)
	if err != nil {
		t.Fatalf("get original population: %v", err)
	}
	if original.Status != domain.AuditPopulationSuperseded {
		t.Fatalf("expected original marked SUPERSEDED, got %q", original.Status)
	}
	if *original.Digest != originalDigest {
		t.Fatal("original population's digest must never change — AddControlledDelta must not mutate it")
	}
	if original.SupersededByPopulationID == nil || *original.SupersededByPopulationID != successor.PopulationID {
		t.Fatal("expected the original to record superseded_by_population_id pointing at the successor")
	}
}
