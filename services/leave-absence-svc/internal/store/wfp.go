package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/leave-absence-svc/internal/domain"
	svcmiddleware "zoiko.io/leave-absence-svc/internal/middleware"
)

// ChangeKind says which command produced a BatchChange.
type ChangeKind string

const (
	ChangeImport     ChangeKind = "IMPORT"
	ChangeFreeze     ChangeKind = "FREEZE"
	ChangeQuarantine ChangeKind = "QUARANTINE_LINE"
	ChangeSupersede  ChangeKind = "SUPERSEDE"
)

// BatchChange is what a committed command changed; the EventBuilder turns it
// into outbox events INSIDE the transaction, so an event exists iff the change does.
type BatchChange struct {
	Kind       ChangeKind
	Batch      *domain.TimeLeaveBatch
	Lines      []domain.TimeLeaveLine // lines created or affected by this command
	Superseded *domain.TimeLeaveBatch // set on ChangeSupersede
	Reason     string
}

type EventBuilder func(BatchChange) ([]domain.OutboxEvent, error)

// BatchResult tells the caller what an import did.
type BatchResult struct {
	Batch   *domain.TimeLeaveBatch
	Lines   []domain.TimeLeaveLine
	Created bool // false: an identical live batch was already on record
}

// SourceChangedError carries the differences found when a batch is checked
// against its source leave. errors.Is(err, domain.ErrSourceChanged) holds.
type SourceChangedError struct{ Issues []domain.SourceIssue }

func (e *SourceChangedError) Error() string        { return domain.ErrSourceChanged.Error() }
func (e *SourceChangedError) Is(target error) bool { return target == domain.ErrSourceChanged }

func tenantOf(ctx context.Context) (string, error) {
	t := svcmiddleware.TenantFromContext(ctx)
	if t == "" {
		return "", domain.ErrIdentityMissing
	}
	return t, nil
}

func enqueue(ctx context.Context, tx pgx.Tx, mk EventBuilder, ch BatchChange) error {
	if mk == nil {
		return nil
	}
	events, err := mk(ch)
	if err != nil {
		return fmt.Errorf("build events: %w", err)
	}
	for _, ev := range events {
		if !domain.EventCatalogue[ev.EventType] {
			return fmt.Errorf("event type %q is not in the WFP-02 catalogue", ev.EventType)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO wfp_outbox (tenant_id, object_id, event_type, payload) VALUES ($1, $2, $3, $4)`,
			ev.TenantID, ev.ObjectID, ev.EventType, ev.Payload); err != nil {
			return fmt.Errorf("enqueue %s: %w", ev.EventType, err)
		}
	}
	return nil
}

// ── source leave ─────────────────────────────────────────────────────────────

const sourceCols = `r.request_id::text, r.employee_id::text, r.leave_type_id::text, t.code, t.is_paid,
	r.start_date::text, r.end_date::text, r.total_hours::text, r.status, r.reviewer_id, r.reviewed_at`

func scanSource(row pgx.Row) (domain.SourceLeave, error) {
	var l domain.SourceLeave
	err := row.Scan(&l.RequestID, &l.WorkerID, &l.LeaveTypeID, &l.LeaveTypeCode, &l.IsPaid,
		&l.StartDate, &l.EndDate, &l.Units, &l.Status, &l.ApprovedBy, &l.ApprovedAt)
	return l, err
}

// ListApprovedLeaveForPeriod returns the APPROVED leave of one legal entity that
// touches the period. Hours are read as text from NUMERIC, so no float rounding
// can enter a control total. A request that only partly overlaps the period is
// returned too; classification quarantines it rather than guessing an allocation.
func (s *PgStore) ListApprovedLeaveForPeriod(ctx context.Context, legalEntityID, periodStart, periodEnd string) ([]domain.SourceLeave, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.SourceLeave
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+sourceCols+`
			FROM leave_requests r
			JOIN leave_types t ON t.leave_type_id = r.leave_type_id AND t.tenant_id = r.tenant_id
			WHERE r.tenant_id = $1 AND t.legal_entity_id = $2 AND r.status = 'APPROVED'
			  AND r.start_date <= $4::date AND r.end_date >= $3::date
			ORDER BY r.request_id`, tenantID, legalEntityID, periodStart, periodEnd)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanSource(rows)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// ── batches ──────────────────────────────────────────────────────────────────

const batchCols = `batch_id, tenant_id, legal_entity_id, payroll_period_id, period_start::text, period_end::text,
	source_system_id, batch_version, on_invalid, state, rejection_reason, line_count, accepted_line_count,
	quarantined_line_count, total_units::text, paid_units::text, unpaid_units::text, unit_of_measure, source_hash,
	normalization_version, supersedes_batch_id::text, version, frozen_at, frozen_by, correlation_id, created_by,
	created_at, updated_at`

func scanBatch(row pgx.Row) (*domain.TimeLeaveBatch, error) {
	var b domain.TimeLeaveBatch
	err := row.Scan(&b.BatchID, &b.TenantID, &b.LegalEntityID, &b.PayrollPeriodID, &b.PeriodStart, &b.PeriodEnd,
		&b.SourceSystemID, &b.BatchVersion, &b.OnInvalid, &b.State, &b.RejectionReason, &b.LineCount, &b.AcceptedLineCount,
		&b.QuarantinedLineCount, &b.TotalUnits, &b.PaidUnits, &b.UnpaidUnits, &b.UnitOfMeasure, &b.SourceHash,
		&b.NormalizationVersion, &b.SupersedesBatchID, &b.Version, &b.FrozenAt, &b.FrozenBy, &b.CorrelationID, &b.CreatedBy,
		&b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrBatchNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

const lineCols = `line_id::text, batch_id::text, source_line_id, worker_id::text, leave_type_id::text, leave_type_code,
	is_paid, start_date::text, end_date::text, units::text, unit_of_measure, approved_by, approved_at, state,
	quarantine_reason, line_hash`

func scanLine(row pgx.Row) (*domain.TimeLeaveLine, error) {
	var l domain.TimeLeaveLine
	err := row.Scan(&l.LineID, &l.BatchID, &l.SourceLineID, &l.WorkerID, &l.LeaveTypeID, &l.LeaveTypeCode,
		&l.IsPaid, &l.StartDate, &l.EndDate, &l.Units, &l.UnitOfMeasure, &l.ApprovedBy, &l.ApprovedAt, &l.State,
		&l.QuarantineReason, &l.LineHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrLineNotFound
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func queryLines(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]domain.TimeLeaveLine, error) {
	rows, err := tx.Query(ctx, `SELECT `+lineCols+` FROM wfp_time_leave_lines `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.TimeLeaveLine
	for rows.Next() {
		l, err := scanLine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// finalizeBatch recomputes counts, exact decimal totals and the control hash
// from the batch's lines, then moves it to its resting state. Sums are done in
// SQL (NUMERIC), so no float ever touches a control total.
func finalizeBatch(ctx context.Context, tx pgx.Tx, tenantID, batchID, toState string, rejection *string) (*domain.TimeLeaveBatch, error) {
	accepted, err := queryLines(ctx, tx, `WHERE tenant_id = $1 AND batch_id = $2::uuid AND state = 'ACCEPTED'`, tenantID, batchID)
	if err != nil {
		return nil, err
	}
	hash := domain.SourceHash(accepted)
	return scanBatch(tx.QueryRow(ctx, `
		UPDATE wfp_time_leave_batches b SET
			line_count = c.total, accepted_line_count = c.accepted, quarantined_line_count = c.total - c.accepted,
			total_units = c.units, paid_units = c.paid, unpaid_units = c.units - c.paid,
			source_hash = $3, state = $4, rejection_reason = $5, version = b.version + 1, updated_at = now()
		FROM (SELECT count(*)::int AS total,
		             count(*) FILTER (WHERE state = 'ACCEPTED')::int AS accepted,
		             COALESCE(sum(units) FILTER (WHERE state = 'ACCEPTED'), 0) AS units,
		             COALESCE(sum(units) FILTER (WHERE state = 'ACCEPTED' AND is_paid), 0) AS paid
		      FROM wfp_time_leave_lines WHERE tenant_id = $1 AND batch_id = $2::uuid) c
		WHERE b.tenant_id = $1 AND b.batch_id = $2::uuid
		RETURNING `+batchColsQualified, tenantID, batchID, hash, toState, rejection))
}

// batchColsQualified is batchCols with the table alias the UPDATE ... FROM needs.
const batchColsQualified = `b.batch_id, b.tenant_id, b.legal_entity_id, b.payroll_period_id, b.period_start::text, b.period_end::text,
	b.source_system_id, b.batch_version, b.on_invalid, b.state, b.rejection_reason, b.line_count, b.accepted_line_count,
	b.quarantined_line_count, b.total_units::text, b.paid_units::text, b.unpaid_units::text, b.unit_of_measure, b.source_hash,
	b.normalization_version, b.supersedes_batch_id::text, b.version, b.frozen_at, b.frozen_by, b.correlation_id, b.created_by,
	b.created_at, b.updated_at`

// insertBatchTx creates the batch with its lines and settles it ACCEPTED or
// REJECTED. The caller has already cleared the live-batch slot.
func insertBatchTx(ctx context.Context, tx pgx.Tx, tenantID string, b *domain.TimeLeaveBatch, lines []domain.TimeLeaveLine) (*domain.TimeLeaveBatch, []domain.TimeLeaveLine, error) {
	var nextVersion int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(batch_version), 0) + 1 FROM wfp_time_leave_batches
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND payroll_period_id = $3 AND source_system_id = $4`,
		tenantID, b.LegalEntityID, b.PayrollPeriodID, b.SourceSystemID).Scan(&nextVersion); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO wfp_time_leave_batches
			(batch_id, tenant_id, legal_entity_id, payroll_period_id, period_start, period_end, source_system_id,
			 batch_version, on_invalid, state, normalization_version, supersedes_batch_id, correlation_id, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5::date, $6::date, $7, $8, $9, 'RECEIVED', $10, $11::uuid, $12, $13)`,
		b.BatchID, tenantID, b.LegalEntityID, b.PayrollPeriodID, b.PeriodStart, b.PeriodEnd, b.SourceSystemID,
		nextVersion, b.OnInvalid, b.NormalizationVersion, b.SupersedesBatchID, b.CorrelationID, b.CreatedBy); err != nil {
		return nil, nil, err
	}

	invalid := 0
	for _, l := range lines {
		if l.State == domain.LineQuarantined {
			invalid++
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO wfp_time_leave_lines
				(line_id, tenant_id, batch_id, source_line_id, worker_id, leave_type_id, leave_type_code, is_paid,
				 start_date, end_date, units, unit_of_measure, approved_by, approved_at, state, quarantine_reason, line_hash)
			VALUES ($1::uuid, $2, $3::uuid, $4, $5::uuid, $6::uuid, $7, $8, $9::date, $10::date, $11::numeric, $12, $13, $14, $15, $16, $17)`,
			l.LineID, tenantID, b.BatchID, l.SourceLineID, l.WorkerID, l.LeaveTypeID, l.LeaveTypeCode, l.IsPaid,
			l.StartDate, l.EndDate, l.Units, l.UnitOfMeasure, l.ApprovedBy, l.ApprovedAt, l.State, l.QuarantineReason, l.LineHash); err != nil {
			return nil, nil, fmt.Errorf("insert line %s: %w", l.SourceLineID, err)
		}
	}

	state, rejection := domain.BatchAccepted, (*string)(nil)
	if invalid > 0 && b.OnInvalid == domain.OnInvalidRejectBatch {
		state = domain.BatchRejected
		msg := fmt.Sprintf("INVALID_LINES:%d", invalid)
		rejection = &msg
	}
	stored, err := finalizeBatch(ctx, tx, tenantID, b.BatchID, state, rejection)
	if err != nil {
		return nil, nil, err
	}
	stored2, err := queryLines(ctx, tx, `WHERE tenant_id = $1 AND batch_id = $2::uuid ORDER BY source_line_id`, tenantID, b.BatchID)
	return stored, stored2, err
}

func lockLive(ctx context.Context, tx pgx.Tx, tenantID, entity, period, source string) (*domain.TimeLeaveBatch, error) {
	b, err := scanBatch(tx.QueryRow(ctx, `SELECT `+batchCols+` FROM wfp_time_leave_batches
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND payroll_period_id = $3 AND source_system_id = $4
		  AND state IN ('RECEIVED', 'ACCEPTED', 'FROZEN') FOR UPDATE`, tenantID, entity, period, source))
	if errors.Is(err, domain.ErrBatchNotFound) {
		return nil, nil
	}
	return b, err
}

// ImportTimeLeaveBatch records a snapshot. lines already carry their state
// (ACCEPTED, or QUARANTINED with a reason); nothing here repairs or defaults.
//
// If a live batch already exists for the entity/period/source: an identical
// population (same control hash) is a replay and returns it; a different one is
// ErrLiveBatchExists, because changing a population is SupersedeBatch's job.
func (s *PgStore) ImportTimeLeaveBatch(ctx context.Context, b *domain.TimeLeaveBatch, lines []domain.TimeLeaveLine, mk EventBuilder) (*BatchResult, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var res *BatchResult
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			tenantID+"|"+b.LegalEntityID+"|"+b.PayrollPeriodID+"|"+b.SourceSystemID); err != nil {
			return err
		}
		live, err := lockLive(ctx, tx, tenantID, b.LegalEntityID, b.PayrollPeriodID, b.SourceSystemID)
		if err != nil {
			return err
		}
		if live != nil {
			var accepted []domain.TimeLeaveLine
			for _, l := range lines {
				if l.State == domain.LineAccepted {
					accepted = append(accepted, l)
				}
			}
			if live.SourceHash != nil && *live.SourceHash == domain.SourceHash(accepted) && live.QuarantinedLineCount == countQuarantined(lines) {
				ls, err := queryLines(ctx, tx, `WHERE tenant_id = $1 AND batch_id = $2::uuid ORDER BY source_line_id`, tenantID, live.BatchID)
				if err != nil {
					return err
				}
				res = &BatchResult{Batch: live, Lines: ls}
				return nil
			}
			return domain.ErrLiveBatchExists
		}

		stored, ls, err := insertBatchTx(ctx, tx, tenantID, b, lines)
		if err != nil {
			return err
		}
		if err := enqueue(ctx, tx, mk, BatchChange{Kind: ChangeImport, Batch: stored, Lines: ls}); err != nil {
			return err
		}
		res = &BatchResult{Batch: stored, Lines: ls, Created: true}
		return nil
	})
	return res, err
}

func countQuarantined(lines []domain.TimeLeaveLine) int {
	n := 0
	for _, l := range lines {
		if l.State == domain.LineQuarantined {
			n++
		}
	}
	return n
}

// SupersedeTimeLeaveBatch replaces an ACCEPTED or FROZEN batch with a fresh
// snapshot (a late source change, a correction). The old batch is kept, marked
// SUPERSEDED, and linked; it is never edited. The new batch is built by the
// caller from the CURRENT source.
func (s *PgStore) SupersedeTimeLeaveBatch(ctx context.Context, oldID string, expectedVersion int, nb *domain.TimeLeaveBatch, lines []domain.TimeLeaveLine, reason string, mk EventBuilder) (*BatchResult, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var res *BatchResult
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		old, err := scanBatch(tx.QueryRow(ctx, `SELECT `+batchCols+` FROM wfp_time_leave_batches
			WHERE tenant_id = $1 AND batch_id = $2::uuid FOR UPDATE`, tenantID, oldID))
		if err != nil {
			return err
		}
		if old.Version != expectedVersion {
			return domain.ErrBatchVersionConflict
		}
		if old.State != domain.BatchAccepted && old.State != domain.BatchFrozen {
			return domain.ErrBatchNotSupersedable
		}
		nb.LegalEntityID, nb.PayrollPeriodID, nb.SourceSystemID = old.LegalEntityID, old.PayrollPeriodID, old.SourceSystemID
		nb.PeriodStart, nb.PeriodEnd = old.PeriodStart, old.PeriodEnd
		nb.SupersedesBatchID = &old.BatchID

		superseded, err := scanBatch(tx.QueryRow(ctx, `UPDATE wfp_time_leave_batches
			SET state = 'SUPERSEDED', version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND batch_id = $2::uuid RETURNING `+batchCols, tenantID, oldID))
		if err != nil {
			return err
		}
		stored, ls, err := insertBatchTx(ctx, tx, tenantID, nb, lines)
		if err != nil {
			return err
		}
		if stored.State == domain.BatchRejected {
			return domain.ErrSupersedeWouldReject // rolls back: the old batch stays live
		}
		if err := enqueue(ctx, tx, mk, BatchChange{Kind: ChangeSupersede, Batch: stored, Lines: ls, Superseded: superseded, Reason: reason}); err != nil {
			return err
		}
		res = &BatchResult{Batch: stored, Lines: ls, Created: true}
		return nil
	})
	return res, err
}

// sourceIssues compares every ACCEPTED line with its source request as it is
// now. A batch that no longer matches its source must not be frozen.
func sourceIssues(ctx context.Context, tx pgx.Tx, tenantID, batchID string) ([]domain.SourceIssue, error) {
	lines, err := queryLines(ctx, tx, `WHERE tenant_id = $1 AND batch_id = $2::uuid AND state = 'ACCEPTED' ORDER BY source_line_id`, tenantID, batchID)
	if err != nil {
		return nil, err
	}
	var issues []domain.SourceIssue
	for _, l := range lines {
		src, err := scanSource(tx.QueryRow(ctx, `SELECT `+sourceCols+`
			FROM leave_requests r JOIN leave_types t ON t.leave_type_id = r.leave_type_id AND t.tenant_id = r.tenant_id
			WHERE r.tenant_id = $1 AND r.request_id::text = $2`, tenantID, l.SourceLineID))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			issues = append(issues, domain.SourceIssue{LineID: l.LineID, SourceLineID: l.SourceLineID, Issue: domain.IssueSourceMissing})
		case err != nil:
			return nil, err
		case src.Status != "APPROVED":
			issues = append(issues, domain.SourceIssue{LineID: l.LineID, SourceLineID: l.SourceLineID, Issue: domain.IssueSourceNotApproved})
		case domain.LineHash(src) != l.LineHash:
			issues = append(issues, domain.SourceIssue{LineID: l.LineID, SourceLineID: l.SourceLineID, Issue: domain.IssueSourceChanged})
		}
	}
	return issues, nil
}

// ValidateTimeLeaveBatch re-checks an unfrozen batch against its source and
// changes nothing.
func (s *PgStore) ValidateTimeLeaveBatch(ctx context.Context, batchID string) ([]domain.SourceIssue, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var issues []domain.SourceIssue
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := scanBatch(tx.QueryRow(ctx, `SELECT `+batchCols+` FROM wfp_time_leave_batches
			WHERE tenant_id = $1 AND batch_id = $2::uuid`, tenantID, batchID)); err != nil {
			return err
		}
		var e error
		issues, e = sourceIssues(ctx, tx, tenantID, batchID)
		return e
	})
	return issues, err
}

// FreezeTimeLeaveBatch freezes an ACCEPTED batch so payroll calculates from a
// stable population. It refuses if the source moved since the snapshot.
func (s *PgStore) FreezeTimeLeaveBatch(ctx context.Context, batchID string, expectedVersion int, actor string, mk EventBuilder) (*domain.TimeLeaveBatch, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.TimeLeaveBatch
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanBatch(tx.QueryRow(ctx, `SELECT `+batchCols+` FROM wfp_time_leave_batches
			WHERE tenant_id = $1 AND batch_id = $2::uuid FOR UPDATE`, tenantID, batchID))
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrBatchVersionConflict
		}
		if cur.State != domain.BatchAccepted {
			return domain.ErrBatchNotAccepted
		}
		issues, err := sourceIssues(ctx, tx, tenantID, batchID)
		if err != nil {
			return err
		}
		if len(issues) > 0 {
			return &SourceChangedError{Issues: issues}
		}
		out, err = scanBatch(tx.QueryRow(ctx, `UPDATE wfp_time_leave_batches
			SET state = 'FROZEN', frozen_at = now(), frozen_by = $3, version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND batch_id = $2::uuid RETURNING `+batchCols, tenantID, batchID, actor))
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, mk, BatchChange{Kind: ChangeFreeze, Batch: out})
	})
	return out, err
}

// QuarantineLine removes one ACCEPTED line from an unfrozen batch (the line is
// kept, with its reason, and the control totals and hash are recomputed).
func (s *PgStore) QuarantineLine(ctx context.Context, batchID, lineID string, expectedVersion int, mk EventBuilder) (*domain.TimeLeaveBatch, *domain.TimeLeaveLine, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, nil, err
	}
	var batch *domain.TimeLeaveBatch
	var line *domain.TimeLeaveLine
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanBatch(tx.QueryRow(ctx, `SELECT `+batchCols+` FROM wfp_time_leave_batches
			WHERE tenant_id = $1 AND batch_id = $2::uuid FOR UPDATE`, tenantID, batchID))
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrBatchVersionConflict
		}
		if cur.State != domain.BatchAccepted {
			return domain.ErrBatchNotAccepted
		}
		line, err = scanLine(tx.QueryRow(ctx, `SELECT `+lineCols+` FROM wfp_time_leave_lines
			WHERE tenant_id = $1 AND batch_id = $2::uuid AND line_id = $3::uuid FOR UPDATE`, tenantID, batchID, lineID))
		if err != nil {
			return err
		}
		if line.State != domain.LineAccepted {
			return domain.ErrLineNotAccepted
		}
		line, err = scanLine(tx.QueryRow(ctx, `UPDATE wfp_time_leave_lines
			SET state = 'QUARANTINED', quarantine_reason = $4, updated_at = now()
			WHERE tenant_id = $1 AND batch_id = $2::uuid AND line_id = $3::uuid RETURNING `+lineCols,
			tenantID, batchID, lineID, domain.ReasonManual))
		if err != nil {
			return err
		}
		batch, err = finalizeBatch(ctx, tx, tenantID, batchID, domain.BatchAccepted, nil)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, mk, BatchChange{Kind: ChangeQuarantine, Batch: batch, Lines: []domain.TimeLeaveLine{*line}})
	})
	return batch, line, err
}

// ── queries ──────────────────────────────────────────────────────────────────

func (s *PgStore) GetTimeLeaveBatch(ctx context.Context, id string) (*domain.TimeLeaveBatch, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var b *domain.TimeLeaveBatch
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		b, e = scanBatch(tx.QueryRow(ctx, `SELECT `+batchCols+` FROM wfp_time_leave_batches
			WHERE tenant_id = $1 AND batch_id = $2::uuid`, tenantID, id))
		return e
	})
	return b, err
}

func (s *PgStore) ListTimeLeaveBatches(ctx context.Context, legalEntityID, periodID, state string) ([]domain.TimeLeaveBatch, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.TimeLeaveBatch
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+batchCols+` FROM wfp_time_leave_batches
			WHERE tenant_id = $1 AND ($2 = '' OR legal_entity_id = $2) AND ($3 = '' OR payroll_period_id = $3)
			  AND ($4 = '' OR state = $4)
			ORDER BY payroll_period_id, batch_version LIMIT 500`, tenantID, legalEntityID, periodID, state)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBatch(rows)
			if err != nil {
				return err
			}
			out = append(out, *b)
		}
		return rows.Err()
	})
	return out, err
}

// ListBatchLines returns a batch's lines, optionally only one state / one worker.
func (s *PgStore) ListBatchLines(ctx context.Context, batchID, state, workerID string) ([]domain.TimeLeaveLine, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.TimeLeaveLine
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		out, e = queryLines(ctx, tx, `WHERE tenant_id = $1 AND batch_id = $2::uuid AND ($3 = '' OR state = $3)
			AND ($4 = '' OR worker_id::text = $4) ORDER BY source_line_id`, tenantID, batchID, state, workerID)
		return e
	})
	return out, err
}

// WorkerUnits is one worker's imported units in a batch.
type WorkerUnits struct {
	WorkerID    string `json:"worker_id"`
	TotalUnits  string `json:"total_units"`
	PaidUnits   string `json:"paid_units"`
	UnpaidUnits string `json:"unpaid_units"`
	LineCount   int    `json:"line_count"`
}

// GetWorkerImportedUnits sums ACCEPTED units for one worker, exactly.
func (s *PgStore) GetWorkerImportedUnits(ctx context.Context, batchID, workerID string) (*WorkerUnits, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var w WorkerUnits
	w.WorkerID = workerID
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(sum(units), 0)::text, COALESCE(sum(units) FILTER (WHERE is_paid), 0)::text,
			       COALESCE(sum(units) FILTER (WHERE NOT is_paid), 0)::text, count(*)::int
			FROM wfp_time_leave_lines
			WHERE tenant_id = $1 AND batch_id = $2::uuid AND worker_id = $3::uuid AND state = 'ACCEPTED'`,
			tenantID, batchID, workerID).Scan(&w.TotalUnits, &w.PaidUnits, &w.UnpaidUnits, &w.LineCount)
	})
	return &w, err
}

// ── outbox ───────────────────────────────────────────────────────────────────

type OutboxRecord struct {
	OutboxID  int64
	EventType string
	Key       string
	Body      []byte
}

func (s *PgStore) withRelay(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin relay transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if s.schema != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('search_path', $1, true)", s.schema); err != nil {
			return fmt.Errorf("set search_path: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return fmt.Errorf("set relay context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit relay transaction: %w", err)
	}
	return nil
}

// ClaimOutbox takes up to limit unpublished events, oldest first, locking them
// (FOR UPDATE SKIP LOCKED, so replicas never double-publish) while fn publishes.
// At-least-once: a crash mid-publish rolls the marking back and re-delivers.
func (s *PgStore) ClaimOutbox(ctx context.Context, limit int, fn func([]OutboxRecord) error) error {
	return s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT outbox_id, event_type, object_id, payload FROM wfp_outbox
			WHERE published_at IS NULL ORDER BY created_at, outbox_id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return fmt.Errorf("claim outbox: %w", err)
		}
		var claimed []OutboxRecord
		for rows.Next() {
			var r OutboxRecord
			if err := rows.Scan(&r.OutboxID, &r.EventType, &r.Key, &r.Body); err != nil {
				rows.Close()
				return fmt.Errorf("scan outbox row: %w", err)
			}
			claimed = append(claimed, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read outbox rows: %w", err)
		}
		if len(claimed) == 0 {
			return nil
		}
		ids := make([]int64, 0, len(claimed))
		for _, r := range claimed {
			ids = append(ids, r.OutboxID)
		}
		if err := fn(claimed); err != nil {
			if _, uerr := tx.Exec(ctx, `UPDATE wfp_outbox SET attempts = attempts + 1, last_error = $2
				WHERE outbox_id = ANY($1)`, ids, err.Error()); uerr != nil {
				return fmt.Errorf("record publish failure: %w (original: %v)", uerr, err)
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit publish failure: %w (original: %v)", cerr, err)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE wfp_outbox SET published_at = now() WHERE outbox_id = ANY($1)`, ids); err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		return nil
	})
}

// OutboxDepth reports the unpublished backlog and the age of its oldest entry.
func (s *PgStore) OutboxDepth(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*), min(created_at) FROM wfp_outbox WHERE published_at IS NULL`).Scan(&pending, &oldest); err != nil {
			return fmt.Errorf("outbox depth: %w", err)
		}
		if oldest != nil {
			oldestAge = time.Since(*oldest)
		}
		return nil
	})
	return pending, oldestAge, err
}
