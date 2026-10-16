package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

func wfpPop(t *testing.T, s *store.PgStore, tenant, population, measure string, period *time.Time) *domain.ControlPopulationPage {
	t.Helper()
	page, err := s.ControlPopulation(context.Background(), domain.ControlPopulationQuery{
		TenantID: tenant, LegalEntityID: "le-1", Population: population, Measure: measure, PeriodStart: period, Limit: 100,
	})
	require.NoError(t, err)
	return page
}

func TestWave8_WFPControlPopulationsPairOneRecordPerAcceptedResult(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	res, d := reconciled(t, s, ctx, tenant)
	rid := res.Result.ResultID

	results := wfpPop(t, s, tenant, domain.PopulationWFPResults, "gross", nil)
	require.Len(t, results.Records, 1)
	assert.Equal(t, rid, results.Records[0].Reference, "records are paired by the result id")
	assert.Equal(t, "3100.5000", results.Records[0].Amount, "exact, never rounded")
	assert.Equal(t, "GBP", results.Records[0].Currency)
	assert.Equal(t, "2026-09-30", results.Records[0].Date)
	assert.Equal(t, map[string]string{"GBP": "3100.5000"}, results.DeclaredTotals.Totals)
	assert.EqualValues(t, 1, results.DeclaredTotals.RowCount)
	assert.NotEmpty(t, results.Watermark)

	j := wfpPop(t, s, tenant, domain.PopulationWFPJournals, "gross", nil)
	require.Len(t, j.Records, 1)
	assert.Equal(t, rid, j.Records[0].Reference)
	assert.Equal(t, "3100.5000", j.Records[0].Amount, "the gross expense debit of the posted journal")
	assert.Equal(t, "POSTED", j.Records[0].Attributes["state"])
	assert.NotEqual(t, rid, j.Records[0].RecordID, "a journal record is the subject's own id")

	netR := wfpPop(t, s, tenant, domain.PopulationWFPResults, "net", nil)
	netJ := wfpPop(t, s, tenant, domain.PopulationWFPJournals, "net", nil)
	netI := wfpPop(t, s, tenant, domain.PopulationWFPNetInstructions, "net", nil)
	for name, p := range map[string]*domain.ControlPopulationPage{"results": netR, "journals": netJ, "instructions": netI} {
		require.Len(t, p.Records, 1, name)
		assert.Equal(t, rid, p.Records[0].Reference, name)
		assert.Equal(t, "2400.4000", p.Records[0].Amount, name)
	}
	assert.Empty(t, wfpPop(t, s, tenant, domain.PopulationWFPNetInstructions, "gross", nil).Records, "instructions are a net-pay population only")

	// Period is the pay date's month, exactly.
	sep := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	assert.Len(t, wfpPop(t, s, tenant, domain.PopulationWFPResults, "net", &sep).Records, 1)
	assert.Empty(t, wfpPop(t, s, tenant, domain.PopulationWFPResults, "net", &oct).Records)

	// A cancelled instruction no longer stands for a payment: the comparison now shows a missing record.
	_, err := s.CancelPaymentInstruction(ctx, d.InstructionID, d.Version, "x", payEvents(tenant))
	require.NoError(t, err)
	assert.Empty(t, wfpPop(t, s, tenant, domain.PopulationWFPNetInstructions, "net", nil).Records)
	assert.Len(t, wfpPop(t, s, tenant, domain.PopulationWFPResults, "net", nil).Records, 1, "the accepted result is still owed")

	// Another tenant sees none of it.
	other, _ := wfpTenant()
	for _, p := range []string{domain.PopulationWFPResults, domain.PopulationWFPJournals, domain.PopulationWFPNetInstructions} {
		assert.Empty(t, wfpPop(t, s, other, p, "net", nil).Records, p)
	}

	// A correction supersedes the result: its results and journal leave the populations.
	acceptedResult(t, s, ctx, tenant, res.Ref.RuleManifestID, "RUN-1", 2)
	after := wfpPop(t, s, tenant, domain.PopulationWFPResults, "gross", nil)
	require.Len(t, after.Records, 1)
	assert.NotEqual(t, rid, after.Records[0].Reference, "only the accepted correction is payroll's position")
	assert.Empty(t, wfpPop(t, s, tenant, domain.PopulationWFPJournals, "gross", nil).Records, "the superseded result's journal is not the replacement's journal")
}

func TestWave8_WFPPopulationWatermarkChangesWhenTheDataDoes(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	res, d := reconciled(t, s, ctx, tenant)
	before := wfpPop(t, s, tenant, domain.PopulationWFPNetInstructions, "net", nil)
	again := wfpPop(t, s, tenant, domain.PopulationWFPNetInstructions, "net", nil)
	assert.Equal(t, before.Watermark, again.Watermark, "the same data has the same watermark")

	_, err := s.CancelPaymentInstruction(ctx, d.InstructionID, d.Version, "x", payEvents(tenant))
	require.NoError(t, err)
	gone := wfpPop(t, s, tenant, domain.PopulationWFPNetInstructions, "net", nil)
	assert.NotEqual(t, before.Watermark, gone.Watermark, "an instruction leaving the population changes it")

	netPayDraft(t, s, ctx, tenant, res)
	back := wfpPop(t, s, tenant, domain.PopulationWFPNetInstructions, "net", nil)
	assert.Equal(t, before.Watermark, back.Watermark, "the population is per result, so a replacement for the same net pay is the same population")
}
