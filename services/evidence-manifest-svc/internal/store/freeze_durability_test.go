package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/store"
)

// buildValidating drives a population to VALIDATING with the given asserted
// control totals (the real sum of the two rows is 300, count 2).
func buildValidating(t *testing.T, s *store.PgStore, ctx context.Context, tag string, totals map[string]float64) *domain.AuditPopulation {
	t.Helper()
	pop, _, err := s.DefinePopulation(ctx, definePopulationParams(tag+"-define"))
	require.NoError(t, err)
	_, _, err = s.BuildPopulation(ctx, domain.BuildPopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: tag + "-build",
		Rows: []domain.BuildPopulationRow{
			{SourceRecordID: "JE-1", Amount: floatPtr(100), RowSnapshot: []byte(`{"je":"1"}`)},
			{SourceRecordID: "JE-2", Amount: floatPtr(200), RowSnapshot: []byte(`{"je":"2"}`)},
		},
	})
	require.NoError(t, err)
	_, _, err = s.ValidatePopulation(ctx, domain.ValidatePopulationParams{
		PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: tag + "-validate", ControlTotals: totals,
	})
	require.NoError(t, err)
	return pop
}

// Regression: the quarantine UPDATE used to be rolled back because the
// mismatch error was returned from inside the transaction. The API said
// QUARANTINED while the row stayed VALIDATING. The check below reads the
// population back through a separate call, so only a COMMITTED quarantine
// passes.
func TestFreezePopulation_Mismatch_QuarantineIsPersisted(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop := buildValidating(t, s, ctx, "qdur", map[string]float64{"record_count": 2, "sum_amount": 999})

	returned, changed, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "qdur-freeze"})
	require.ErrorIs(t, err, domain.ErrPopulationControlTotalMismatch)
	require.True(t, changed)
	require.Equal(t, domain.PopulationQuarantined, returned.Status)

	persisted, err := s.GetAuditPopulation(ctx, testTenant, pop.PopulationID)
	require.NoError(t, err)
	assert.Equal(t, domain.PopulationQuarantined, persisted.Status, "the quarantine must survive in the database, not just in the response")
	require.NotNil(t, persisted.QuarantineReason)
	assert.Contains(t, *persisted.QuarantineReason, "sum_amount")
	assert.Nil(t, persisted.FrozenAt)

	// And it is a real terminal-ish state: freezing again is refused, not
	// silently retried into FROZEN.
	_, _, err = s.FreezePopulation(ctx, domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "qdur-freeze-2"})
	require.ErrorIs(t, err, domain.ErrPopulationInvalidState)
}

func TestFreezePopulation_NoControlTotals_QuarantineIsPersisted(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	ctx := tenantCtx(testTenant)

	pop := buildValidating(t, s, ctx, "qnone", map[string]float64{})

	_, _, err := s.FreezePopulation(ctx, domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: testTenant, CorrelationID: "qnone-freeze"})
	require.ErrorIs(t, err, domain.ErrPopulationControlTotalMismatch)

	persisted, err := s.GetAuditPopulation(ctx, testTenant, pop.PopulationID)
	require.NoError(t, err)
	assert.Equal(t, domain.PopulationQuarantined, persisted.Status)
	require.NotNil(t, persisted.QuarantineReason)
	assert.Contains(t, *persisted.QuarantineReason, "no control totals")
}
