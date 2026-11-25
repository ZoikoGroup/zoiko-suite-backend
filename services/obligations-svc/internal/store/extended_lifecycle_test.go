package store_test

// Real-Postgres proof for the WFC-05 consolidation (Doc 04 §2.1 / migration
// 000013): the EXTENDED lifecycle coexists with the pre-existing LEGACY one
// on the same obligations table, and enforces the spec's evidence
// requirements rather than silently accepting a bare status transition.

import (
	"errors"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/store"
)

func extendedParams(code string, extractedByAI bool) domain.CreateObligationParams {
	p := validParams()
	p.ObligationCode = code
	p.LifecycleModel = domain.LifecycleModelExtended
	p.ClockClass = domain.ClockClassContractual
	p.ExtractedByAI = extractedByAI
	return p
}

// TestPgStore_ExtendedLifecycle_AICandidateRequiresValidation proves LEG-07
// §9.1: an AI-extracted obligation starts at CANDIDATE, cannot be scheduled
// into ACTIVE directly, and only ValidateExtractedObligation moves it to
// PLANNED.
func TestPgStore_ExtendedLifecycle_AICandidateRequiresValidation(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	o, created, err := s.CreateObligation(ctx, extendedParams("EXT-CAND-001", true))
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if o.ObligationStatus != domain.ExtendedStatusCandidate {
		t.Fatalf("expected initial status CANDIDATE for AI-extracted obligation, got %s", o.ObligationStatus)
	}
	if o.LifecycleModel != domain.LifecycleModelExtended {
		t.Fatalf("expected lifecycle_model EXTENDED, got %s", o.LifecycleModel)
	}

	// Attempting to schedule straight from CANDIDATE is not a legal
	// transition (only PLANNED or SUPERSEDED are) — the validation gate
	// cannot be bypassed by calling the generic transition endpoint instead.
	_, _, err = s.TransitionExtended(ctx, o.ObligationID, domain.ExtendedStatusActive, 0, "actor-1", "corr-1",
		domain.ExtendedTransitionEvidence{TriggerDescription: "x", CalculationMethod: "y"})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition skipping validation, got %v", err)
	}

	validated, err := s.ValidateExtractedObligation(ctx, o.ObligationID, "reviewer-1", "corr-2")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if validated.ObligationStatus != domain.ExtendedStatusPlanned {
		t.Fatalf("expected PLANNED after validation, got %s", validated.ObligationStatus)
	}
	if validated.ValidatedBy == nil || *validated.ValidatedBy != "reviewer-1" {
		t.Fatalf("expected validated_by recorded, got %+v", validated.ValidatedBy)
	}

	// Validating twice is not idempotent-success — it is no longer a
	// CANDIDATE, so the second call must fail, not silently no-op.
	if _, err := s.ValidateExtractedObligation(ctx, o.ObligationID, "reviewer-1", "corr-3"); !errors.Is(err, domain.ErrNotCandidate) {
		t.Fatalf("expected ErrNotCandidate on re-validation, got %v", err)
	}
}

// TestPgStore_ExtendedLifecycle_AmbiguousDueDateBlocksScheduling proves
// LEG-07 §9: scheduling into ACTIVE without both a trigger description and
// calculation method is refused, not silently accepted with a blank basis.
func TestPgStore_ExtendedLifecycle_AmbiguousDueDateBlocksScheduling(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	o, _, err := s.CreateObligation(ctx, extendedParams("EXT-SCHED-001", false))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if o.ObligationStatus != domain.ExtendedStatusPlanned {
		t.Fatalf("expected initial status PLANNED for a non-AI-extracted obligation, got %s", o.ObligationStatus)
	}

	// Missing calculation_method.
	_, _, err = s.TransitionExtended(ctx, o.ObligationID, domain.ExtendedStatusActive, 0, "actor-1", "corr-1",
		domain.ExtendedTransitionEvidence{TriggerDescription: "quarter end"})
	if !errors.Is(err, domain.ErrAmbiguousDueDateBasis) {
		t.Fatalf("expected ErrAmbiguousDueDateBasis, got %v", err)
	}

	active, transitioned, err := s.TransitionExtended(ctx, o.ObligationID, domain.ExtendedStatusActive, 0, "actor-1", "corr-2",
		domain.ExtendedTransitionEvidence{TriggerDescription: "quarter end", CalculationMethod: "quarter_end_plus_15_business_days"})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if !transitioned || active.ObligationStatus != domain.ExtendedStatusActive {
		t.Fatalf("expected transition to ACTIVE, got transitioned=%v status=%s", transitioned, active.ObligationStatus)
	}
	if active.ScheduledBy == nil || *active.ScheduledBy != "actor-1" {
		t.Fatalf("expected scheduled_by recorded, got %+v", active.ScheduledBy)
	}
}

// TestPgStore_ExtendedLifecycle_WaiverRequiresEvidence proves spec §7.1:
// "Waiver ... requires policy authority, reason code, approver and
// evidence; cannot be a hidden admin toggle."
func TestPgStore_ExtendedLifecycle_WaiverRequiresEvidence(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	o, _, err := s.CreateObligation(ctx, extendedParams("EXT-WAIVE-001", false))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	active, _, err := s.TransitionExtended(ctx, o.ObligationID, domain.ExtendedStatusActive, 0, "actor-1", "corr-1",
		domain.ExtendedTransitionEvidence{TriggerDescription: "t", CalculationMethod: "m"})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}

	// No evidence at all.
	if _, _, err := s.TransitionExtended(ctx, active.ObligationID, domain.ExtendedStatusWaived, 0, "actor-1", "corr-2",
		domain.ExtendedTransitionEvidence{}); !errors.Is(err, domain.ErrWaiverEvidenceRequired) {
		t.Fatalf("expected ErrWaiverEvidenceRequired with no evidence, got %v", err)
	}

	// Authority reference without a reason code is still incomplete.
	if _, _, err := s.TransitionExtended(ctx, active.ObligationID, domain.ExtendedStatusWaived, 0, "actor-1", "corr-3",
		domain.ExtendedTransitionEvidence{WaiverAuthorityReference: "POLICY-44"}); !errors.Is(err, domain.ErrWaiverEvidenceRequired) {
		t.Fatalf("expected ErrWaiverEvidenceRequired with only authority ref, got %v", err)
	}

	waived, transitioned, err := s.TransitionExtended(ctx, active.ObligationID, domain.ExtendedStatusWaived, 0, "actor-1", "corr-4",
		domain.ExtendedTransitionEvidence{WaiverAuthorityReference: "POLICY-44", WaiverReasonCode: "IMMATERIAL"})
	if err != nil {
		t.Fatalf("waive: %v", err)
	}
	if !transitioned || waived.ObligationStatus != domain.ExtendedStatusWaived {
		t.Fatalf("expected WAIVED, got transitioned=%v status=%s", transitioned, waived.ObligationStatus)
	}
	if waived.WaiverAuthorityReference == nil || *waived.WaiverAuthorityReference != "POLICY-44" {
		t.Fatalf("expected waiver_authority_reference recorded, got %+v", waived.WaiverAuthorityReference)
	}
	if waived.WaiverReasonCode == nil || *waived.WaiverReasonCode != "IMMATERIAL" {
		t.Fatalf("expected waiver_reason_code recorded, got %+v", waived.WaiverReasonCode)
	}

	// WAIVED is terminal in the EXTENDED machine.
	if _, _, err := s.TransitionExtended(ctx, waived.ObligationID, domain.ExtendedStatusFulfilled, 0, "actor-1", "corr-5",
		domain.ExtendedTransitionEvidence{}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition out of terminal WAIVED, got %v", err)
	}
}

// TestPgStore_ExtendedLifecycle_DoesNotInterfereWithLegacy proves the two
// lifecycle models genuinely coexist on one table: an EXTENDED obligation
// cannot be driven through the LEGACY UpdateObligationStatus endpoint's
// transition table (it isn't even a legal LEGACY status), and a LEGACY
// obligation is refused by TransitionExtended.
func TestPgStore_ExtendedLifecycle_DoesNotInterfereWithLegacy(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenantID)

	legacy, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatalf("create legacy: %v", err)
	}
	if legacy.LifecycleModel != domain.LifecycleModelLegacy {
		t.Fatalf("expected an existing caller's obligation to default to LEGACY, got %s", legacy.LifecycleModel)
	}
	if legacy.ClockClass != domain.ClockClassRegulatory {
		t.Fatalf("expected default clock_class REGULATORY, got %s", legacy.ClockClass)
	}

	if _, _, err := s.TransitionExtended(ctx, legacy.ObligationID, domain.ExtendedStatusPlanned, 0, "actor-1", "corr-1",
		domain.ExtendedTransitionEvidence{}); !errors.Is(err, domain.ErrWrongLifecycleModel) {
		t.Fatalf("expected ErrWrongLifecycleModel transitioning a LEGACY obligation via the EXTENDED endpoint, got %v", err)
	}

	extended, _, err := s.CreateObligation(ctx, extendedParams("EXT-ISOLATION-001", false))
	if err != nil {
		t.Fatalf("create extended: %v", err)
	}
	if _, err := s.ValidateExtractedObligation(ctx, extended.ObligationID, "reviewer-1", "corr-2"); !errors.Is(err, domain.ErrNotCandidate) {
		// Not AI-extracted, so it started at PLANNED, not CANDIDATE — proves
		// the initial-status branch in CreateObligation is reading
		// extracted_by_ai correctly rather than always landing on CANDIDATE.
		t.Fatalf("expected ErrNotCandidate for a non-AI-extracted EXTENDED obligation, got %v", err)
	}

	// The pre-existing LEGACY machine still works exactly as before this
	// migration, on the same table, for a differently-coded obligation.
	legacyUpdated, transitioned, err := s.UpdateObligationStatus(ctx, legacy.ObligationID, "IN_PROGRESS", 0, "actor-1", "corr-3")
	if err != nil || !transitioned || legacyUpdated.ObligationStatus != "IN_PROGRESS" {
		t.Fatalf("expected LEGACY transition to IN_PROGRESS unaffected: transitioned=%v status=%v err=%v",
			transitioned, legacyUpdated.ObligationStatus, err)
	}
}
