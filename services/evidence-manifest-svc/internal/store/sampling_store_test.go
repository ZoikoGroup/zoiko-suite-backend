package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/store"
)

func floatPtr2(f float64) *float64 { return &f }

// buildAndFreezePopulation builds and freezes a population with rowCount
// sequential rows, each amount = its own 1-based index * 10 — so callers
// can pick a keyItemThreshold that deterministically selects a known
// subset as key items.
func buildAndFreezePopulation(t *testing.T, s *store.PgStore, ctx context.Context, correlationPrefix string, rowCount int) *domain.AuditPopulation {
	t.Helper()
	pop, _, err := s.DefinePopulation(ctx, definePopulationParams(correlationPrefix+"-define"))
	require.NoError(t, err)

	rows := make([]domain.BuildPopulationRow, rowCount)
	for i := 0; i < rowCount; i++ {
		amount := float64((i + 1) * 10)
		rows[i] = domain.BuildPopulationRow{SourceRecordID: "row", Amount: floatPtr2(amount), RowSnapshot: []byte(`{}`)}
	}
	_, _, err = s.BuildPopulation(ctx, domain.BuildPopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: correlationPrefix + "-build", Rows: rows})
	require.NoError(t, err)

	_, _, err = s.ValidatePopulation(ctx, domain.ValidatePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: correlationPrefix + "-validate",
		ControlTotals: map[string]float64{"record_count": float64(rowCount)},
	})
	require.NoError(t, err)

	frozen, _, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: correlationPrefix + "-freeze"})
	require.NoError(t, err)
	return frozen
}

func fullSampleDesignSetup(t *testing.T, s *store.PgStore, ctx context.Context, correlationPrefix string, rowCount int, sampleSize int, keyItemThreshold *float64) (*domain.AuditPopulation, *domain.SampleDesign) {
	t.Helper()
	pop := buildAndFreezePopulation(t, s, ctx, correlationPrefix, rowCount)

	ps, err := s.CreateSamplingParameterSet(ctx, domain.CreateSamplingParameterSetParams{Approach: domain.SamplingApproachSystematic, KeyItemThreshold: keyItemThreshold})
	require.NoError(t, err)

	design, _, err := s.CreateSampleDesign(ctx, domain.CreateSampleDesignParams{
		PopulationID: pop.PopulationID, Objective: "TEST_COMPLETENESS", ParamSetID: ps.ParamSetID, SampleSize: sampleSize,
		CreatedByPrincipalID: "preparer-1", CorrelationID: correlationPrefix + "-design",
	})
	require.NoError(t, err)

	approved, _, err := s.ApproveSampleDesign(ctx, domain.ApproveSampleDesignParams{DesignID: design.DesignID, ActorPrincipalID: "reviewer-1", CorrelationID: correlationPrefix + "-approve"})
	require.NoError(t, err)
	return pop, approved
}

// TestPgStore_SelectSample_KeyItemsAlwaysSelected proves "key items/100%
// strata explicit": every row at or above the threshold is selected
// deterministically, never subject to the interval formula.
func TestPgStore_SelectSample_KeyItemsAlwaysSelected(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	threshold := 90.0 // rows 9 and 10 (amounts 90, 100) qualify
	_, design := fullSampleDesignSetup(t, s, ctx, "select-key", 10, 2, &threshold)

	sel, items, _, err := s.SelectSample(ctx, domain.SelectSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "select-key-select"})
	require.NoError(t, err)
	require.NotNil(t, sel)

	keyCount := 0
	for _, it := range items {
		if it.IsKeyItem {
			keyCount++
		}
	}
	require.Equal(t, 2, keyCount, "expected both rows >= threshold to be selected as key items")
}

// TestPgStore_SelectSample_IsReproducible proves "selection method/seed
// preserved": ReproduceSelection recomputes the identical ordinal set
// from the selection's own stored inputs.
func TestPgStore_SelectSample_IsReproducible(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	_, design := fullSampleDesignSetup(t, s, ctx, "select-repro", 20, 5, nil)
	sel, _, _, err := s.SelectSample(ctx, domain.SelectSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "select-repro-select"})
	require.NoError(t, err)

	matches, err := s.ReproduceSelection(ctx, testTenant, sel.SelectionID)
	require.NoError(t, err)
	require.True(t, matches, "expected ReproduceSelection to recompute the identical ordinal set")
}

// TestPgStore_RecordItemResult_ObjectiveMismatch is the real proof of
// AUD-NEG-013: testing under a different objective than the design's own
// leaves the item non-terminal (still SELECTED), never silently TESTED.
func TestPgStore_RecordItemResult_ObjectiveMismatch(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	_, design := fullSampleDesignSetup(t, s, ctx, "objmismatch", 10, 3, nil)
	_, items, _, err := s.SelectSample(ctx, domain.SelectSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "objmismatch-select"})
	require.NoError(t, err)
	require.NotEmpty(t, items)
	item := items[0]

	_, _, err = s.RecordItemResult(ctx, domain.RecordItemResultParams{
		ItemID: item.ItemID, TenantID: testTenant, ActorPrincipalID: "tester-1",
		ObjectiveTested: "WRONG_OBJECTIVE", Result: "NO_EXCEPTION", CorrelationID: "objmismatch-result-1",
	})
	require.ErrorIs(t, err, domain.ErrObjectiveMismatch)

	items2, err := s.GetItemResults(ctx, testTenant, design.DesignID)
	require.NoError(t, err)
	for _, it := range items2 {
		if it.ItemID == item.ItemID {
			require.Equal(t, domain.SampleItemSelected, it.Status, "expected the item to remain non-terminal after an objective mismatch")
		}
	}

	_, changed, err := s.RecordItemResult(ctx, domain.RecordItemResultParams{
		ItemID: item.ItemID, TenantID: testTenant, ActorPrincipalID: "tester-1",
		ObjectiveTested: "TEST_COMPLETENESS", Result: "NO_EXCEPTION", CorrelationID: "objmismatch-result-2",
	})
	require.NoError(t, err)
	require.True(t, changed)
}

// TestPgStore_EvaluateSample_RequiresCompleteness is the real proof of
// AUD-NEG-014: an evaluation attempted while any item is still SELECTED
// (no terminal disposition) is reported incomplete and the design never
// reaches EVALUATED.
func TestPgStore_EvaluateSample_RequiresCompleteness(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	_, design := fullSampleDesignSetup(t, s, ctx, "evalcomplete", 10, 3, nil)
	_, items, _, err := s.SelectSample(ctx, domain.SelectSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "evalcomplete-select"})
	require.NoError(t, err)
	require.NotEmpty(t, items)

	eval, _, err := s.EvaluateSample(ctx, domain.EvaluateSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "evalcomplete-eval-1"})
	require.ErrorIs(t, err, domain.ErrSampleEvaluationIncomplete)
	require.NotNil(t, eval)
	require.False(t, eval.CompletenessOK)

	for _, it := range items {
		_, _, err := s.RecordItemResult(ctx, domain.RecordItemResultParams{
			ItemID: it.ItemID, TenantID: testTenant, ActorPrincipalID: "tester-1",
			ObjectiveTested: "TEST_COMPLETENESS", Result: "NO_EXCEPTION", CorrelationID: "evalcomplete-result-" + it.ItemID,
		})
		require.NoError(t, err)
	}

	eval2, changed, err := s.EvaluateSample(ctx, domain.EvaluateSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "evalcomplete-eval-2"})
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, eval2.CompletenessOK)

	updatedDesign, err := s.GetSampleDesign(ctx, testTenant, design.DesignID)
	require.NoError(t, err)
	require.Equal(t, domain.SampleDesignEvaluated, updatedDesign.Status)
}

// TestPgStore_SampleItem_NoSilentReplacement proves "no silent item
// replacement": a raw UPDATE attempting to swap population_row_id on an
// already-selected item is rejected by the trigger.
func TestPgStore_SampleItem_NoSilentReplacement(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop, design := fullSampleDesignSetup(t, s, ctx, "noreplace", 10, 3, nil)
	_, items, _, err := s.SelectSample(ctx, domain.SelectSampleParams{DesignID: design.DesignID, TenantID: testTenant, CorrelationID: "noreplace-select"})
	require.NoError(t, err)
	require.NotEmpty(t, items)

	var anotherRowID string
	err = pool.QueryRow(ctx, `SELECT row_id FROM population_rows WHERE population_id=$1 AND row_id <> $2 LIMIT 1`, pop.PopulationID, items[0].PopulationRowID).Scan(&anotherRowID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE sample_items SET population_row_id=$1 WHERE item_id=$2`, anotherRowID, items[0].ItemID)
	require.Error(t, err, "expected trg_reject_sample_item_replacement to refuse swapping a sample item's own population row")
}
