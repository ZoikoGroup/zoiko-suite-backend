package store_test

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/store"
)

// TestPgStore_BackfillObligationIsIdempotentAndPreservesHistory proves the
// WFC-05 consolidation backfill (cmd/migrate-duplicates): a historical row
// from obligation-tracking-svc lands with its own evidence intact, reaches a
// non-initial EXTENDED status directly (never replayed through
// CreateObligation/TransitionExtended's live-command gates), and a repeat
// backfill of the same source row is a no-op, not a duplicate or an error.
func TestPgStore_BackfillObligationIsIdempotentAndPreservesHistory(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	waivedAt := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	authorityRef, reasonCode, waivedBy := "POLICY-9", "IMMATERIAL", "legacy-admin"
	params := domain.BackfillObligationParams{
		Obligation: domain.Obligation{
			ObligationID:         "cccccccc-0000-0000-0000-000000000001",
			LegalEntityID:        "00000000-0000-0000-0000-000000000001",
			JurisdictionID:       "00000000-0000-0000-0000-000000000002",
			ObligationSourceType: "CONTRACT_CLAUSE",
			ObligationSourceID:   "ot-src-42",
			ObligationCode:       "OTS-ot-src-42",
			ObligationType:       "CONTRACTUAL",
			ObligationStatus:     domain.ExtendedStatusWaived,
			DueDate:              time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC),
			SeverityLevel:        "MEDIUM",
			ResponsibleFunction:  "Legal",
			SourceReference:      "obligation-tracking-svc:CONTRACT_CLAUSE:ot-src-42",
			CreatedAt:            time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			CreatedByPrincipalID: "legacy-admin",
			UpdatedAt:            waivedAt,
			LifecycleModel:       domain.LifecycleModelExtended,
			ClockClass:           domain.ClockClassContractual,
			WaivedAt:             &waivedAt,
			WaivedBy:             &waivedBy,
			WaiverAuthorityReference: &authorityRef,
			WaiverReasonCode:         &reasonCode,
		},
		CorrelationID: "migrate-1",
	}

	out, created, err := s.BackfillObligation(ctx, params)
	if err != nil || !created {
		t.Fatalf("backfill: created=%v err=%v", created, err)
	}
	if out.ObligationStatus != domain.ExtendedStatusWaived {
		t.Fatalf("expected the historical WAIVED status preserved verbatim, got %s", out.ObligationStatus)
	}
	if out.WaiverAuthorityReference == nil || *out.WaiverAuthorityReference != "POLICY-9" {
		t.Fatalf("expected waiver evidence preserved, got %+v", out.WaiverAuthorityReference)
	}
	if out.LifecycleModel != domain.LifecycleModelExtended {
		t.Fatalf("expected EXTENDED lifecycle_model, got %s", out.LifecycleModel)
	}
	if !out.CreatedAt.Equal(params.CreatedAt) {
		t.Fatalf("expected the original created_at preserved, got %v want %v", out.CreatedAt, params.CreatedAt)
	}

	// Re-running the backfill for the same source row (same deterministic
	// obligation_id) must be a no-op, not a duplicate or a conflict error.
	again, created, err := s.BackfillObligation(ctx, params)
	if err != nil || created {
		t.Fatalf("repeat backfill: created=%v err=%v", created, err)
	}
	if again.ObligationID != out.ObligationID {
		t.Fatalf("repeat backfill returned a different row: %s vs %s", again.ObligationID, out.ObligationID)
	}

	// A row backfilled straight to WAIVED must not be reachable by replaying
	// it through the live command path — TransitionExtended must see it
	// exactly as any other EXTENDED obligation and refuse to leave WAIVED,
	// proving the backfill is not a side door around today's state machine.
	if _, _, err := s.TransitionExtended(ctx, out.ObligationID, domain.ExtendedStatusFulfilled, 0, "actor-1", "corr-2",
		domain.ExtendedTransitionEvidence{}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition out of backfilled terminal WAIVED, got %v", err)
	}

	// Required fields are still enforced — the backfill preserves history, it
	// does not waive the platform's own non-negotiable invariants.
	bad := params
	bad.ObligationID = "cccccccc-0000-0000-0000-000000000002"
	bad.JurisdictionID = ""
	if _, _, err := s.BackfillObligation(ctx, bad); !errors.Is(err, domain.ErrInvalidField) {
		t.Fatalf("expected ErrInvalidField with no jurisdiction_id, got %v", err)
	}
}

// TestPgStore_BackfillFilingRequirementNeedsItsParentObligationFirst proves
// the filing-tracker-svc backfill shape: a filing-tracker-svc row has no
// parent obligation of its own, so the tool must create a synthesized one
// first (BackfillObligation) — BackfillFilingRequirement refuses to attach a
// filing requirement to an obligation_id that does not exist, and is itself
// idempotent on filing_requirement_id.
func TestPgStore_BackfillFilingRequirementNeedsItsParentObligationFirst(t *testing.T) {
	admin := getTestPool(t)
	defer admin.Close()
	setupTestDB(t, admin)
	s := store.New(rlsPool(t, admin), zap.NewNop())
	ctx := tenantCtx(testTenantID)

	filingParams := domain.BackfillFilingRequirementParams{
		FilingRequirement: domain.FilingRequirement{
			FilingRequirementID: "dddddddd-0000-0000-0000-000000000001",
			ObligationID:        "eeeeeeee-0000-0000-0000-000000000001",
			FilingType:          "VAT",
			FilingAuthority:     "HMRC",
			SubmissionChannel:   "PORTAL",
			FilingStatus:        "CONFIRMED",
		},
		CorrelationID: "migrate-2",
	}
	if _, _, err := s.BackfillFilingRequirement(ctx, filingParams); !errors.Is(err, domain.ErrObligationNotFound) {
		t.Fatalf("expected ErrObligationNotFound with no parent, got %v", err)
	}

	parent := domain.BackfillObligationParams{
		Obligation: domain.Obligation{
			ObligationID:         "eeeeeeee-0000-0000-0000-000000000001",
			LegalEntityID:        "00000000-0000-0000-0000-000000000001",
			JurisdictionID:       "00000000-0000-0000-0000-000000000003",
			ObligationSourceType: "FILING_TRACKER_SVC",
			ObligationSourceID:   "fts-7",
			ObligationCode:       "FTS-fts-7",
			ObligationType:       "FILING",
			ObligationStatus:     domain.ExtendedStatusActive,
			DueDate:              time.Date(2025, 9, 30, 0, 0, 0, 0, time.UTC),
			SeverityLevel:        "MEDIUM",
			ResponsibleFunction:  "Tax",
			SourceReference:      "filing-tracker-svc:fts-7",
			CreatedAt:            time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			CreatedByPrincipalID: "legacy-admin",
			UpdatedAt:            time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			LifecycleModel:       domain.LifecycleModelExtended,
			ClockClass:           domain.ClockClassRegulatory,
		},
		CorrelationID: "migrate-2",
	}
	if _, created, err := s.BackfillObligation(ctx, parent); err != nil || !created {
		t.Fatalf("backfill parent: created=%v err=%v", created, err)
	}

	f, created, err := s.BackfillFilingRequirement(ctx, filingParams)
	if err != nil || !created || f.FilingStatus != "CONFIRMED" {
		t.Fatalf("backfill filing: created=%v status=%v err=%v", created, f, err)
	}
	again, created, err := s.BackfillFilingRequirement(ctx, filingParams)
	if err != nil || created || again.FilingRequirementID != f.FilingRequirementID {
		t.Fatalf("repeat backfill filing: created=%v err=%v", created, err)
	}

	// An unrecognized filing_status is refused, not silently inserted.
	badStatus := filingParams
	badStatus.FilingRequirementID = "dddddddd-0000-0000-0000-000000000002"
	badStatus.FilingStatus = "NONSENSE"
	if _, _, err := s.BackfillFilingRequirement(ctx, badStatus); !errors.Is(err, domain.ErrInvalidField) {
		t.Fatalf("expected ErrInvalidField for an unknown filing_status, got %v", err)
	}
}
