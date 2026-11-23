package domain

// Specification negative paths (rule R10: every one has a test named after its
// id). ZS-JUR-001 s34 JUR-NEG-nn and ZS-SVC-V-001 s27 PDC-02 NP-nn scenarios
// proved by the pure domain logic are named here; each runs the test that
// proves it. The ones that need the database run in internal/registryit,
// internal/store and internal/resolver. The full index with the status of every
// id is in progress.md section 4.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// JUR-NEG-01: a transaction dated before a new rate's effective date resolves the old rule version.
func TestJURNEG01_TransactionBeforeANewRateUsesTheOldRule(t *testing.T) {
	TestResolve_IsEffectiveDated_AndHalfOpen(t)
}

// JUR-NEG-02: two active rules overlapping without declared precedence fail certification (the compiler refuses them).
func TestJURNEG02_OverlapWithoutPrecedenceIsRefused(t *testing.T) {
	TestCompile_OverlapNeedsDeclaredResolution(t)
}

// JUR-NEG-03 and JUR-NEG-04: an artifact whose digest does not match, or that is unsigned, is refused (fail closed).
func TestJURNEG03_DigestMismatchIsRefused(t *testing.T) {
	TestVerify_AcceptsAGoodSignature_AndFailsClosedOtherwise(t)
}

func TestJURNEG04_UnsignedPackFailsClosed(t *testing.T) {
	TestVerify_AcceptsAGoodSignature_AndFailsClosedOtherwise(t)
}

// JUR-NEG-05 (and PDC-02 NP-13): a client-supplied jurisdiction never widens what the entity's facts authorise.
func TestJURNEG05_AClientClaimIsNotAuthoritative(t *testing.T) {
	TestDetermineJurisdiction_AClaimNeverWidensTheAuthorisedSet(t)
}

func TestPDC02NP13_LocaleOrClaimIsNotALegalFact(t *testing.T) {
	TestDetermineJurisdiction_AClaimNeverWidensTheAuthorisedSet(t)
}

// JUR-NEG-09 and JUR-NEG-11: a rejection is never a false accepted state, and a late
// or out-of-order callback cannot regress the submission.
func TestJURNEG09_RejectedEInvoiceIsNotAccepted(t *testing.T) {
	TestEvaluateEvent_NoFalseAcceptedNoRegression(t)
}

func TestJURNEG11_LateCallbackCannotRegress(t *testing.T) {
	TestEvaluateEvent_NoFalseAcceptedNoRegression(t)
}

// JUR-NEG-13: an amended holiday calendar is a new version; earlier due dates stay on the earlier version.
func TestJURNEG13_HolidayAmendmentIsANewVersion(t *testing.T) {
	TestCalendarSelection_IsByEffectiveDate_AndAmendmentsAreNewVersions(t)
}

// JUR-NEG-18: an AI-proposed (or any unapproved) change cannot be compiled into a releasable pack.
func TestJURNEG18_UnapprovedChangeCannotBeReleased(t *testing.T) {
	TestCompile_RejectsEachViolation(t)
}

// JUR-NEG-21: a future-dated rule shipped early in a released pack is selected only
// from its effective date; before it, the rule in force (or none) answers.
func TestJURNEG21_FutureDatedRuleAppliesOnlyFromItsEffectiveDate(t *testing.T) {
	a := twoRuleArtifact() // r2 starts 2026-06-01 and is in the artifact from the start
	before := a.Resolve("GB", "TAX", "STD", tm(2026, 5, 31))
	assert.Equal(t, OutcomeResolved, before.Outcome)
	assert.Equal(t, "r1", before.RuleID, "the early-deployed rule is not used before its date")
	at := a.Resolve("GB", "TAX", "STD", tm(2026, 6, 1))
	assert.Equal(t, "r2", at.RuleID, "it is used from its date")

	// A pack holding only a future rule answers nothing before that rule's date.
	future := &ArtifactDoc{PackRef: "jur.gb.tax.future", PackVersion: "1.0",
		Jurisdictions: []ArtifactScopeEntry{{ID: "j-gb", Code: "GB"}},
		Rules:         []ArtifactRule{{RuleID: "f1", JurisdictionID: "j-gb", RuleDomain: "TAX", RuleCode: "NEW", EffectiveFrom: tm(2027, 1, 1), ContentDigest: "sha256:f1"}}}
	assert.Equal(t, OutcomeNoRule, future.Resolve("GB", "TAX", "NEW", time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)).Outcome)
	assert.Equal(t, OutcomeResolved, future.Resolve("GB", "TAX", "NEW", tm(2027, 1, 1)).Outcome)
}

// JUR-NEG-22: an unsupported jurisdiction gets an explicit answer, never a guessed treatment.
func TestJURNEG22_UnsupportedJurisdictionIsExplicit(t *testing.T) {
	TestResolve_UnsupportedJurisdictionIsExplicit_NotGuessed(t)
}

// JUR-NEG-23: a source classified as guidance cannot carry a mandatory rule without an approved interpretation.
func TestJURNEG23_GuidanceNeedsAnApprovedInterpretation(t *testing.T) {
	TestCompile_Warnings(t)
}

// JUR-NEG-25: binary floating-point arithmetic in a financially material rule is refused.
func TestJURNEG25_FloatArithmeticIsRefused(t *testing.T) {
	TestCompile_RejectsEachViolation(t)
}

// JUR-NEG-26: a due date landing on an authority holiday moves by the declared business-day adjustment.
func TestJURNEG26_DueDateOnAHolidayIsAdjusted(t *testing.T) {
	TestDueDate_IsDerived_AdjustedForWeekendsAndHolidays(t)
}

// PDC-02 NP-12: several authorised jurisdictions with nothing selecting between them are a review item, not a choice.
func TestPDC02NP12_TwoPlausibleJurisdictionsNeedReview(t *testing.T) {
	TestDetermineJurisdiction_SeveralAuthorisedNeedReviewUnlessAClaimSelectsAmongThem(t)
}

// PDC-02 NP-11 / NP-18: nothing authorised never falls back to a default or to the claim.
func TestPDC02NP18_NoMatchNeverFallsBack(t *testing.T) {
	TestDetermineJurisdiction_NothingAuthorisedNeverFallsBackToTheClaim(t)
}

// PDC-02 NP-19: a deadline instant uses the authority's IANA time zone and cutoff, not a fixed offset.
func TestPDC02NP19_DeadlinesUseTheAuthorityTimeZone(t *testing.T) {
	TestDueDate_InstantUsesTheAuthorityTimeZoneAndCutoff(t)
}

// REF-10 #41: a payment term whose basis date cannot be resolved is refused, never guessed.
func TestREF10NP41_UnresolvableBasisDateIsRefused(t *testing.T) {
	TestCalculatePaymentDueDate_BasisDates(t)
	TestCalculatePaymentDueDate_RefusalsAndDeterminism(t)
}
