//go:build integration

// Real-Postgres tests for SPEC_DEVIATIONS A5 (manual match).
//
// Run: go test -v -tags=integration -count=1 -timeout=180s ./internal/store/
package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
)

// manualMatchFixture runs one execution that leaves side A's "a1" and side
// B's "g1" unmatched (different references, so automatic reference matching
// never pairs them) — exactly the "an unmatched pair a human can explain"
// case A5 exists for.
func manualMatchFixture(t *testing.T, amtA, amtB string) (*e2e, *domain.ControlRun) {
	t.Helper()
	x := newE2E(t)
	x.src.set("ar", "invoices", "wm", domain.PopulationRecord{RecordID: "a1", Reference: "INV-1", Amount: amtA, Currency: "USD", Date: "2026-09-01"})
	x.src.set("gl", "ar-postings", "wm", domain.PopulationRecord{RecordID: "g1", Reference: "INV-OTHER", Amount: amtB, Currency: "USD", Date: "2026-09-01"})
	_, final := x.start("k1")
	require.Equal(t, domain.ResultFail, final.ResultState, "both records are MISSING their counterpart before any manual match")
	return x, final
}

func TestManualMatch_WithinPinnedToleranceSucceeds(t *testing.T) {
	x, run := manualMatchFixture(t, "10.00", "9.99")
	tol, err := testStore.CreateTolerancePolicy(ctx, x.tenant, "maker", domain.CreateTolerancePolicyRequest{
		LegalEntityID: x.entity, Metric: "FIN-CTRL-001", AbsoluteTolerance: "0.05", Currency: "USD",
		Rationale: "cent rounding", EffectiveFrom: "2026-01-01"})
	require.NoError(t, err)
	_, err = testStore.ApproveTolerancePolicy(ctx, x.tenant, x.entity, tol.ToleranceID, "owner")
	require.NoError(t, err)

	// The tolerance only governs a NEW run (Invariant 4); re-pin this one by
	// creating a fresh run so it actually resolves the now-approved policy.
	run2, _, err := testStore.CreateRun(ctx, x.tenant, "prep", "c", "k2", runReq(x.def.ControlDefinitionID, x.entity))
	require.NoError(t, err)
	run2 = x.execute(run2, "k2")
	_ = run // the first run (no tolerance) is exercised by the exceeds-tolerance test below

	m, err := testStore.CreateManualMatch(ctx, x.tenant, run2.RunID, "reviewer", domain.CreateManualMatchRequest{
		SideARecordIDs: []string{"a1"}, SideBRecordIDs: []string{"g1"}, Reason: "confirmed with AR team, timing difference",
		EvidenceRef: "ticket-123"})
	require.NoError(t, err)
	// NUMERIC(38,12)::text, same full-scale convention as tolerance_policies'
	// absolute_tolerance (see TestRun_TolerancePinned_NewVersionOnlyAffectsNewRuns).
	assert.Equal(t, "10.000000000000", m.AmountA)
	assert.Equal(t, "9.990000000000", m.AmountB)
	assert.Equal(t, "0.010000000000", m.Difference, "the real difference is recorded, never hidden (§10)")
	assert.Equal(t, "USD", m.Currency)
	assert.Equal(t, "confirmed with AR team, timing difference", m.Reason)
	assert.Equal(t, "ticket-123", m.EvidenceRef)
	assert.Equal(t, "reviewer", m.CreatedBy)
	assert.Equal(t, []string{"a1"}, m.SideARecordIDs)
	assert.Equal(t, []string{"g1"}, m.SideBRecordIDs)

	list, err := testStore.ListManualMatches(ctx, x.tenant, run2.RunID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, m.ManualMatchID, list[0].ManualMatchID)
}

func TestManualMatch_ExceedsPinnedToleranceRefused(t *testing.T) {
	// No tolerance policy exists for this entity/metric: the pinned tolerance
	// is the "exact match required" default (0), so any non-zero difference
	// must be refused outright — never silently matched.
	_, run := manualMatchFixture(t, "10.00", "9.99")

	_, err := testStore.CreateManualMatch(ctx, run.TenantID, run.RunID, "reviewer", domain.CreateManualMatchRequest{
		SideARecordIDs: []string{"a1"}, SideBRecordIDs: []string{"g1"}, Reason: "r", EvidenceRef: "e"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	list, err := testStore.ListManualMatches(ctx, run.TenantID, run.RunID)
	require.NoError(t, err)
	assert.Empty(t, list, "a refused manual match must leave no trace — it was never honestly matched")
}

func TestManualMatch_CurrencyMismatchRefused(t *testing.T) {
	x := newE2E(t)
	x.src.set("ar", "invoices", "wm", domain.PopulationRecord{RecordID: "a1", Reference: "INV-1", Amount: "10.00", Currency: "USD", Date: "2026-09-01"})
	x.src.set("gl", "ar-postings", "wm", domain.PopulationRecord{RecordID: "g1", Reference: "INV-OTHER", Amount: "10.00", Currency: "EUR", Date: "2026-09-01"})
	run, _ := x.start("k1")

	_, err := testStore.CreateManualMatch(ctx, x.tenant, run.RunID, "reviewer", domain.CreateManualMatchRequest{
		SideARecordIDs: []string{"a1"}, SideBRecordIDs: []string{"g1"}, Reason: "r", EvidenceRef: "e"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "no FX is applied without an approved rate basis (§10)")
}

func TestManualMatch_UnknownRecordIDRefused(t *testing.T) {
	_, run := manualMatchFixture(t, "10.00", "10.00")
	_, err := testStore.CreateManualMatch(ctx, run.TenantID, run.RunID, "reviewer", domain.CreateManualMatchRequest{
		SideARecordIDs: []string{"does-not-exist"}, SideBRecordIDs: []string{"g1"}, Reason: "r", EvidenceRef: "e"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestManualMatch_RefusedOnCertifiedRun(t *testing.T) {
	_, run := manualMatchFixture(t, "10.00", "10.00")
	// Test-only setup: force the certification dimension to CERTIFIED directly,
	// bypassing the real governed certify workflow, purely to exercise this
	// guard in isolation (control_runs carries no append-only trigger, unlike
	// the tables CertifyRun itself writes).
	_, err := testPool.Exec(ctx, `UPDATE control_runs SET result_state = 'PASS', certification_state = 'CERTIFIED' WHERE run_id = $1`, run.RunID)
	require.NoError(t, err)

	_, err = testStore.CreateManualMatch(ctx, run.TenantID, run.RunID, "reviewer", domain.CreateManualMatchRequest{
		SideARecordIDs: []string{"a1"}, SideBRecordIDs: []string{"g1"}, Reason: "r", EvidenceRef: "e"})
	assert.ErrorIs(t, err, domain.ErrInvalidTransition, "a certified run's evidence is sealed (Invariant 2/9)")
}

func TestManualMatch_RowsAreAppendOnly(t *testing.T) {
	x, run := manualMatchFixture(t, "10.00", "10.00")
	_ = x
	m, err := testStore.CreateManualMatch(ctx, run.TenantID, run.RunID, "reviewer", domain.CreateManualMatchRequest{
		SideARecordIDs: []string{"a1"}, SideBRecordIDs: []string{"g1"}, Reason: "r", EvidenceRef: "e"})
	require.NoError(t, err)

	_, err = testPool.Exec(ctx, `UPDATE manual_matches SET reason = 'changed' WHERE manual_match_id = $1`, m.ManualMatchID)
	assert.Error(t, err, "manual_matches rows are append-only")
	_, err = testPool.Exec(ctx, `DELETE FROM manual_matches WHERE manual_match_id = $1`, m.ManualMatchID)
	assert.Error(t, err, "manual_matches rows can never be deleted")
}

func TestManualMatch_UnknownRunRefused(t *testing.T) {
	x := newE2E(t)
	_, err := testStore.CreateManualMatch(ctx, x.tenant, "00000000-0000-0000-0000-000000000000", "reviewer",
		domain.CreateManualMatchRequest{SideARecordIDs: []string{"a1"}, SideBRecordIDs: []string{"g1"}, Reason: "r", EvidenceRef: "e"})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
