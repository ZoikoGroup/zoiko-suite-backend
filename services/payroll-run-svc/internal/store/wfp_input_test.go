package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

// Real-Postgres tests. The embedded superuser bypasses RLS, so tenant isolation
// proves the explicit tenant_id predicate; database guards use direct SQL.

const (
	inEntity = "le-1"
	inPeriod = "2026-09"
	inStart  = "2026-09-01"
	inEnd    = "2026-09-30"
)

func mapping(kind, code, payroll, category, tax, from string) *domain.InputCodeMapping {
	return &domain.InputCodeMapping{
		MappingID: uuid.NewString(), SourceKind: kind, SourceCode: code, PayrollCode: payroll, Category: category,
		TaxTreatment: tax, Active: true, EffectiveFrom: from, RegisteredBy: "p-1",
	}
}

func mapEvt(tenant string) store.EventBuilder[domain.InputCodeMapping] {
	return func(m *domain.InputCodeMapping) (domain.OutboxEvent, error) {
		return domain.OutboxEvent{TenantID: tenant, ObjectID: m.MappingID, EventType: domain.EventInputMappingRegistered, Payload: []byte(`{}`)}, nil
	}
}

// seedMappings registers the mapping set the builder needs.
func seedMappings(t *testing.T, s *store.PgStore, ctx context.Context) map[string]domain.InputCodeMapping {
	t.Helper()
	for _, m := range []*domain.InputCodeMapping{
		mapping("BASE_SALARY", "BASE", "P-BASE", "EARNING", "TAXABLE", "2026-01-01"),
		mapping("COMP_COMPONENT", "HRA", "P-HRA", "EARNING", "TAXABLE", "2026-01-01"),
		mapping("COMP_BONUS", "ANNUAL", "P-BONUS", "EARNING", "TAXABLE", "2026-01-01"),
		mapping("BENEFIT_EMPLOYEE", "HEALTH_INSURANCE", "P-HEALTH-EE", "DEDUCTION", "PRE_TAX", "2026-01-01"),
		mapping("BENEFIT_EMPLOYER", "HEALTH_INSURANCE", "P-HEALTH-ER", "BENEFIT", "NOT_APPLICABLE", "2026-01-01"),
	} {
		_, err := s.RegisterInputCodeMapping(ctx, m, nil)
		require.NoError(t, err)
	}
	got, err := s.ResolveInputCodeMappings(ctx, inEnd)
	require.NoError(t, err)
	return got
}

func worker(id string, base float64, currency string) domain.WorkerSource {
	by := "mgr-1"
	return domain.WorkerSource{
		WorkerID: id, Context: domain.WorkerContext{Valid: true, PayCurrencies: []string{currency}},
		Base:       &domain.SourceBase{ContractID: "c-" + id[:8], Amount: base, Currency: currency, PayFrequency: "MONTHLY"},
		Components: []domain.SourceComponent{{StructureID: "s-1", Code: "HRA", Name: "House", Type: "EARNING", IsTaxable: true, Amount: 100.10, Currency: currency}},
		Bonuses:    []domain.SourceBonus{{GrantID: "g-" + id[:8], BonusType: "ANNUAL", Amount: 500, Currency: currency, GrantDate: "2026-09-15", ApprovedBy: &by}},
		Elections:  []domain.SourceElection{{ElectionID: "e-" + id[:8], PlanID: "pl-1", PlanType: "HEALTH_INSURANCE", TaxTreatment: "PRE_TAX", Currency: currency, EmployeeAmount: 50.20, EmployerAmount: 120}},
	}
}

func buildLines(ws ...domain.WorkerSource) func(map[string]domain.InputCodeMapping) []domain.InputLine {
	return func(m map[string]domain.InputCodeMapping) []domain.InputLine {
		return domain.BuildInputLines(ws, m, inEnd)
	}
}

func newInBatch(policy string) *domain.InputBatch {
	return &domain.InputBatch{
		BatchID: uuid.NewString(), LegalEntityID: inEntity, PayrollPeriodID: inPeriod, PeriodStart: inStart, PeriodEnd: inEnd,
		OnInvalid: policy, NormalizationVersion: domain.InputNormalizationVersion, CorrelationID: "corr", CreatedBy: "p-1",
	}
}

func inEvents(tenant string) store.InputEventBuilder {
	return func(ch store.InputChange) ([]domain.OutboxEvent, error) {
		mk := func(typ string) domain.OutboxEvent {
			return domain.OutboxEvent{TenantID: tenant, ObjectID: ch.Batch.BatchID, EventType: typ, Payload: []byte(`{}`)}
		}
		switch ch.Kind {
		case store.InputChangeFreeze:
			return []domain.OutboxEvent{mk(domain.EventPayrollInputBatchFrozen)}, nil
		case store.InputChangeCancel:
			return []domain.OutboxEvent{mk(domain.EventInputBatchCancelled)}, nil
		case store.InputChangeConsume:
			return []domain.OutboxEvent{mk(domain.EventInputBatchConsumed)}, nil
		case store.InputChangeSupersede:
			return []domain.OutboxEvent{mk(domain.EventInputSuperseded), mk(domain.EventInputBatchValidated)}, nil
		}
		return []domain.OutboxEvent{mk(domain.EventInputCreated), mk(domain.EventInputBatchValidated)}, nil
	}
}

func inEventTypes(t *testing.T, pool *pgxpool.Pool, tenant string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT event_type FROM wfp_outbox WHERE tenant_id = $1 ORDER BY outbox_id`, tenant)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func newRun(t *testing.T, pool *pgxpool.Pool, tenant, entity, start, end string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO payroll_runs (run_id, tenant_id, legal_entity_id, run_number, pay_period_start, pay_period_end, pay_date, status,
			is_shadow_run, created_at, updated_at, correlation_id)
		VALUES ($1,$2,$3,$4,$5::date,$6::date,$6::date,'INITIATED',false,now(),now(),$7)`,
		id, tenant, entity, "PAY-"+id[:8], start, end, "c-"+id)
	require.NoError(t, err)
	return id
}

func TestWFP03_MappingsAreVersionedAppendOnlyAndResolvedAsOf(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()

	v1 := mapping("COMP_COMPONENT", "HRA", "OLD", "EARNING", "TAXABLE", "2026-01-01")
	created, err := s.RegisterInputCodeMapping(ctx, v1, mapEvt(tenant))
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, 1, v1.MappingVersion)

	// Replay of identical content: same row, no new event.
	again := mapping("COMP_COMPONENT", "HRA", "OLD", "EARNING", "TAXABLE", "2026-01-01")
	created, err = s.RegisterInputCodeMapping(ctx, again, mapEvt(tenant))
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, v1.MappingID, again.MappingID)
	assert.Len(t, inEventTypes(t, pool, tenant), 1)

	// A change must take effect LATER; history is never rewritten.
	_, err = s.RegisterInputCodeMapping(ctx, mapping("COMP_COMPONENT", "HRA", "NEW", "EARNING", "TAXABLE", "2026-01-01"), nil)
	assert.ErrorIs(t, err, domain.ErrMappingRegression)
	_, err = s.RegisterInputCodeMapping(ctx, mapping("COMP_COMPONENT", "HRA", "NEW", "EARNING", "TAXABLE", "2025-12-01"), nil)
	assert.ErrorIs(t, err, domain.ErrMappingRegression)
	v2 := mapping("COMP_COMPONENT", "HRA", "NEW", "EARNING", "TAXABLE", "2026-10-01")
	_, err = s.RegisterInputCodeMapping(ctx, v2, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, v2.MappingVersion)

	// A September period resolves to the mapping in force THEN, even though v2 exists.
	at, err := s.ResolveInputCodeMappings(ctx, "2026-09-30")
	require.NoError(t, err)
	assert.Equal(t, "OLD", at["COMP_COMPONENT|HRA"].PayrollCode)
	at, err = s.ResolveInputCodeMappings(ctx, "2026-10-01")
	require.NoError(t, err)
	assert.Equal(t, "NEW", at["COMP_COMPONENT|HRA"].PayrollCode)
	assert.Equal(t, 2, at["COMP_COMPONENT|HRA"].MappingVersion)
	before, err := s.ResolveInputCodeMappings(ctx, "2025-12-31")
	require.NoError(t, err)
	assert.Empty(t, before, "nothing is mapped before the first effective date")

	// Retiring a code is a new version with active=false, not a delete.
	off := mapping("COMP_COMPONENT", "HRA", "NEW", "EARNING", "TAXABLE", "2026-12-01")
	off.Active = false
	_, err = s.RegisterInputCodeMapping(ctx, off, nil)
	require.NoError(t, err)
	at, _ = s.ResolveInputCodeMappings(ctx, "2026-12-15")
	assert.False(t, at["COMP_COMPONENT|HRA"].Active)

	all, err := s.ListInputCodeMappings(ctx, "COMP_COMPONENT", "HRA")
	require.NoError(t, err)
	assert.Len(t, all, 3)

	// Governed evidence is append-only at the database.
	_, err = pool.Exec(context.Background(), `UPDATE wfp_input_code_mappings SET payroll_code = 'x' WHERE tenant_id = $1`, tenant)
	assert.Error(t, err)
	_, err = pool.Exec(context.Background(), `DELETE FROM wfp_input_code_mappings WHERE tenant_id = $1`, tenant)
	assert.Error(t, err)

	// The same code for another tenant is independent.
	_, ctxB := wfpTenant()
	b, err := s.RegisterInputCodeMapping(ctxB, mapping("COMP_COMPONENT", "HRA", "B-CODE", "EARNING", "TAXABLE", "2026-01-01"), nil)
	require.NoError(t, err)
	assert.True(t, b)
}

func TestWFP03_BatchHasExactControlTotalsPerCategoryAndCurrency(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m := seedMappings(t, s, ctx)

	// Two GBP workers and one USD worker: totals are never summed across currencies.
	w1, w2, w3 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	lines := buildLines(worker(w1, 3000, "GBP"), worker(w2, 2000.55, "GBP"), worker(w3, 4000, "USD"))(m)
	res, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), lines, inEvents(tenant))
	require.NoError(t, err)

	b := res.Batch
	assert.True(t, res.Created)
	assert.Equal(t, domain.InputBatchReady, b.State)
	assert.Equal(t, 3, b.WorkerCount)
	assert.Equal(t, 15, b.LineCount)
	assert.Equal(t, 15, b.AcceptedLineCount)
	require.NotNil(t, b.SourceHash)
	assert.Equal(t, domain.InputSourceHash(lines), *b.SourceHash)

	totals, err := s.ListInputControlTotals(ctx, b.BatchID)
	require.NoError(t, err)
	got := map[string]string{}
	for _, tt := range totals {
		got[tt.Category+"/"+tt.Currency] = tt.TotalAmount
	}
	// GBP earnings: base 3000 + 2000.55, HRA 100.10 x2, bonus 500 x2, computed exactly in SQL.
	assert.Equal(t, "6200.7500", got["EARNING/GBP"], "3000 + 2000.55 + 2x100.10 + 2x500, exact")
	assert.Equal(t, "100.4000", got["DEDUCTION/GBP"], "2 x 50.20")
	assert.Equal(t, "240.0000", got["BENEFIT/GBP"], "2 x 120")
	assert.Equal(t, "4600.1000", got["EARNING/USD"], "4000 + 100.10 + 500")
	assert.Equal(t, "50.2000", got["DEDUCTION/USD"])
	assert.Len(t, got, 6)

	assert.Equal(t, []string{domain.EventInputCreated, domain.EventInputBatchValidated}, inEventTypes(t, pool, tenant))

	// The decimal written is the one a reader sees, and the raw source float is kept.
	ls, err := s.ListInputLines(ctx, b.BatchID, "", w2)
	require.NoError(t, err)
	var base domain.InputLine
	for _, l := range ls {
		if l.SourceKind == domain.SourceBaseSalary {
			base = l
		}
	}
	assert.Equal(t, "2000.5500", base.Amount)
	assert.Equal(t, "2000.55", base.SourceAmountRaw)
	require.NotNil(t, base.MappingVersion)
	assert.Equal(t, 1, *base.MappingVersion)
}

func TestWFP03_ReplayConflictAndPolicy(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m := seedMappings(t, s, ctx)
	w1 := uuid.NewString()

	first, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), buildLines(worker(w1, 3000, "GBP"))(m), inEvents(tenant))
	require.NoError(t, err)

	// A retry of the same pull: the same batch, no duplicate inputs, no new events.
	replay, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), buildLines(worker(w1, 3000, "GBP"))(m), inEvents(tenant))
	require.NoError(t, err)
	assert.False(t, replay.Created)
	assert.Equal(t, first.Batch.BatchID, replay.Batch.BatchID)
	assert.Len(t, inEventTypes(t, pool, tenant), 2)

	// A different population while one is live: supersede is the only path.
	_, err = s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), buildLines(worker(w1, 3500, "GBP"))(m), inEvents(tenant))
	assert.ErrorIs(t, err, domain.ErrInputLiveBatchExists)

	// Two competing live batches are impossible at the database.
	_, err = pool.Exec(context.Background(), `
		INSERT INTO wfp_payroll_input_batches (batch_id, tenant_id, legal_entity_id, payroll_period_id, period_start, period_end,
			batch_version, on_invalid, state, normalization_version, correlation_id, created_by)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 9, 'REJECT_BATCH', 'READY', 'v', 'c', 'p')`, tenant, inEntity, inPeriod, inStart, inEnd)
	assert.Error(t, err)
}

func TestWFP03_InvalidLinesFollowBatchPolicy(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m := seedMappings(t, s, ctx)
	good, bad := uuid.NewString(), uuid.NewString()
	badW := worker(bad, 1000, "GBP")
	badW.Components[0].Code = "MYSTERY" // unmapped
	build := func() []domain.InputLine { return buildLines(worker(good, 3000, "GBP"), badW)(m) }

	rej, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), build(), inEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.InputBatchRejected, rej.Batch.State)
	require.NotNil(t, rej.Batch.RejectionReason)
	assert.Equal(t, "INVALID_LINES:1", *rej.Batch.RejectionReason)
	assert.Equal(t, 1, rej.Batch.QuarantinedLineCount)

	// A rejected batch does not block the next attempt.
	q, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidQuarantineLines), build(), inEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.InputBatchReady, q.Batch.State)
	assert.Equal(t, 2, q.Batch.BatchVersion)
	ex, err := s.ListInputLines(ctx, q.Batch.BatchID, domain.InputLineQuarantined, "")
	require.NoError(t, err)
	require.Len(t, ex, 1)
	assert.Equal(t, domain.InReasonUnmapped, *ex[0].Quarantine)

	// Quarantined lines are outside the control totals.
	totals, _ := s.ListInputControlTotals(ctx, q.Batch.BatchID)
	var earn string
	for _, tt := range totals {
		if tt.Category == "EARNING" {
			earn = tt.TotalAmount
		}
	}
	assert.Equal(t, "5100.1000", earn, "3000 + 100.10 + 500 for the good worker, plus 1000 + 500 for the other; only the unmapped HRA line is excluded")
}

func TestWFP03_FreezeConsumeLifecycleAndGuards(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m := seedMappings(t, s, ctx)
	res, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), buildLines(worker(uuid.NewString(), 3000, "GBP"))(m), inEvents(tenant))
	require.NoError(t, err)
	id, v := res.Batch.BatchID, res.Batch.Version

	runID := newRun(t, pool, tenant, inEntity, inStart, inEnd)
	_, err = s.ConsumeInputBatch(ctx, id, v, runID, nil)
	assert.ErrorIs(t, err, domain.ErrInputBatchNotFrozen, "only a FROZEN batch can be consumed")

	_, err = s.FreezeInputBatch(ctx, id, 99, "p-2", nil)
	assert.ErrorIs(t, err, domain.ErrInputBatchVersion)
	frozen, err := s.FreezeInputBatch(ctx, id, v, "p-2", inEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.InputBatchFrozen, frozen.State)
	require.NotNil(t, frozen.FrozenBy)
	assert.Equal(t, "p-2", *frozen.FrozenBy)
	require.NotNil(t, frozen.FrozenAt)

	_, err = s.FreezeInputBatch(ctx, id, frozen.Version, "p-2", nil)
	assert.ErrorIs(t, err, domain.ErrInputBatchNotReady)
	_, err = s.CancelInputBatch(ctx, id, frozen.Version, "x", nil)
	assert.ErrorIs(t, err, domain.ErrInputBatchNotReady, "a frozen batch cannot be cancelled")

	// A frozen population is immutable at the database.
	for _, stmt := range []string{
		`UPDATE wfp_payroll_input_batches SET line_count = 0, accepted_line_count = 0 WHERE batch_id = $1`,
		`UPDATE wfp_payroll_input_batches SET source_hash = 'x' WHERE batch_id = $1`,
		`UPDATE wfp_payroll_input_batches SET state = 'READY' WHERE batch_id = $1`,
		`UPDATE wfp_payroll_input_batches SET period_end = '2026-10-31' WHERE batch_id = $1`,
		`UPDATE wfp_payroll_input_lines SET amount = 0 WHERE batch_id = $1`,
		`DELETE FROM wfp_payroll_input_lines WHERE batch_id = $1`,
		`UPDATE wfp_payroll_input_totals SET total_amount = 0 WHERE batch_id = $1`,
		`DELETE FROM wfp_payroll_input_batches WHERE batch_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, id)
		assert.Error(t, err, stmt)
	}

	// Consumed by a payroll run for the same entity and period.
	consumed, err := s.ConsumeInputBatch(ctx, id, frozen.Version, runID, inEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.InputBatchConsumed, consumed.State)
	require.NotNil(t, consumed.PayrollRunID)
	assert.Equal(t, runID, *consumed.PayrollRunID)
	require.NotNil(t, consumed.ConsumedAt)

	// CONSUMED ends the line: no supersede, and the database agrees.
	_, err = s.SupersedeInputBatch(ctx, id, consumed.Version, newInBatch(domain.OnInvalidRejectBatch), nil, "late", nil)
	assert.ErrorIs(t, err, domain.ErrInputNotSupersedable)
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payroll_input_batches SET state = 'SUPERSEDED' WHERE batch_id = $1`, id)
	assert.Error(t, err)

	assert.Equal(t, []string{
		domain.EventInputCreated, domain.EventInputBatchValidated, domain.EventPayrollInputBatchFrozen, domain.EventInputBatchConsumed,
	}, inEventTypes(t, pool, tenant))
}

func TestWFP03_OneRunConsumesAtMostOneBatch(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m := seedMappings(t, s, ctx)
	runID := newRun(t, pool, tenant, inEntity, inStart, inEnd)

	freeze := func(period string) *domain.InputBatch {
		b := newInBatch(domain.OnInvalidRejectBatch)
		b.PayrollPeriodID = period
		r, err := s.CreateInputBatch(ctx, b, buildLines(worker(uuid.NewString(), 3000, "GBP"))(m), nil)
		require.NoError(t, err)
		f, err := s.FreezeInputBatch(ctx, r.Batch.BatchID, r.Batch.Version, "p", nil)
		require.NoError(t, err)
		return f
	}
	a, b := freeze("2026-09"), freeze("2026-10")
	_, err := s.ConsumeInputBatch(ctx, a.BatchID, a.Version, runID, nil)
	require.NoError(t, err)
	_, err = s.ConsumeInputBatch(ctx, b.BatchID, b.Version, runID, nil)
	assert.ErrorIs(t, err, domain.ErrRunAlreadyLinked)
}

func TestWFP03_CancelAndSupersede(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m := seedMappings(t, s, ctx)
	w := uuid.NewString()

	first, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), buildLines(worker(w, 3000, "GBP"))(m), inEvents(tenant))
	require.NoError(t, err)

	// Supersede a READY batch with a changed population: old kept, linked, SUPERSEDED.
	sup, err := s.SupersedeInputBatch(ctx, first.Batch.BatchID, first.Batch.Version, newInBatch(domain.OnInvalidRejectBatch),
		buildLines(worker(w, 3500, "GBP"))(m), "salary revised", inEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, 2, sup.Batch.BatchVersion)
	require.NotNil(t, sup.Batch.SupersedesBatchID)
	assert.Equal(t, first.Batch.BatchID, *sup.Batch.SupersedesBatchID)
	old, err := s.GetInputBatch(ctx, first.Batch.BatchID)
	require.NoError(t, err)
	assert.Equal(t, domain.InputBatchSuperseded, old.State)
	oldLines, _ := s.ListInputLines(ctx, old.BatchID, "", "")
	assert.Len(t, oldLines, 5, "the superseded batch keeps exactly what it recorded")

	// A frozen batch is superseded the same way (a controlled delta, never a mutation).
	frozen, err := s.FreezeInputBatch(ctx, sup.Batch.BatchID, sup.Batch.Version, "p", nil)
	require.NoError(t, err)
	delta, err := s.SupersedeInputBatch(ctx, frozen.BatchID, frozen.Version, newInBatch(domain.OnInvalidRejectBatch),
		buildLines(worker(w, 3600, "GBP"))(m), "late change", nil)
	require.NoError(t, err)
	assert.Equal(t, 3, delta.Batch.BatchVersion)

	// A replacement that would be REJECTED leaves the live batch untouched.
	badW := worker(w, 3600, "GBP")
	badW.Components[0].Code = "MYSTERY"
	_, err = s.SupersedeInputBatch(ctx, delta.Batch.BatchID, delta.Batch.Version, newInBatch(domain.OnInvalidRejectBatch), buildLines(badW)(m), "x", nil)
	assert.ErrorIs(t, err, domain.ErrInputSupersedeWouldReject)
	live, _ := s.GetInputBatch(ctx, delta.Batch.BatchID)
	assert.Equal(t, domain.InputBatchReady, live.State, "payroll is never left with no live population")
	all, _ := s.ListInputBatches(ctx, inEntity, inPeriod, "")
	assert.Len(t, all, 3, "the failed supersede left no stray batch")

	// Cancel an unfrozen batch: terminal.
	cancelled, err := s.CancelInputBatch(ctx, delta.Batch.BatchID, delta.Batch.Version, "not needed", inEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.InputBatchCancelled, cancelled.State)
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payroll_input_batches SET state = 'READY' WHERE batch_id = $1`, cancelled.BatchID)
	assert.Error(t, err, "CANCELLED is terminal")
	// ...and frees the slot for a fresh pull.
	fresh, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), buildLines(worker(w, 3600, "GBP"))(m), nil)
	require.NoError(t, err)
	assert.True(t, fresh.Created)
	assert.Equal(t, 4, fresh.Batch.BatchVersion)
}

func TestWFP03_AcceptedLinesMustBeFullyClassifiedAtTheDatabase(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	res, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, domain.InputBatchReady, res.Batch.State)
	assert.Equal(t, 0, res.Batch.LineCount, "an empty population is a valid, reconcilable batch")

	// Lines can only be written while a batch is RECEIVED.
	_, err = pool.Exec(context.Background(), `
		INSERT INTO wfp_payroll_input_lines (line_id, tenant_id, batch_id, worker_id, source_kind, source_line_id, input_code,
			category, amount, currency, effective_date, source_amount_raw, state, line_hash)
		VALUES (gen_random_uuid(), $1, $2, gen_random_uuid(), 'BASE_SALARY', 'x', 'BASE', 'EARNING', 1, 'GBP', '2026-09-30', '1', 'QUARANTINED', 'h')`,
		tenant, res.Batch.BatchID)
	assert.Error(t, err, "no lines can be added to a settled batch")
}

func TestWFP03_BonusIsHeldByOnePeriodOnly(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	_, ctx := wfpTenant()
	m := seedMappings(t, s, ctx)
	w := uuid.NewString()

	sep := newInBatch(domain.OnInvalidRejectBatch)
	r, err := s.CreateInputBatch(ctx, sep, buildLines(worker(w, 3000, "GBP"))(m), nil)
	require.NoError(t, err)

	held, err := s.ListLiveBonusSourceLines(ctx, inEntity, "2026-10")
	require.NoError(t, err)
	assert.Contains(t, held, "BONUS:g-"+w[:8], "another period must not import it again")
	own, err := s.ListLiveBonusSourceLines(ctx, inEntity, inPeriod)
	require.NoError(t, err)
	assert.Empty(t, own, "a batch's own bonuses are not excluded from itself")

	// Once the batch is cancelled the bonus is free again.
	_, err = s.CancelInputBatch(ctx, r.Batch.BatchID, r.Batch.Version, "redo", nil)
	require.NoError(t, err)
	held, _ = s.ListLiveBonusSourceLines(ctx, inEntity, "2026-10")
	assert.Empty(t, held)
}

func TestWFP03_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	_, ctxA := wfpTenant()
	_, ctxB := wfpTenant()
	m := seedMappings(t, s, ctxA)
	res, err := s.CreateInputBatch(ctxA, newInBatch(domain.OnInvalidRejectBatch), buildLines(worker(uuid.NewString(), 3000, "GBP"))(m), nil)
	require.NoError(t, err)

	_, err = s.GetInputBatch(ctxB, res.Batch.BatchID)
	assert.ErrorIs(t, err, domain.ErrInputBatchNotFound)
	bs, _ := s.ListInputBatches(ctxB, inEntity, "", "")
	assert.Empty(t, bs)
	ls, _ := s.ListInputLines(ctxB, res.Batch.BatchID, "", "")
	assert.Empty(t, ls)
	tt, _ := s.ListInputControlTotals(ctxB, res.Batch.BatchID)
	assert.Empty(t, tt)
	mm, _ := s.ResolveInputCodeMappings(ctxB, inEnd)
	assert.Empty(t, mm, "another tenant's mappings are invisible")
	_, err = s.FreezeInputBatch(ctxB, res.Batch.BatchID, 1, "p", nil)
	assert.ErrorIs(t, err, domain.ErrInputBatchNotFound)
	_, err = s.GetInputBatch(context.Background(), res.Batch.BatchID)
	assert.ErrorIs(t, err, domain.ErrIdentityMissing)

	// Tenant B builds its own batch for the same entity/period labels.
	resB, err := s.CreateInputBatch(ctxB, newInBatch(domain.OnInvalidRejectBatch), nil, nil)
	require.NoError(t, err)
	assert.True(t, resB.Created)
}

func TestWFP03_OutboxIsAtomicWithStateChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	m := seedMappings(t, s, ctx)
	lines := func() []domain.InputLine { return buildLines(worker(uuid.NewString(), 3000, "GBP"))(m) }

	_, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), lines(),
		func(store.InputChange) ([]domain.OutboxEvent, error) { return nil, errors.New("boom") })
	require.Error(t, err)
	bs, _ := s.ListInputBatches(ctx, inEntity, "", "")
	assert.Empty(t, bs, "no batch (and no lines) without its event")
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM wfp_payroll_input_lines WHERE tenant_id = $1`, tenant).Scan(&n))
	assert.Zero(t, n)

	_, err = s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), lines(),
		func(ch store.InputChange) ([]domain.OutboxEvent, error) {
			return []domain.OutboxEvent{{TenantID: tenant, ObjectID: ch.Batch.BatchID, EventType: "payroll_paid", Payload: []byte(`{}`)}}, nil
		})
	require.Error(t, err, "an uncatalogued event rolls the change back")
	bs, _ = s.ListInputBatches(ctx, inEntity, "", "")
	assert.Empty(t, bs)

	res, err := s.CreateInputBatch(ctx, newInBatch(domain.OnInvalidRejectBatch), lines(), inEvents(tenant))
	require.NoError(t, err)
	var keys []string
	require.NoError(t, s.ClaimOutbox(ctx, 50, func(recs []store.OutboxRecord) error {
		for _, r := range recs {
			keys = append(keys, r.Key)
		}
		return nil
	}))
	assert.Contains(t, keys, res.Batch.BatchID, "keyed by batch so one batch's events stay ordered")
}
