package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

func newManual(worker string) *domain.ManualInput {
	return &domain.ManualInput{
		ManualID: uuid.NewString(), LegalEntityID: inEntity, PayrollPeriodID: inPeriod, WorkerID: worker, InputCode: "SPOT-AWARD",
		Category: domain.CategoryEarning, Amount: "250.0000", Currency: "GBP", EffectiveDate: "2026-09-20",
		Reason: "spot award", EvidenceRef: "TICKET-4411", ProposedBy: "maker", CorrelationID: "corr",
	}
}

func TestManual_FourEyesIsEnforcedByTheStoreAndByTheDatabase(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m := newManual(uuid.NewString())
	require.NoError(t, s.ProposeManualInput(ctx, m, nil))
	assert.Equal(t, domain.ManualProposed, m.State)

	_, err := s.DecideManualInput(ctx, m.ManualID, domain.ManualApproved, "maker", "", nil)
	assert.ErrorIs(t, err, domain.ErrManualFourEyes, "the maker cannot approve")

	// The database refuses it too, whatever the application does.
	tx, err := pool.Begin(context.Background())
	require.NoError(t, err)
	_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, tenant)
	require.NoError(t, err)
	_, err = tx.Exec(context.Background(), `UPDATE wfp_manual_inputs SET state = 'APPROVED', decided_by = proposed_by, decided_at = now() WHERE manual_id = $1::uuid`, m.ManualID)
	assert.Error(t, err, "the CHECK constraint refuses the maker as the checker")
	_ = tx.Rollback(context.Background())

	got, err := s.DecideManualInput(ctx, m.ManualID, domain.ManualApproved, "checker", "looks right", nil)
	require.NoError(t, err)
	assert.Equal(t, domain.ManualApproved, got.State)
	require.NotNil(t, got.DecidedBy)
	assert.Equal(t, "checker", *got.DecidedBy)

	_, err = s.DecideManualInput(ctx, m.ManualID, domain.ManualRejected, "someone-else", "x", nil)
	assert.ErrorIs(t, err, domain.ErrManualInputNotPending, "a decision is final")
}

func TestManual_AnApprovedInputIsImmutableAndNeverDeleted(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m := newManual(uuid.NewString())
	require.NoError(t, s.ProposeManualInput(ctx, m, nil))
	_, err := s.DecideManualInput(ctx, m.ManualID, domain.ManualApproved, "checker", "", nil)
	require.NoError(t, err)

	for name, stmt := range map[string]string{
		"edit amount":    `UPDATE wfp_manual_inputs SET amount = 9999 WHERE manual_id = $1::uuid`,
		"edit worker":    `UPDATE wfp_manual_inputs SET worker_id = gen_random_uuid() WHERE manual_id = $1::uuid`,
		"flip decision":  `UPDATE wfp_manual_inputs SET state = 'REJECTED' WHERE manual_id = $1::uuid`,
		"change checker": `UPDATE wfp_manual_inputs SET decided_by = 'someone' WHERE manual_id = $1::uuid`,
		"delete":         `DELETE FROM wfp_manual_inputs WHERE manual_id = $1::uuid`,
	} {
		tx, err := pool.Begin(context.Background())
		require.NoError(t, err, name)
		_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, tenant)
		require.NoError(t, err, name)
		_, err = tx.Exec(context.Background(), stmt, m.ManualID)
		assert.Error(t, err, name)
		_ = tx.Rollback(context.Background())
	}
	got, err := s.GetManualInput(ctx, m.ManualID)
	require.NoError(t, err)
	assert.Equal(t, "250.0000", got.Amount)
}

func TestManual_OnlyTheMakerWithdrawsAndOnlyWhileProposed(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	_, ctx := wfpTenant()
	m := newManual(uuid.NewString())
	require.NoError(t, s.ProposeManualInput(ctx, m, nil))
	_, err := s.DecideManualInput(ctx, m.ManualID, domain.ManualWithdrawn, "checker", "", nil)
	assert.ErrorIs(t, err, domain.ErrManualOnlyMakerWithdraws)
	got, err := s.DecideManualInput(ctx, m.ManualID, domain.ManualWithdrawn, "maker", "wrong worker", nil)
	require.NoError(t, err)
	assert.Equal(t, domain.ManualWithdrawn, got.State)
	_, err = s.DecideManualInput(ctx, m.ManualID, domain.ManualApproved, "checker", "", nil)
	assert.ErrorIs(t, err, domain.ErrManualInputNotPending)
	_, err = s.GetManualInput(ctx, uuid.NewString())
	assert.ErrorIs(t, err, domain.ErrManualInputNotFound)
}

func TestManual_TenantsNeverSeeEachOthersInputs(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	_, ctx := wfpTenant()
	_, other := wfpTenant()
	m := newManual(uuid.NewString())
	require.NoError(t, s.ProposeManualInput(ctx, m, nil))
	_, err := s.GetManualInput(other, m.ManualID)
	assert.ErrorIs(t, err, domain.ErrManualInputNotFound)
	list, err := s.ListManualInputs(other, inEntity, "", "")
	require.NoError(t, err)
	assert.Empty(t, list)
	_, err = s.DecideManualInput(other, m.ManualID, domain.ManualApproved, "checker", "", nil)
	assert.ErrorIs(t, err, domain.ErrManualInputNotFound)
}

func TestManual_ProposalAndDecisionEventsAreInTheSameTransaction(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mk := func(ch store.ManualChange) (domain.OutboxEvent, error) {
		typ := domain.EventManualInputProposed
		if ch.Kind == "APPROVE" {
			typ = domain.EventManualInputApproved
		}
		return domain.OutboxEvent{TenantID: tenant, ObjectID: ch.Input.ManualID, EventType: typ, Payload: []byte(`{}`)}, nil
	}
	m := newManual(uuid.NewString())
	require.NoError(t, s.ProposeManualInput(ctx, m, mk))
	_, err := s.DecideManualInput(ctx, m.ManualID, domain.ManualApproved, "checker", "", mk)
	require.NoError(t, err)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM wfp_outbox WHERE object_id = $1::text`, m.ManualID).Scan(&n))
	assert.Equal(t, 2, n)

	bad := func(store.ManualChange) (domain.OutboxEvent, error) {
		return domain.OutboxEvent{TenantID: tenant, ObjectID: "x", EventType: "not_in_catalogue", Payload: []byte(`{}`)}, nil
	}
	m2 := newManual(uuid.NewString())
	assert.Error(t, s.ProposeManualInput(ctx, m2, bad), "an event outside the catalogue fails the whole change")
	_, err = s.GetManualInput(ctx, m2.ManualID)
	assert.ErrorIs(t, err, domain.ErrManualInputNotFound, "no event, no change")
}

// An approved manual input reaches pay only through a batch: classified by the MANUAL mapping, frozen, and then
// derived into the worker pay exactly like every other line.
func TestManual_AnApprovedInputFlowsThroughAFrozenBatchIntoWorkerPay(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m0 := seedMappings(t, s, ctx)
	for _, mm := range []*domain.InputCodeMapping{
		mapping("MANUAL", "SPOT-AWARD", "P-SPOT", "EARNING", "TAXABLE", "2026-01-01"),
		mapping("MANUAL", "ADV-RECOVERY", "P-ADV", "DEDUCTION", "POST_TAX", "2026-01-01"),
	} {
		_, err := s.RegisterInputCodeMapping(ctx, mm, nil)
		require.NoError(t, err, "the MANUAL source kind is accepted by the mapping table")
	}
	_ = m0
	maps, err := s.ResolveInputCodeMappings(ctx, inEnd)
	require.NoError(t, err)

	wid := uuid.NewString()
	earning := newManual(wid)
	deduction := newManual(wid)
	deduction.InputCode, deduction.Category, deduction.Amount = "ADV-RECOVERY", domain.CategoryDeduction, "75.5000"
	for _, m := range []*domain.ManualInput{earning, deduction} {
		require.NoError(t, s.ProposeManualInput(ctx, m, nil))
		_, err := s.DecideManualInput(ctx, m.ManualID, domain.ManualApproved, "checker", "", nil)
		require.NoError(t, err)
	}
	pending := newManual(wid)
	require.NoError(t, s.ProposeManualInput(ctx, pending, nil))

	approved, err := s.ListApprovedManualInputs(ctx, inEntity, inPeriod)
	require.NoError(t, err)
	require.Len(t, approved, 2, "a PROPOSED input is not pulled")

	src := worker(wid, 3000, "GBP")
	for _, a := range approved {
		src.Manuals = append(src.Manuals, domain.SourceManualInput{ManualID: a.ManualID, Code: a.InputCode, Category: a.Category, Amount: mustFloat(t, a.Amount),
			Currency: a.Currency, EffectiveDate: a.EffectiveDate, Reason: a.Reason, EvidenceRef: a.EvidenceRef, ApprovedBy: *a.DecidedBy})
	}
	lines := domain.BuildInputLines([]domain.WorkerSource{src}, maps, inEnd)
	res, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), lines, inEvents(tenant))
	require.NoError(t, err, "MANUAL lines are accepted by the line table")
	assert.Equal(t, domain.InputBatchReady, res.Batch.State)

	stored, err := s.ListInputLines(ctx, res.Batch.BatchID, domain.InputLineAccepted, wid)
	require.NoError(t, err)
	pay, err := domain.BuildBatchPay(stored)
	require.NoError(t, err)
	base, err := domain.BuildBatchPay(domain.BuildInputLines([]domain.WorkerSource{worker(wid, 3000, "GBP")}, maps, inEnd))
	require.NoError(t, err)
	assert.InDelta(t, base[wid].Gross+250, pay[wid].Gross, 0.005, "the approved earning is added to gross")
	assert.InDelta(t, base[wid].Deductions+75.5, pay[wid].Deductions, 0.005, "the approved deduction is added to deductions")
	assert.InDelta(t, base[wid].Taxable+250, pay[wid].Taxable, 0.005, "the taxable earning is taxable; the POST_TAX deduction does not reduce it")
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	r, err := domain.ParseDecimal(s)
	require.NoError(t, err)
	f, _ := r.Float64()
	return f
}
