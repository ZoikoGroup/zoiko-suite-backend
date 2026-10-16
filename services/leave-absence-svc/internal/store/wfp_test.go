package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/leave-absence-svc/internal/domain"
	"zoiko.io/leave-absence-svc/internal/middleware"
	"zoiko.io/leave-absence-svc/internal/store"
)

// Real-Postgres tests. The embedded superuser bypasses RLS, so tenant isolation
// proves the explicit tenant_id predicate; database guards use direct SQL.

const (
	entity      = "le-1"
	periodStart = "2026-09-01"
	periodEnd   = "2026-09-30"
)

func tctx() (string, context.Context) {
	id := uuid.NewString()
	return id, middleware.WithTenant(context.Background(), id)
}

func addType(t *testing.T, pool *pgxpool.Pool, tenant, ent, code string, paid bool) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO leave_types (leave_type_id, tenant_id, legal_entity_id, name, code, is_paid, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$4,$5,'ACTIVE',now(),now())`, id, tenant, ent, code, paid)
	require.NoError(t, err)
	return id
}

type leaveOpts struct {
	status, start, end, hours string
	reviewer                  *string
	reviewedAt                *time.Time
}

func addLeave(t *testing.T, pool *pgxpool.Pool, tenant, worker, leaveType string, o leaveOpts) string {
	t.Helper()
	if o.status == "" {
		o.status = "APPROVED"
	}
	if o.start == "" {
		o.start, o.end = "2026-09-10", "2026-09-11"
	}
	if o.hours == "" {
		o.hours = "16.00"
	}
	if o.status == "APPROVED" && o.reviewer == nil {
		r := "mgr-1"
		o.reviewer = &r
	}
	if o.status == "APPROVED" && o.reviewedAt == nil {
		n := time.Date(2026, 9, 1, 9, 0, 0, 123456000, time.UTC)
		o.reviewedAt = &n
	}
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO leave_requests (request_id, tenant_id, employee_id, leave_type_id, start_date, end_date, total_hours,
			status, reviewer_id, reviewed_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5::date,$6::date,$7::numeric,$8,$9,$10,now(),now())`,
		id, tenant, worker, leaveType, o.start, o.end, o.hours, o.status, o.reviewer, o.reviewedAt)
	require.NoError(t, err)
	return id
}

// snapshot builds batch lines the way the handler does, from the live source.
func snapshot(t *testing.T, s *store.PgStore, ctx context.Context, workerReason map[string]string) ([]domain.TimeLeaveLine, []domain.SourceLeave) {
	t.Helper()
	src, err := s.ListApprovedLeaveForPeriod(ctx, entity, periodStart, periodEnd)
	require.NoError(t, err)
	var lines []domain.TimeLeaveLine
	for _, sl := range src {
		reason := domain.ClassifyLine(sl, periodStart, periodEnd, workerReason[sl.WorkerID])
		l := domain.TimeLeaveLine{
			LineID: uuid.NewString(), SourceLineID: sl.RequestID, WorkerID: sl.WorkerID, LeaveTypeID: sl.LeaveTypeID,
			LeaveTypeCode: sl.LeaveTypeCode, IsPaid: sl.IsPaid, StartDate: sl.StartDate, EndDate: sl.EndDate, Units: sl.Units,
			UnitOfMeasure: domain.UnitHours, ApprovedBy: sl.ApprovedBy, ApprovedAt: sl.ApprovedAt,
			State: domain.LineAccepted, LineHash: domain.LineHash(sl),
		}
		if reason != "" {
			l.State, l.QuarantineReason = domain.LineQuarantined, &reason
		}
		lines = append(lines, l)
	}
	return lines, src
}

func newBatch(onInvalid string) *domain.TimeLeaveBatch {
	return &domain.TimeLeaveBatch{
		BatchID: uuid.NewString(), LegalEntityID: entity, PayrollPeriodID: "2026-09", PeriodStart: periodStart,
		PeriodEnd: periodEnd, SourceSystemID: domain.SourceLeaveAbsence, OnInvalid: onInvalid,
		NormalizationVersion: domain.NormalizationVersion, CorrelationID: "corr", CreatedBy: "p-1",
	}
}

func evs(tenant string) store.EventBuilder {
	return func(ch store.BatchChange) ([]domain.OutboxEvent, error) {
		mk := func(typ string) domain.OutboxEvent {
			return domain.OutboxEvent{TenantID: tenant, ObjectID: ch.Batch.BatchID, EventType: typ, Payload: []byte(`{}`)}
		}
		switch ch.Kind {
		case store.ChangeFreeze:
			return []domain.OutboxEvent{mk(domain.EventBatchFrozen)}, nil
		case store.ChangeQuarantine:
			return []domain.OutboxEvent{mk(domain.EventLineQuarantined)}, nil
		case store.ChangeSupersede:
			return []domain.OutboxEvent{mk(domain.EventBatchSuperseded), mk(domain.EventBatchAccepted)}, nil
		}
		return []domain.OutboxEvent{mk(domain.EventBatchReceived), mk(domain.EventBatchAccepted)}, nil
	}
}

func eventTypes(t *testing.T, pool *pgxpool.Pool, tenant string) []string {
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

func TestWFP02_ImportBuildsExactControlTotals(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tctx()
	paid, unpaid := addType(t, pool, tenant, entity, "VACATION", true), addType(t, pool, tenant, entity, "UNPAID", false)
	w1, w2 := uuid.NewString(), uuid.NewString()
	// Amounts a float would drift on: 0.10 + 0.20 must be exactly 0.30.
	addLeave(t, pool, tenant, w1, paid, leaveOpts{hours: "0.10"})
	addLeave(t, pool, tenant, w1, paid, leaveOpts{hours: "0.20", start: "2026-09-14", end: "2026-09-14"})
	addLeave(t, pool, tenant, w2, unpaid, leaveOpts{hours: "8.05", start: "2026-09-21", end: "2026-09-21"})
	// Not approved: never enters the population.
	addLeave(t, pool, tenant, w2, paid, leaveOpts{status: "SUBMITTED", hours: "99.00"})
	// Another entity's leave type: out of scope.
	other := addType(t, pool, tenant, "le-OTHER", "VACATION", true)
	addLeave(t, pool, tenant, w1, other, leaveOpts{hours: "40.00"})

	lines, _ := snapshot(t, s, ctx, nil)
	res, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), lines, evs(tenant))
	require.NoError(t, err)
	b := res.Batch
	assert.True(t, res.Created)
	assert.Equal(t, domain.BatchAccepted, b.State)
	assert.Equal(t, 3, b.LineCount)
	assert.Equal(t, 3, b.AcceptedLineCount)
	assert.Equal(t, "8.35", b.TotalUnits)
	assert.Equal(t, "0.30", b.PaidUnits)
	assert.Equal(t, "8.05", b.UnpaidUnits)
	require.NotNil(t, b.SourceHash)
	assert.Len(t, *b.SourceHash, 64)

	units, err := s.GetWorkerImportedUnits(ctx, b.BatchID, w1)
	require.NoError(t, err)
	assert.Equal(t, "0.30", units.TotalUnits)
	assert.Equal(t, 2, units.LineCount)
	assert.Equal(t, []string{domain.EventBatchReceived, domain.EventBatchAccepted}, eventTypes(t, pool, tenant))
}

func TestWFP02_ReplayIsIdempotentAndChangedPopulationNeedsSupersede(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tctx()
	typ := addType(t, pool, tenant, entity, "VACATION", true)
	w := uuid.NewString()
	addLeave(t, pool, tenant, w, typ, leaveOpts{})

	lines, _ := snapshot(t, s, ctx, nil)
	first, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), lines, evs(tenant))
	require.NoError(t, err)

	// Same population again (a retry): the same batch, no duplicate units, no new events.
	again, _ := snapshot(t, s, ctx, nil)
	replay, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), again, evs(tenant))
	require.NoError(t, err)
	assert.False(t, replay.Created)
	assert.Equal(t, first.Batch.BatchID, replay.Batch.BatchID)
	assert.Equal(t, "16.00", replay.Batch.TotalUnits, "a replayed source line never doubles units")
	assert.Len(t, eventTypes(t, pool, tenant), 2)

	// The population changes; a plain import is refused, supersede is the path.
	addLeave(t, pool, tenant, w, typ, leaveOpts{start: "2026-09-22", end: "2026-09-22", hours: "8.00"})
	changed, _ := snapshot(t, s, ctx, nil)
	_, err = s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), changed, evs(tenant))
	assert.ErrorIs(t, err, domain.ErrLiveBatchExists)
}

func TestWFP02_InvalidLinesFollowBatchPolicy(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tctx()
	typ := addType(t, pool, tenant, entity, "VACATION", true)
	good, bad, noEvidence, crossing := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	addLeave(t, pool, tenant, good, typ, leaveOpts{hours: "8.00"})
	addLeave(t, pool, tenant, bad, typ, leaveOpts{hours: "8.00", start: "2026-09-02", end: "2026-09-02"})
	blank := " "
	addLeave(t, pool, tenant, noEvidence, typ, leaveOpts{hours: "8.00", reviewer: &blank, start: "2026-09-03", end: "2026-09-03"})
	addLeave(t, pool, tenant, crossing, typ, leaveOpts{hours: "24.00", start: "2026-09-29", end: "2026-10-02"})
	reasons := map[string]string{bad: "NO_ACTIVE_PROFILE"}

	lines, _ := snapshot(t, s, ctx, reasons)
	got := map[string]string{}
	for _, l := range lines {
		if l.QuarantineReason != nil {
			got[l.WorkerID] = *l.QuarantineReason
		}
	}
	assert.Equal(t, map[string]string{
		bad: "NO_ACTIVE_PROFILE", noEvidence: domain.ReasonMissingApprovalEvidence, crossing: domain.ReasonCrossPeriod,
	}, got, "nothing is repaired or allocated by guesswork")

	// REJECT_BATCH: the whole batch is rejected, evidence retained, nothing live.
	rej, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), lines, evs(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.BatchRejected, rej.Batch.State)
	require.NotNil(t, rej.Batch.RejectionReason)
	assert.Equal(t, "INVALID_LINES:3", *rej.Batch.RejectionReason)
	assert.Len(t, rej.Lines, 4)

	// A rejected batch does not block the next attempt; QUARANTINE_LINES keeps the valid ones.
	retry, _ := snapshot(t, s, ctx, reasons)
	q, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidQuarantineLines), retry, evs(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.BatchAccepted, q.Batch.State)
	assert.Equal(t, 2, q.Batch.BatchVersion)
	assert.Equal(t, 1, q.Batch.AcceptedLineCount)
	assert.Equal(t, 3, q.Batch.QuarantinedLineCount)
	assert.Equal(t, "8.00", q.Batch.TotalUnits)
	ex, err := s.ListBatchLines(ctx, q.Batch.BatchID, domain.LineQuarantined, "")
	require.NoError(t, err)
	assert.Len(t, ex, 3)
}

func TestWFP02_FreezeRefusesWhenSourceMovedAndSupersedeKeepsHistory(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tctx()
	typ := addType(t, pool, tenant, entity, "VACATION", true)
	w := uuid.NewString()
	req1 := addLeave(t, pool, tenant, w, typ, leaveOpts{hours: "8.00"})
	addLeave(t, pool, tenant, w, typ, leaveOpts{hours: "8.00", start: "2026-09-15", end: "2026-09-15"})

	lines, _ := snapshot(t, s, ctx, nil)
	imp, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), lines, evs(tenant))
	require.NoError(t, err)

	issues, err := s.ValidateTimeLeaveBatch(ctx, imp.Batch.BatchID)
	require.NoError(t, err)
	assert.Empty(t, issues)

	// An approval is revoked at the source after the snapshot (approved -> revoked).
	_, err = pool.Exec(context.Background(), `UPDATE leave_requests SET status = 'CANCELLED' WHERE request_id = $1`, req1)
	require.NoError(t, err)
	issues, err = s.ValidateTimeLeaveBatch(ctx, imp.Batch.BatchID)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, domain.IssueSourceNotApproved, issues[0].Issue)

	_, err = s.FreezeTimeLeaveBatch(ctx, imp.Batch.BatchID, imp.Batch.Version, "p-1", evs(tenant))
	var changed *store.SourceChangedError
	require.ErrorAs(t, err, &changed)
	assert.ErrorIs(t, err, domain.ErrSourceChanged)
	still, _ := s.GetTimeLeaveBatch(ctx, imp.Batch.BatchID)
	assert.Equal(t, domain.BatchAccepted, still.State, "a refused freeze changes nothing")

	// The supported path: supersede with a fresh snapshot of the current source.
	fresh, _ := snapshot(t, s, ctx, nil)
	nb := newBatch(domain.OnInvalidRejectBatch)
	sup, err := s.SupersedeTimeLeaveBatch(ctx, imp.Batch.BatchID, imp.Batch.Version, nb, fresh, "approval revoked", evs(tenant))
	require.NoError(t, err)
	assert.Equal(t, 2, sup.Batch.BatchVersion)
	assert.Equal(t, "8.00", sup.Batch.TotalUnits)
	require.NotNil(t, sup.Batch.SupersedesBatchID)
	assert.Equal(t, imp.Batch.BatchID, *sup.Batch.SupersedesBatchID)
	old, err := s.GetTimeLeaveBatch(ctx, imp.Batch.BatchID)
	require.NoError(t, err)
	assert.Equal(t, domain.BatchSuperseded, old.State)
	assert.Equal(t, "16.00", old.TotalUnits, "the superseded batch keeps what it recorded")

	// Now it freezes, and a FROZEN batch is immutable.
	frozen, err := s.FreezeTimeLeaveBatch(ctx, sup.Batch.BatchID, sup.Batch.Version, "p-2", evs(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.BatchFrozen, frozen.State)
	require.NotNil(t, frozen.FrozenBy)
	assert.Equal(t, "p-2", *frozen.FrozenBy)

	_, err = s.FreezeTimeLeaveBatch(ctx, sup.Batch.BatchID, frozen.Version, "p-2", nil)
	assert.ErrorIs(t, err, domain.ErrBatchNotAccepted)
	for _, stmt := range []string{
		`UPDATE wfp_time_leave_batches SET total_units = 0 WHERE batch_id = $1`,
		`UPDATE wfp_time_leave_batches SET state = 'ACCEPTED' WHERE batch_id = $1`,
		`UPDATE wfp_time_leave_lines SET units = 0 WHERE batch_id = $1`,
		`UPDATE wfp_time_leave_lines SET state = 'QUARANTINED', quarantine_reason = 'x' WHERE batch_id = $1`,
		`DELETE FROM wfp_time_leave_lines WHERE batch_id = $1`,
		`DELETE FROM wfp_time_leave_batches WHERE batch_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, sup.Batch.BatchID)
		assert.Error(t, err, stmt)
	}

	// A late record after freeze is a controlled delta: supersede the FROZEN batch.
	addLeave(t, pool, tenant, w, typ, leaveOpts{hours: "4.00", start: "2026-09-25", end: "2026-09-25"})
	late, _ := snapshot(t, s, ctx, nil)
	delta, err := s.SupersedeTimeLeaveBatch(ctx, sup.Batch.BatchID, frozen.Version, newBatch(domain.OnInvalidRejectBatch), late, "late leave", evs(tenant))
	require.NoError(t, err)
	assert.Equal(t, "12.00", delta.Batch.TotalUnits)
	assert.Equal(t, 3, delta.Batch.BatchVersion)
	prev, _ := s.GetTimeLeaveBatch(ctx, sup.Batch.BatchID)
	assert.Equal(t, domain.BatchSuperseded, prev.State)
	assert.Equal(t, "8.00", prev.TotalUnits, "the frozen population was never mutated")
}

func TestWFP02_SupersedeThatWouldBeRejectedLeavesOldBatchLive(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tctx()
	typ := addType(t, pool, tenant, entity, "VACATION", true)
	w := uuid.NewString()
	addLeave(t, pool, tenant, w, typ, leaveOpts{})
	lines, _ := snapshot(t, s, ctx, nil)
	imp, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), lines, nil)
	require.NoError(t, err)

	// The source now yields an invalid line.
	bad := uuid.NewString()
	addLeave(t, pool, tenant, bad, typ, leaveOpts{start: "2026-09-03", end: "2026-09-03"})
	next, _ := snapshot(t, s, ctx, map[string]string{bad: "WORKER_NOT_FOUND"})

	_, err = s.SupersedeTimeLeaveBatch(ctx, imp.Batch.BatchID, imp.Batch.Version, newBatch(domain.OnInvalidRejectBatch), next, "x", nil)
	assert.ErrorIs(t, err, domain.ErrSupersedeWouldReject)
	live, err := s.GetTimeLeaveBatch(ctx, imp.Batch.BatchID)
	require.NoError(t, err)
	assert.Equal(t, domain.BatchAccepted, live.State, "payroll must never be left with no live population")
	all, _ := s.ListTimeLeaveBatches(ctx, entity, "2026-09", "")
	assert.Len(t, all, 1, "the failed supersede left no stray batch")
}

func TestWFP02_ManualQuarantineRecomputesTotalsAndIsVersionChecked(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tctx()
	typ := addType(t, pool, tenant, entity, "VACATION", true)
	w := uuid.NewString()
	addLeave(t, pool, tenant, w, typ, leaveOpts{hours: "8.00"})
	addLeave(t, pool, tenant, w, typ, leaveOpts{hours: "2.50", start: "2026-09-16", end: "2026-09-16"})
	lines, _ := snapshot(t, s, ctx, nil)
	imp, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidQuarantineLines), lines, nil)
	require.NoError(t, err)
	before := *imp.Batch.SourceHash

	var target domain.TimeLeaveLine
	for _, l := range imp.Lines {
		if l.Units == "2.50" {
			target = l
		}
	}
	_, _, err = s.QuarantineLine(ctx, imp.Batch.BatchID, target.LineID, 99, nil)
	assert.ErrorIs(t, err, domain.ErrBatchVersionConflict)

	b, l, err := s.QuarantineLine(ctx, imp.Batch.BatchID, target.LineID, imp.Batch.Version, evs(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.LineQuarantined, l.State)
	assert.Equal(t, domain.ReasonManual, *l.QuarantineReason)
	assert.Equal(t, "8.00", b.TotalUnits)
	assert.Equal(t, 1, b.QuarantinedLineCount)
	assert.NotEqual(t, before, *b.SourceHash, "the control hash follows the population")

	_, _, err = s.QuarantineLine(ctx, imp.Batch.BatchID, target.LineID, b.Version, nil)
	assert.ErrorIs(t, err, domain.ErrLineNotAccepted)
	// A quarantined line cannot be quietly un-quarantined.
	_, err = pool.Exec(context.Background(), `UPDATE wfp_time_leave_lines SET state = 'ACCEPTED', quarantine_reason = NULL WHERE line_id = $1`, target.LineID)
	assert.Error(t, err)
}

func TestWFP02_OneLiveBatchPerPeriodAtTheDatabase(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tctx()
	res, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, domain.BatchAccepted, res.Batch.State)
	assert.Equal(t, "0.00", res.Batch.TotalUnits, "an empty population is a valid, reconcilable batch")

	_, err = pool.Exec(context.Background(), `
		INSERT INTO wfp_time_leave_batches (batch_id, tenant_id, legal_entity_id, payroll_period_id, period_start, period_end,
			source_system_id, batch_version, on_invalid, state, normalization_version, correlation_id, created_by)
		VALUES (gen_random_uuid(), $1, $2, '2026-09', '2026-09-01', '2026-09-30', $3, 9, 'REJECT_BATCH', 'ACCEPTED', 'v', 'c', 'p')`,
		tenant, entity, domain.SourceLeaveAbsence)
	assert.Error(t, err, "two live populations for one period can never coexist")
}

func TestWFP02_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenantA, ctxA := tctx()
	_, ctxB := tctx()
	typ := addType(t, pool, tenantA, entity, "VACATION", true)
	w := uuid.NewString()
	addLeave(t, pool, tenantA, w, typ, leaveOpts{})
	lines, _ := snapshot(t, s, ctxA, nil)
	res, err := s.ImportTimeLeaveBatch(ctxA, newBatch(domain.OnInvalidRejectBatch), lines, nil)
	require.NoError(t, err)

	_, err = s.GetTimeLeaveBatch(ctxB, res.Batch.BatchID)
	assert.ErrorIs(t, err, domain.ErrBatchNotFound)
	bs, err := s.ListTimeLeaveBatches(ctxB, entity, "", "")
	require.NoError(t, err)
	assert.Empty(t, bs)
	src, err := s.ListApprovedLeaveForPeriod(ctxB, entity, periodStart, periodEnd)
	require.NoError(t, err)
	assert.Empty(t, src, "another tenant's leave is invisible")
	_, err = s.FreezeTimeLeaveBatch(ctxB, res.Batch.BatchID, 1, "p", nil)
	assert.ErrorIs(t, err, domain.ErrBatchNotFound)
	_, err = s.GetTimeLeaveBatch(context.Background(), res.Batch.BatchID)
	assert.ErrorIs(t, err, domain.ErrIdentityMissing)

	// Tenant B can run its own batch for the same entity/period labels.
	resB, err := s.ImportTimeLeaveBatch(ctxB, newBatch(domain.OnInvalidRejectBatch), nil, nil)
	require.NoError(t, err)
	assert.True(t, resB.Created)
}

func TestWFP02_OutboxIsAtomicWithStateChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tctx()

	_, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), nil,
		func(store.BatchChange) ([]domain.OutboxEvent, error) { return nil, errors.New("boom") })
	require.Error(t, err)
	bs, _ := s.ListTimeLeaveBatches(ctx, entity, "", "")
	assert.Empty(t, bs, "no batch without its event")

	_, err = s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), nil,
		func(ch store.BatchChange) ([]domain.OutboxEvent, error) {
			return []domain.OutboxEvent{{TenantID: tenant, ObjectID: ch.Batch.BatchID, EventType: "leave_paid", Payload: []byte(`{}`)}}, nil
		})
	require.Error(t, err, "an uncatalogued event rolls the change back")
	bs, _ = s.ListTimeLeaveBatches(ctx, entity, "", "")
	assert.Empty(t, bs)

	res, err := s.ImportTimeLeaveBatch(ctx, newBatch(domain.OnInvalidRejectBatch), nil, evs(tenant))
	require.NoError(t, err)
	var keys []string
	require.NoError(t, s.ClaimOutbox(ctx, 50, func(recs []store.OutboxRecord) error {
		for _, r := range recs {
			keys = append(keys, r.Key)
		}
		return nil
	}))
	assert.Contains(t, keys, res.Batch.BatchID, "keyed by batch so one batch's events stay ordered")

	require.NoError(t, s.ClaimOutbox(ctx, 50, func([]store.OutboxRecord) error { return nil }))
	var unpublished int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM wfp_outbox WHERE tenant_id = $1 AND published_at IS NULL`, tenant).Scan(&unpublished))
	assert.Zero(t, unpublished)
}
