package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/store"
)

func definePopulationParams(correlationID string) domain.DefinePopulationParams {
	return domain.DefinePopulationParams{
		EngagementID: uuid.New().String(), LegalEntityID: uuid.New().String(), ObjectClass: "JOURNAL_ENTRY",
		PeriodStart: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC), PeriodEnd: time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
		SourceSystem: "general-ledger-svc", SourceQuery: "fiscal_year=2026", SourceWatermark: "2026-04-01T00:00:00Z",
		Assertion: "COMPLETENESS", ExpectedCompletenessCheck: "row count matches GL journal count",
		CreatedByPrincipalID: "preparer-1", CorrelationID: correlationID,
	}
}

// TestPgStore_FreezePopulation_RequiresReconciledControlTotals is the
// real proof of AUD-CTRL-007 "population completeness" / AUD-NEG-008
// ("count matches, monetary total differs -> quarantine, not freeze"):
// a mismatched control total routes the population to QUARANTINED, never
// to FROZEN.
func TestPgStore_FreezePopulation_RequiresReconciledControlTotals(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop, created, err := s.DefinePopulation(ctx, definePopulationParams("pop-create-1"))
	require.NoError(t, err)
	require.True(t, created)

	built, _, err := s.BuildPopulation(ctx, domain.BuildPopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-build-1",
		Rows: []domain.BuildPopulationRow{
			{SourceRecordID: "JE-1", Amount: floatPtr(100), RowSnapshot: []byte(`{"je":"1"}`)},
			{SourceRecordID: "JE-2", Amount: floatPtr(200), RowSnapshot: []byte(`{"je":"2"}`)},
		},
	})
	require.NoError(t, err)
	require.Equal(t, domain.PopulationValidating, built.Status)

	// The caller asserts the SOURCE system reports 999 — deliberately
	// wrong versus the real computed sum (300) — to prove FreezePopulation
	// actually recomputes rather than trusting the caller.
	_, _, err = s.ValidatePopulation(ctx, domain.ValidatePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-validate-1",
		ControlTotals: map[string]float64{"record_count": 2, "sum_amount": 999},
	})
	require.NoError(t, err)

	frozen, _, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-freeze-1"})
	require.ErrorIs(t, err, domain.ErrPopulationControlTotalMismatch)
	require.NotNil(t, frozen)
	require.Equal(t, domain.PopulationQuarantined, frozen.Status)
	require.NotNil(t, frozen.QuarantineReason)
	require.Contains(t, *frozen.QuarantineReason, "sum_amount")
}

// TestPgStore_FreezePopulation_Succeeds_WithReconciledTotals is the
// positive-path companion: correct control totals produce a real
// FROZEN population with a non-empty digest and row_count.
func TestPgStore_FreezePopulation_Succeeds_WithReconciledTotals(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop, _, err := s.DefinePopulation(ctx, definePopulationParams("pop-create-2"))
	require.NoError(t, err)
	_, _, err = s.BuildPopulation(ctx, domain.BuildPopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-build-2",
		Rows: []domain.BuildPopulationRow{
			{SourceRecordID: "JE-1", Amount: floatPtr(100), RowSnapshot: []byte(`{"je":"1"}`)},
			{SourceRecordID: "JE-2", Amount: floatPtr(200), RowSnapshot: []byte(`{"je":"2"}`)},
		},
	})
	require.NoError(t, err)
	_, _, err = s.ValidatePopulation(ctx, domain.ValidatePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-validate-2",
		ControlTotals: map[string]float64{"record_count": 2, "sum_amount": 300},
	})
	require.NoError(t, err)

	frozen, changed, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-freeze-2"})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, domain.PopulationFrozen, frozen.Status)
	require.NotNil(t, frozen.DigestSHA256)
	require.NotEmpty(t, *frozen.DigestSHA256)
	require.NotNil(t, frozen.RowCount)
	require.EqualValues(t, 2, *frozen.RowCount)
}

// TestPgStore_FrozenPopulation_IsImmutable proves "frozen population
// immutable" at the DB layer: a raw UPDATE on a FROZEN row is rejected by
// the trigger, independent of any application-level check.
func TestPgStore_FrozenPopulation_IsImmutable(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop, _, err := s.DefinePopulation(ctx, definePopulationParams("pop-create-3"))
	require.NoError(t, err)
	_, _, err = s.BuildPopulation(ctx, domain.BuildPopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-build-3",
		Rows: []domain.BuildPopulationRow{{SourceRecordID: "JE-1", Amount: floatPtr(50), RowSnapshot: []byte(`{}`)}},
	})
	require.NoError(t, err)
	_, _, err = s.ValidatePopulation(ctx, domain.ValidatePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-validate-3",
		ControlTotals: map[string]float64{"record_count": 1},
	})
	require.NoError(t, err)
	frozen, _, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-freeze-3"})
	require.NoError(t, err)
	require.Equal(t, domain.PopulationFrozen, frozen.Status)

	_, err = pool.Exec(ctx, `UPDATE audit_populations SET object_class='TAMPERED' WHERE population_id=$1`, frozen.PopulationID)
	require.Error(t, err, "expected the reject_frozen_population_mutation trigger to refuse content tampering on a frozen population")

	_, err = pool.Exec(ctx, `UPDATE audit_populations SET digest_sha256='deadbeef' WHERE population_id=$1`, frozen.PopulationID)
	require.Error(t, err, "expected the trigger to refuse tampering with the digest specifically")

	// A pure status transition (no content field touched) IS legitimate —
	// this is how SupersedePopulation/AddControlledDelta themselves move a
	// frozen row, and the trigger must not block its own store's own
	// controlled state machine.
	_, err = pool.Exec(ctx, `UPDATE audit_populations SET status='IN_USE' WHERE population_id=$1`, frozen.PopulationID)
	require.NoError(t, err, "expected a pure status transition to be permitted once frozen")

	// But once terminal (SUPERSEDED/QUARANTINED), no UPDATE at all is
	// permitted, including a further status change.
	_, err = pool.Exec(ctx, `UPDATE audit_populations SET status='SUPERSEDED' WHERE population_id=$1`, frozen.PopulationID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE audit_populations SET status='FROZEN' WHERE population_id=$1`, frozen.PopulationID)
	require.Error(t, err, "expected a terminal SUPERSEDED population to refuse any further mutation")
}

// TestPgStore_AddControlledDelta_NeverMutatesFrozenPopulation is the real
// proof of AUD-NEG-010 "late journal appears after frozen population ->
// create controlled delta/new version; do not mutate original": the new
// version carries the old rows plus the delta, and the old population
// becomes SUPERSEDED, never edited in place.
func TestPgStore_AddControlledDelta_NeverMutatesFrozenPopulation(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop, _, err := s.DefinePopulation(ctx, definePopulationParams("pop-create-4"))
	require.NoError(t, err)
	_, _, err = s.BuildPopulation(ctx, domain.BuildPopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-build-4",
		Rows: []domain.BuildPopulationRow{{SourceRecordID: "JE-1", Amount: floatPtr(100), RowSnapshot: []byte(`{}`)}},
	})
	require.NoError(t, err)
	_, _, err = s.ValidatePopulation(ctx, domain.ValidatePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-validate-4",
		ControlTotals: map[string]float64{"record_count": 1},
	})
	require.NoError(t, err)
	frozen, _, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "pop-freeze-4"})
	require.NoError(t, err)
	origDigest := *frozen.DigestSHA256

	newPop, created, err := s.AddControlledDelta(ctx, domain.AddControlledDeltaParams{
		PopulationID: frozen.PopulationID, TenantID: testTenant, Reason: "late journal entry received",
		CreatedByPrincipalID: "preparer-1", CorrelationID: "pop-delta-1",
		Rows: []domain.BuildPopulationRow{{SourceRecordID: "JE-2-LATE", Amount: floatPtr(75), RowSnapshot: []byte(`{}`)}},
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, frozen.PopulationID, newPop.PopulationID)
	require.Equal(t, domain.PopulationDefined, newPop.Status)
	require.NotNil(t, newPop.PriorPopulationID)
	require.Equal(t, frozen.PopulationID, *newPop.PriorPopulationID)

	oldReloaded, err := s.GetAuditPopulation(ctx, testTenant, frozen.PopulationID)
	require.NoError(t, err)
	require.Equal(t, domain.PopulationSuperseded, oldReloaded.Status)
	require.NotNil(t, oldReloaded.DigestSHA256)
	require.Equal(t, origDigest, *oldReloaded.DigestSHA256, "the original frozen population's own digest must never change")
	require.NotNil(t, oldReloaded.SupersededByPopulationID)
	require.Equal(t, newPop.PopulationID, *oldReloaded.SupersededByPopulationID)

	var rowCount int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM population_rows WHERE population_id=$1`, newPop.PopulationID).Scan(&rowCount)
	require.NoError(t, err)
	require.Equal(t, 2, rowCount, "expected the new version to carry the old row plus the delta row")
}

func floatPtr(f float64) *float64 { return &f }
