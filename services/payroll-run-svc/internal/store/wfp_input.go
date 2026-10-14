package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// InputChangeKind says which command produced an InputChange.
type InputChangeKind string

const (
	InputChangeCreate    InputChangeKind = "CREATE"
	InputChangeFreeze    InputChangeKind = "FREEZE"
	InputChangeSupersede InputChangeKind = "SUPERSEDE"
	InputChangeCancel    InputChangeKind = "CANCEL"
	InputChangeConsume   InputChangeKind = "CONSUME"
)

// InputChange is what a committed command changed. The builder turns it into
// outbox events INSIDE the transaction, so an event exists iff the change does.
type InputChange struct {
	Kind       InputChangeKind
	Batch      *domain.InputBatch
	Lines      []domain.InputLine
	Superseded *domain.InputBatch
	Reason     string
}

type InputEventBuilder func(InputChange) ([]domain.OutboxEvent, error)

// InputBatchResult tells the caller what a create/supersede did.
type InputBatchResult struct {
	Batch   *domain.InputBatch
	Lines   []domain.InputLine
	Created bool // false: an identical live batch was already on record
}

func enqueueMany(ctx context.Context, tx pgx.Tx, mk InputEventBuilder, ch InputChange) error {
	if mk == nil {
		return nil
	}
	events, err := mk(ch)
	if err != nil {
		return fmt.Errorf("build events: %w", err)
	}
	for _, ev := range events {
		if !domain.EventCatalogue[ev.EventType] {
			return fmt.Errorf("event type %q is not in the WFP catalogue", ev.EventType)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO wfp_outbox (tenant_id, object_id, event_type, payload) VALUES ($1, $2, $3, $4)`,
			ev.TenantID, ev.ObjectID, ev.EventType, ev.Payload); err != nil {
			return fmt.Errorf("enqueue %s: %w", ev.EventType, err)
		}
	}
	return nil
}

// ── code mappings ────────────────────────────────────────────────────────────

const mappingCols = `mapping_id, tenant_id, source_kind, source_code, mapping_version, payroll_code, category,
	tax_treatment, active, effective_from::text, registered_by, created_at`

func scanMapping(row pgx.Row) (*domain.InputCodeMapping, error) {
	var m domain.InputCodeMapping
	err := row.Scan(&m.MappingID, &m.TenantID, &m.SourceKind, &m.SourceCode, &m.MappingVersion, &m.PayrollCode, &m.Category,
		&m.TaxTreatment, &m.Active, &m.EffectiveFrom, &m.RegisteredBy, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMappingNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// RegisterInputCodeMapping appends the next version of a source code's mapping.
// The same content as the latest version is a replay; a change must take effect
// later than the latest one (history is never rewritten).
func (s *PgStore) RegisterInputCodeMapping(ctx context.Context, m *domain.InputCodeMapping, mk EventBuilder[domain.InputCodeMapping]) (created bool, err error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return false, err
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			tenantID+"|"+m.SourceKind+"|"+m.SourceCode); err != nil {
			return err
		}
		latest, err := scanMapping(tx.QueryRow(ctx, `SELECT `+mappingCols+` FROM wfp_input_code_mappings
			WHERE tenant_id = $1 AND source_kind = $2 AND source_code = $3 ORDER BY mapping_version DESC LIMIT 1`,
			tenantID, m.SourceKind, m.SourceCode))
		version := 1
		if err == nil {
			if latest.PayrollCode == m.PayrollCode && latest.Category == m.Category && latest.TaxTreatment == m.TaxTreatment &&
				latest.Active == m.Active && latest.EffectiveFrom == m.EffectiveFrom {
				*m = *latest
				return nil
			}
			if m.EffectiveFrom <= latest.EffectiveFrom {
				return domain.ErrMappingRegression
			}
			version = latest.MappingVersion + 1
		} else if !errors.Is(err, domain.ErrMappingNotFound) {
			return err
		}
		stored, err := scanMapping(tx.QueryRow(ctx, `
			INSERT INTO wfp_input_code_mappings (mapping_id, tenant_id, source_kind, source_code, mapping_version,
				payroll_code, category, tax_treatment, active, effective_from, registered_by)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10::date, $11) RETURNING `+mappingCols,
			m.MappingID, tenantID, m.SourceKind, m.SourceCode, version, m.PayrollCode, m.Category, m.TaxTreatment,
			m.Active, m.EffectiveFrom, m.RegisteredBy))
		if err != nil {
			return err
		}
		created = true
		if err := enqueue(ctx, tx, stored, mk); err != nil {
			return err
		}
		*m = *stored
		return nil
	})
	return created, err
}

// ListInputCodeMappings returns every version, oldest first, optionally for one source.
func (s *PgStore) ListInputCodeMappings(ctx context.Context, kind, code string) ([]domain.InputCodeMapping, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.InputCodeMapping
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+mappingCols+` FROM wfp_input_code_mappings
			WHERE tenant_id = $1 AND ($2 = '' OR source_kind = $2) AND ($3 = '' OR source_code = $3)
			ORDER BY source_kind, source_code, mapping_version LIMIT 2000`, tenantID, kind, code)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMapping(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	return out, err
}

// ResolveInputCodeMappings returns, for every source code, the mapping version in
// force on asOf (the latest effective_from not after it). Rows with active=false
// are returned so the caller can tell "unmapped on purpose" from "never mapped".
func (s *PgStore) ResolveInputCodeMappings(ctx context.Context, asOf string) (map[string]domain.InputCodeMapping, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]domain.InputCodeMapping{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT ON (source_kind, source_code) `+mappingCols+`
			FROM wfp_input_code_mappings WHERE tenant_id = $1 AND effective_from <= $2::date
			ORDER BY source_kind, source_code, effective_from DESC`, tenantID, asOf)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMapping(rows)
			if err != nil {
				return err
			}
			out[domain.MappingKey(m.SourceKind, m.SourceCode)] = *m
		}
		return rows.Err()
	})
	return out, err
}

// ── batches ──────────────────────────────────────────────────────────────────

const inBatchCols = `batch_id, tenant_id, legal_entity_id, payroll_period_id, period_start::text, period_end::text,
	batch_version, on_invalid, state, rejection_reason, worker_count, line_count, accepted_line_count,
	quarantined_line_count, source_hash, normalization_version, supersedes_batch_id::text, payroll_run_id::text, version,
	frozen_at, frozen_by, consumed_at, correlation_id, created_by, created_at, updated_at`

func scanInBatch(row pgx.Row) (*domain.InputBatch, error) {
	var b domain.InputBatch
	err := row.Scan(&b.BatchID, &b.TenantID, &b.LegalEntityID, &b.PayrollPeriodID, &b.PeriodStart, &b.PeriodEnd,
		&b.BatchVersion, &b.OnInvalid, &b.State, &b.RejectionReason, &b.WorkerCount, &b.LineCount, &b.AcceptedLineCount,
		&b.QuarantinedLineCount, &b.SourceHash, &b.NormalizationVersion, &b.SupersedesBatchID, &b.PayrollRunID, &b.Version,
		&b.FrozenAt, &b.FrozenBy, &b.ConsumedAt, &b.CorrelationID, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrInputBatchNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

const inLineCols = `line_id::text, batch_id::text, worker_id::text, source_kind, source_line_id, source_ref, input_code,
	payroll_code, category, tax_treatment, mapping_id::text, mapping_version, amount::text, currency, effective_date::text,
	approval_ref, reason, source_amount_raw, state, quarantine_reason, line_hash, created_at`

func scanInLine(row pgx.Row) (*domain.InputLine, error) {
	var l domain.InputLine
	err := row.Scan(&l.LineID, &l.BatchID, &l.WorkerID, &l.SourceKind, &l.SourceLineID, &l.SourceRef, &l.InputCode,
		&l.PayrollCode, &l.Category, &l.TaxTreatment, &l.MappingID, &l.MappingVersion, &l.Amount, &l.Currency, &l.EffectiveDate,
		&l.ApprovalRef, &l.Reason, &l.SourceAmountRaw, &l.State, &l.Quarantine, &l.LineHash, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func queryInLines(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]domain.InputLine, error) {
	rows, err := tx.Query(ctx, `SELECT `+inLineCols+` FROM wfp_payroll_input_lines `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.InputLine
	for rows.Next() {
		l, err := scanInLine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

func lockInLive(ctx context.Context, tx pgx.Tx, tenantID, entity, period string) (*domain.InputBatch, error) {
	b, err := scanInBatch(tx.QueryRow(ctx, `SELECT `+inBatchCols+` FROM wfp_payroll_input_batches
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND payroll_period_id = $3
		  AND state IN ('RECEIVED', 'READY', 'FROZEN', 'CONSUMED') FOR UPDATE`, tenantID, entity, period))
	if errors.Is(err, domain.ErrInputBatchNotFound) {
		return nil, nil
	}
	return b, err
}

// insertInBatchTx writes the batch (RECEIVED), its lines and control totals, then
// settles it READY or REJECTED. Sums are done in SQL on NUMERIC, so no float
// touches a control total.
func insertInBatchTx(ctx context.Context, tx pgx.Tx, tenantID string, b *domain.InputBatch, lines []domain.InputLine) (*domain.InputBatch, []domain.InputLine, error) {
	var nextVersion int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(batch_version), 0) + 1 FROM wfp_payroll_input_batches
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND payroll_period_id = $3`,
		tenantID, b.LegalEntityID, b.PayrollPeriodID).Scan(&nextVersion); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO wfp_payroll_input_batches
			(batch_id, tenant_id, legal_entity_id, payroll_period_id, period_start, period_end, batch_version, on_invalid,
			 state, normalization_version, supersedes_batch_id, correlation_id, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5::date, $6::date, $7, $8, 'RECEIVED', $9, $10::uuid, $11, $12)`,
		b.BatchID, tenantID, b.LegalEntityID, b.PayrollPeriodID, b.PeriodStart, b.PeriodEnd, nextVersion, b.OnInvalid,
		b.NormalizationVersion, b.SupersedesBatchID, b.CorrelationID, b.CreatedBy); err != nil {
		return nil, nil, err
	}

	invalid, workers := 0, map[string]bool{}
	for _, l := range lines {
		workers[l.WorkerID] = true
		if l.State == domain.InputLineQuarantined {
			invalid++
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO wfp_payroll_input_lines
				(line_id, tenant_id, batch_id, worker_id, source_kind, source_line_id, source_ref, input_code, payroll_code,
				 category, tax_treatment, mapping_id, mapping_version, amount, currency, effective_date, approval_ref, reason,
				 source_amount_raw, state, quarantine_reason, line_hash)
			VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12::uuid, $13, $14::numeric, $15,
			        $16::date, $17, $18, $19, $20, $21, $22)`,
			l.LineID, tenantID, b.BatchID, l.WorkerID, l.SourceKind, l.SourceLineID, l.SourceRef, l.InputCode, l.PayrollCode,
			l.Category, l.TaxTreatment, l.MappingID, l.MappingVersion, l.Amount, l.Currency, l.EffectiveDate, l.ApprovalRef,
			l.Reason, l.SourceAmountRaw, l.State, l.Quarantine, l.LineHash); err != nil {
			return nil, nil, fmt.Errorf("insert line %s: %w", l.SourceLineID, err)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO wfp_payroll_input_totals (tenant_id, batch_id, category, currency, line_count, total_amount)
		SELECT tenant_id, batch_id, category, currency, count(*)::int, sum(amount)
		FROM wfp_payroll_input_lines WHERE tenant_id = $1 AND batch_id = $2::uuid AND state = 'ACCEPTED'
		GROUP BY tenant_id, batch_id, category, currency`, tenantID, b.BatchID); err != nil {
		return nil, nil, fmt.Errorf("control totals: %w", err)
	}

	state, rejection := domain.InputBatchReady, (*string)(nil)
	if invalid > 0 && b.OnInvalid == domain.OnInvalidRejectBatch {
		state = domain.InputBatchRejected
		msg := fmt.Sprintf("INVALID_LINES:%d", invalid)
		rejection = &msg
	}
	stored, err := scanInBatch(tx.QueryRow(ctx, `
		UPDATE wfp_payroll_input_batches SET
			worker_count = $3, line_count = $4, accepted_line_count = $5, quarantined_line_count = $6,
			source_hash = $7, state = $8, rejection_reason = $9, version = version + 1, updated_at = now()
		WHERE tenant_id = $1 AND batch_id = $2::uuid RETURNING `+inBatchCols,
		tenantID, b.BatchID, len(workers), len(lines), len(lines)-invalid, invalid, domain.InputSourceHash(lines), state, rejection))
	if err != nil {
		return nil, nil, err
	}
	stored2, err := queryInLines(ctx, tx, `WHERE tenant_id = $1 AND batch_id = $2::uuid ORDER BY source_line_id`, tenantID, b.BatchID)
	return stored, stored2, err
}

func countInQuarantined(lines []domain.InputLine) int {
	n := 0
	for _, l := range lines {
		if l.State == domain.InputLineQuarantined {
			n++
		}
	}
	return n
}

// CreateInputBatch records a pulled snapshot. If a live batch already exists for
// the entity and period, an identical population is a replay (the same batch is
// returned); a different one is ErrInputLiveBatchExists, because changing a
// population is SupersedeBatch's job.
func (s *PgStore) CreateInputBatch(ctx context.Context, b *domain.InputBatch, lines []domain.InputLine, mk InputEventBuilder) (*InputBatchResult, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var res *InputBatchResult
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			tenantID+"|input|"+b.LegalEntityID+"|"+b.PayrollPeriodID); err != nil {
			return err
		}
		live, err := lockInLive(ctx, tx, tenantID, b.LegalEntityID, b.PayrollPeriodID)
		if err != nil {
			return err
		}
		if live != nil {
			if live.SourceHash != nil && *live.SourceHash == domain.InputSourceHash(lines) && live.QuarantinedLineCount == countInQuarantined(lines) {
				ls, err := queryInLines(ctx, tx, `WHERE tenant_id = $1 AND batch_id = $2::uuid ORDER BY source_line_id`, tenantID, live.BatchID)
				if err != nil {
					return err
				}
				res = &InputBatchResult{Batch: live, Lines: ls}
				return nil
			}
			return domain.ErrInputLiveBatchExists
		}
		stored, ls, err := insertInBatchTx(ctx, tx, tenantID, b, lines)
		if err != nil {
			return err
		}
		if err := enqueueMany(ctx, tx, mk, InputChange{Kind: InputChangeCreate, Batch: stored, Lines: ls}); err != nil {
			return err
		}
		res = &InputBatchResult{Batch: stored, Lines: ls, Created: true}
		return nil
	})
	return res, err
}

// SupersedeInputBatch replaces a READY or FROZEN batch with a fresh snapshot.
// The old batch is kept, marked SUPERSEDED and linked; if the replacement would
// be REJECTED the whole transaction rolls back and the old batch stays live.
func (s *PgStore) SupersedeInputBatch(ctx context.Context, oldID string, expectedVersion int, nb *domain.InputBatch, lines []domain.InputLine, reason string, mk InputEventBuilder) (*InputBatchResult, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var res *InputBatchResult
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		old, err := scanInBatch(tx.QueryRow(ctx, `SELECT `+inBatchCols+` FROM wfp_payroll_input_batches
			WHERE tenant_id = $1 AND batch_id = $2::uuid FOR UPDATE`, tenantID, oldID))
		if err != nil {
			return err
		}
		if old.Version != expectedVersion {
			return domain.ErrInputBatchVersion
		}
		if old.State != domain.InputBatchReady && old.State != domain.InputBatchFrozen {
			return domain.ErrInputNotSupersedable
		}
		nb.LegalEntityID, nb.PayrollPeriodID, nb.PeriodStart, nb.PeriodEnd = old.LegalEntityID, old.PayrollPeriodID, old.PeriodStart, old.PeriodEnd
		nb.SupersedesBatchID = &old.BatchID

		superseded, err := scanInBatch(tx.QueryRow(ctx, `UPDATE wfp_payroll_input_batches
			SET state = 'SUPERSEDED', version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND batch_id = $2::uuid RETURNING `+inBatchCols, tenantID, oldID))
		if err != nil {
			return err
		}
		stored, ls, err := insertInBatchTx(ctx, tx, tenantID, nb, lines)
		if err != nil {
			return err
		}
		if stored.State == domain.InputBatchRejected {
			return domain.ErrInputSupersedeWouldReject
		}
		if err := enqueueMany(ctx, tx, mk, InputChange{Kind: InputChangeSupersede, Batch: stored, Lines: ls, Superseded: superseded, Reason: reason}); err != nil {
			return err
		}
		res = &InputBatchResult{Batch: stored, Lines: ls, Created: true}
		return nil
	})
	return res, err
}

// transitionInBatch moves a batch along one edge under an expected_version check.
func (s *PgStore) transitionInBatch(ctx context.Context, id string, expectedVersion int, from []string, notFrom error, set string, args []any, kind InputChangeKind, reason string, mk InputEventBuilder) (*domain.InputBatch, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.InputBatch
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanInBatch(tx.QueryRow(ctx, `SELECT `+inBatchCols+` FROM wfp_payroll_input_batches
			WHERE tenant_id = $1 AND batch_id = $2::uuid FOR UPDATE`, tenantID, id))
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrInputBatchVersion
		}
		allowed := false
		for _, f := range from {
			allowed = allowed || cur.State == f
		}
		if !allowed {
			return notFrom
		}
		out, err = scanInBatch(tx.QueryRow(ctx, `UPDATE wfp_payroll_input_batches SET `+set+`, version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND batch_id = $2::uuid RETURNING `+inBatchCols, append([]any{tenantID, id}, args...)...))
		if err != nil {
			return err
		}
		return enqueueMany(ctx, tx, mk, InputChange{Kind: kind, Batch: out, Reason: reason})
	})
	return out, err
}

// FreezeInputBatch freezes a READY batch so payroll calculates from a stable population.
func (s *PgStore) FreezeInputBatch(ctx context.Context, id string, expectedVersion int, actor string, mk InputEventBuilder) (*domain.InputBatch, error) {
	return s.transitionInBatch(ctx, id, expectedVersion, []string{domain.InputBatchReady}, domain.ErrInputBatchNotReady,
		`state = 'FROZEN', frozen_at = now(), frozen_by = $3`, []any{actor}, InputChangeFreeze, "", mk)
}

// CancelInputBatch cancels an unfrozen (READY) batch.
func (s *PgStore) CancelInputBatch(ctx context.Context, id string, expectedVersion int, reason string, mk InputEventBuilder) (*domain.InputBatch, error) {
	return s.transitionInBatch(ctx, id, expectedVersion, []string{domain.InputBatchReady}, domain.ErrInputBatchNotReady,
		`state = 'CANCELLED'`, nil, InputChangeCancel, reason, mk)
}

// ConsumeInputBatch records that a payroll run took this FROZEN batch as its input.
func (s *PgStore) ConsumeInputBatch(ctx context.Context, id string, expectedVersion int, runID string, mk InputEventBuilder) (*domain.InputBatch, error) {
	b, err := s.transitionInBatch(ctx, id, expectedVersion, []string{domain.InputBatchFrozen}, domain.ErrInputBatchNotFrozen,
		`state = 'CONSUMED', payroll_run_id = $3::uuid, consumed_at = now()`, []any{runID}, InputChangeConsume, "", mk)
	if err != nil && isUniqueViolation(err) {
		return nil, domain.ErrRunAlreadyLinked
	}
	return b, err
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// ── queries ──────────────────────────────────────────────────────────────────

func (s *PgStore) GetInputBatch(ctx context.Context, id string) (*domain.InputBatch, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var b *domain.InputBatch
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		b, e = scanInBatch(tx.QueryRow(ctx, `SELECT `+inBatchCols+` FROM wfp_payroll_input_batches
			WHERE tenant_id = $1 AND batch_id = $2::uuid`, tenantID, id))
		return e
	})
	return b, err
}

func (s *PgStore) ListInputBatches(ctx context.Context, legalEntityID, periodID, state string) ([]domain.InputBatch, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.InputBatch
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+inBatchCols+` FROM wfp_payroll_input_batches
			WHERE tenant_id = $1 AND ($2 = '' OR legal_entity_id = $2) AND ($3 = '' OR payroll_period_id = $3)
			  AND ($4 = '' OR state = $4) ORDER BY payroll_period_id, batch_version LIMIT 500`,
			tenantID, legalEntityID, periodID, state)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanInBatch(rows)
			if err != nil {
				return err
			}
			out = append(out, *b)
		}
		return rows.Err()
	})
	return out, err
}

// ListInputLines returns a batch's lines, optionally only one state / one worker.
func (s *PgStore) ListInputLines(ctx context.Context, batchID, state, workerID string) ([]domain.InputLine, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.InputLine
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		out, e = queryInLines(ctx, tx, `WHERE tenant_id = $1 AND batch_id = $2::uuid AND ($3 = '' OR state = $3)
			AND ($4 = '' OR worker_id::text = $4) ORDER BY source_line_id`, tenantID, batchID, state, workerID)
		return e
	})
	return out, err
}

// ListInputControlTotals returns the per-category, per-currency totals.
func (s *PgStore) ListInputControlTotals(ctx context.Context, batchID string) ([]domain.InputControlTotal, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.InputControlTotal{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT category, currency, line_count, total_amount::text FROM wfp_payroll_input_totals
			WHERE tenant_id = $1 AND batch_id = $2::uuid ORDER BY category, currency`, tenantID, batchID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.InputControlTotal
			if err := rows.Scan(&t.Category, &t.Currency, &t.LineCount, &t.TotalAmount); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// ListLiveBonusSourceLines returns the bonus source lines already held by a live
// batch of ANOTHER period for this entity, so a bonus is imported into exactly
// one period.
func (s *PgStore) ListLiveBonusSourceLines(ctx context.Context, legalEntityID, excludePeriodID string) (map[string]bool, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT l.source_line_id FROM wfp_payroll_input_lines l
			JOIN wfp_payroll_input_batches b ON b.batch_id = l.batch_id AND b.tenant_id = l.tenant_id
			WHERE l.tenant_id = $1 AND b.legal_entity_id = $2 AND b.payroll_period_id <> $3
			  AND b.state IN ('READY', 'FROZEN', 'CONSUMED') AND l.source_kind = 'COMP_BONUS' AND l.state = 'ACCEPTED'`,
			tenantID, legalEntityID, excludePeriodID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out[id] = true
		}
		return rows.Err()
	})
	return out, err
}
