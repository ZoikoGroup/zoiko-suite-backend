package store_test

import (
	"math/big"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

func TestGetInputBatchByRunIsTheRecordOfWhichFrozenPopulationACalculationUsed(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m := seedMappings(t, s, ctx)
	res, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), buildLines(worker(uuid.NewString(), 3000, "GBP"))(m), inEvents(tenant))
	require.NoError(t, err)
	runID := newRun(t, pool, tenant, inEntity, inStart, inEnd)

	none, err := s.GetInputBatchByRun(ctx, runID)
	require.NoError(t, err)
	assert.Nil(t, none, "a run that consumed nothing has no batch")

	frozen, err := s.FreezeInputBatch(ctx, res.Batch.BatchID, res.Batch.Version, "p-2", inEvents(tenant))
	require.NoError(t, err)
	none, err = s.GetInputBatchByRun(ctx, runID)
	require.NoError(t, err)
	assert.Nil(t, none, "freezing is not consuming")

	consumed, err := s.ConsumeInputBatch(ctx, res.Batch.BatchID, frozen.Version, runID, inEvents(tenant))
	require.NoError(t, err)
	got, err := s.GetInputBatchByRun(ctx, runID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, consumed.BatchID, got.BatchID)
	assert.Equal(t, domain.InputBatchConsumed, got.State)

	other := newRun(t, pool, tenant, inEntity, inStart, inEnd)
	none, err = s.GetInputBatchByRun(ctx, other)
	require.NoError(t, err)
	assert.Nil(t, none, "another run did not consume it")

	otherTenant, otherCtx := wfpTenant()
	_ = otherTenant
	none, err = s.GetInputBatchByRun(otherCtx, runID)
	require.NoError(t, err)
	assert.Nil(t, none, "another tenant cannot see it")

	lines, err := s.ListInputLines(ctx, got.BatchID, domain.InputLineAccepted, "")
	require.NoError(t, err)
	assert.NotEmpty(t, lines)
	pay, err := domain.BuildBatchPay(lines)
	require.NoError(t, err)
	require.Len(t, pay, 1)
	earned := new(big.Rat)
	for _, l := range lines {
		if l.Category == domain.CategoryEarning {
			a, perr := domain.ParseDecimal(l.Amount)
			require.NoError(t, perr)
			earned.Add(earned, a)
		}
	}
	f, _ := earned.Float64()
	for _, p := range pay {
		assert.InDelta(t, f, p.Gross, 0.0051, "the stored batch lines derive exactly the worker's gross")
		assert.Greater(t, p.Gross, 3000.0, "base salary and the components are all in it")
		assert.Equal(t, "GBP", p.Currency)
	}
}
