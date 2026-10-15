package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

func TestCloseEvidence_RelianceManifestRoundTripsExactly(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	fp := &domain.FiscalPeriod{FiscalPeriodID: uuid.NewString(), TenantID: tenantID, LegalEntityID: "le-1",
		PeriodName: "2026-10", PeriodStart: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), CloseStatus: "OPEN"}
	if _, err := s.CreateFiscalPeriod(ctx, fp); err != nil {
		t.Fatal(err)
	}

	// Key order and spacing deliberately not canonical: the stored text must
	// come back byte for byte, or the signed hash stops verifying.
	manifest := `{"subledger_control_gate":"enforce",  "bank_reconciliation_gate":"enforce","checklist_requirement_ids":[]}`
	legacy := &domain.CloseEvidence{EvidenceID: uuid.NewString(), TenantID: tenantID, FiscalPeriodID: fp.FiscalPeriodID,
		TrialBalanceHash: "tb-1", Signature: "sig-1", GeneratedAt: time.Now().UTC().Add(-time.Hour)}
	pinned := &domain.CloseEvidence{EvidenceID: uuid.NewString(), TenantID: tenantID, FiscalPeriodID: fp.FiscalPeriodID,
		TrialBalanceHash: "tb-2", Signature: "sig-2", GeneratedAt: time.Now().UTC(),
		RelianceManifest: manifest, RelianceHash: "abc", RelianceSignature: "def"}
	for _, ev := range []*domain.CloseEvidence{legacy, pinned} {
		if err := s.CreateCloseEvidence(ctx, ev, "corr-1", "actor-1", "2026-10", "le-1"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListCloseEvidence(ctx, fp.FiscalPeriodID)
	if err != nil || len(got) != 2 {
		t.Fatalf("list: %d rows, err %v", len(got), err)
	}
	if got[0].EvidenceID != legacy.EvidenceID || got[0].RelianceManifest != "" {
		t.Fatalf("legacy evidence must come first with no manifest: %+v", got[0])
	}
	if got[1].RelianceManifest != manifest || got[1].RelianceHash != "abc" || got[1].RelianceSignature != "def" {
		t.Fatalf("manifest altered in storage: %q", got[1].RelianceManifest)
	}

	half := &domain.CloseEvidence{EvidenceID: uuid.NewString(), TenantID: tenantID, FiscalPeriodID: fp.FiscalPeriodID,
		TrialBalanceHash: "tb-3", Signature: "sig-3", GeneratedAt: time.Now().UTC(), RelianceManifest: manifest}
	if err := s.CreateCloseEvidence(ctx, half, "corr-1", "actor-1", "2026-10", "le-1"); err == nil {
		t.Fatal("a manifest without its hash and signature was stored")
	}

	other := uuid.NewString()
	if rows, err := s.ListCloseEvidence(svcmiddleware.WithTenant(context.Background(), other), fp.FiscalPeriodID); err != nil || len(rows) != 0 {
		t.Fatalf("another tenant read this period's evidence: %d rows, %v", len(rows), err)
	}
}
