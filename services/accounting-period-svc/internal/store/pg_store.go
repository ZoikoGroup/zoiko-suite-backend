// Package store is the Postgres implementation of service.Store, plus the
// outbox claim side the relay uses.
//
// Every transaction installs app.tenant_id as a transaction-local setting; all
// four tables FORCE row-level security on it, and every query ALSO filters on
// tenant_id explicitly (defence in depth: RLS must not be the only thing
// standing between two tenants). All reads go to the primary: the pool has one
// target and no replica routing, which is what the posting gate relies on for
// strong consistency.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/service"
)

// PgStore is the Postgres-backed store.
type PgStore struct {
	pool *pgxpool.Pool
}

// New builds the store over a pool.
func New(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

var _ service.Store = (*PgStore)(nil)

// InTx runs fn in one transaction with app.tenant_id installed. An empty
// tenant is refused: nothing in this service is readable without one.
func (s *PgStore) InTx(ctx context.Context, tenantID string, fn func(service.Tx) error) error {
	if tenantID == "" {
		return domain.Errf(domain.CodeContextInvalid, "tenant context is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(&pgTx{tx: tx, tenant: tenantID}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

type pgTx struct {
	tx     pgx.Tx
	tenant string
}

func validUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

func (t *pgTx) LockKey(ctx context.Context, key string) error {
	_, err := t.tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key)
	return err
}

// ── idempotency ──────────────────────────────────────────────────────────────

func (t *pgTx) GetIdempotency(ctx context.Context, key string) (*service.IdempotencyRecord, error) {
	var r service.IdempotencyRecord
	err := t.tx.QueryRow(ctx, `
		SELECT idempotency_key, operation, request_hash, response
		FROM accounting_period_idempotency WHERE tenant_id = $1 AND idempotency_key = $2`, t.tenant, key).
		Scan(&r.Key, &r.Operation, &r.RequestHash, &r.Response)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (t *pgTx) PutIdempotency(ctx context.Context, rec service.IdempotencyRecord) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO accounting_period_idempotency (tenant_id, idempotency_key, operation, request_hash, response)
		VALUES ($1, $2, $3, $4, $5)`, t.tenant, rec.Key, rec.Operation, rec.RequestHash, []byte(rec.Response))
	return err
}

// ── periods ──────────────────────────────────────────────────────────────────

const periodColumns = `period_id::text, tenant_id, legal_entity_id, calendar_id, calendar_version_id, book_scope, module_scope,
	period_key, fiscal_year, period_no, to_char(start_date,'YYYY-MM-DD'), to_char(end_date,'YYYY-MM-DD'), kind, state, version,
	reopen_book_scope, reopen_module_scope, reopen_expires_at, created_at, updated_at`

func scanPeriod(row pgx.Row) (*domain.Period, error) {
	var p domain.Period
	var kind, state string
	var rb, rm *string
	var rexp *time.Time
	if err := row.Scan(&p.PeriodID, &p.TenantID, &p.LegalEntityID, &p.CalendarID, &p.CalendarVersionID, &p.BookScope, &p.ModuleScope,
		&p.PeriodKey, &p.FiscalYear, &p.PeriodNo, &p.StartDate, &p.EndDate, &kind, &state, &p.Version,
		&rb, &rm, &rexp, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Kind, p.State = domain.Kind(kind), domain.State(state)
	if rexp != nil {
		w := domain.ReopenWindow{ExpiresAt: rexp.UTC()}
		if rb != nil {
			w.BookScope = *rb
		}
		if rm != nil {
			w.ModuleScope = *rm
		}
		p.Reopen = &w
	}
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	return &p, nil
}

func (t *pgTx) queryPeriods(ctx context.Context, sql string, args ...any) ([]domain.Period, error) {
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Period
	for rows.Next() {
		p, err := scanPeriod(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (t *pgTx) GetPeriod(ctx context.Context, id string, forUpdate bool) (*domain.Period, error) {
	if !validUUID(id) {
		return nil, nil
	}
	q := `SELECT ` + periodColumns + ` FROM accounting_periods WHERE tenant_id = $1 AND period_id = $2::uuid`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	p, err := scanPeriod(t.tx.QueryRow(ctx, q, t.tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (t *pgTx) FindPeriodByKey(ctx context.Context, entity, versionID, book, module, key string) (*domain.Period, error) {
	p, err := scanPeriod(t.tx.QueryRow(ctx, `SELECT `+periodColumns+` FROM accounting_periods
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND calendar_version_id = $3 AND book_scope = $4 AND module_scope = $5 AND period_key = $6`,
		t.tenant, entity, versionID, book, module, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (t *pgTx) ListPeriodsByKey(ctx context.Context, entity, key string) ([]domain.Period, error) {
	return t.queryPeriods(ctx, `SELECT `+periodColumns+` FROM accounting_periods
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND period_key = $3 ORDER BY start_date, period_id`, t.tenant, entity, key)
}

func (t *pgTx) FindPeriodsCovering(ctx context.Context, entity, date string) ([]domain.Period, error) {
	return t.queryPeriods(ctx, `SELECT `+periodColumns+` FROM accounting_periods
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND start_date <= $3::date AND end_date >= $3::date
		ORDER BY start_date, period_id`, t.tenant, entity, date)
}

func (t *pgTx) ListPeriods(ctx context.Context, entity, state string, limit, offset int) ([]domain.Period, error) {
	return t.queryPeriods(ctx, `SELECT `+periodColumns+` FROM accounting_periods
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3 = '' OR state = $3)
		ORDER BY start_date, period_id LIMIT $4 OFFSET $5`, t.tenant, entity, state, limit, offset)
}

func (t *pgTx) InsertPeriod(ctx context.Context, p *domain.Period) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO accounting_periods (period_id, tenant_id, legal_entity_id, calendar_id, calendar_version_id, book_scope, module_scope,
			period_key, fiscal_year, period_no, start_date, end_date, kind, state, version, created_at, updated_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::date, $12::date, $13, $14, $15, $16, $17)`,
		p.PeriodID, t.tenant, p.LegalEntityID, p.CalendarID, p.CalendarVersionID, p.BookScope, p.ModuleScope,
		p.PeriodKey, p.FiscalYear, p.PeriodNo, p.StartDate, p.EndDate, string(p.Kind), string(p.State), p.Version, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert period %s: %w", p.PeriodKey, err)
	}
	return nil
}

func (t *pgTx) UpdatePeriodState(ctx context.Context, p *domain.Period, expectedVersion int64) error {
	var rb, rm *string
	var rexp *time.Time
	if p.Reopen != nil {
		b, m, e := p.Reopen.BookScope, p.Reopen.ModuleScope, p.Reopen.ExpiresAt
		rb, rm, rexp = &b, &m, &e
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE accounting_periods
		SET state = $3, version = $4, reopen_book_scope = $5, reopen_module_scope = $6, reopen_expires_at = $7, updated_at = $8
		WHERE tenant_id = $1 AND period_id = $2::uuid AND version = $9`,
		t.tenant, p.PeriodID, string(p.State), p.Version, rb, rm, rexp, p.UpdatedAt, expectedVersion)
	if err != nil {
		return fmt.Errorf("update period state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrVersionConflictStore
	}
	return nil
}

// ── history ──────────────────────────────────────────────────────────────────

func (t *pgTx) InsertHistory(ctx context.Context, e *domain.HistoryEntry) error {
	var rb, rm *string
	var rexp *time.Time
	if e.Reopen != nil {
		b, m, x := e.Reopen.BookScope, e.Reopen.ModuleScope, e.Reopen.ExpiresAt
		rb, rm, rexp = &b, &m, &x
	}
	_, err := t.tx.Exec(ctx, `
		INSERT INTO period_state_history (history_id, tenant_id, period_id, from_state, to_state, command, acc14_workflow_ref,
			control_snapshot_ref, requested_by, reason, recorded_at, expected_version, resulting_version, decision_fingerprint,
			reopen_book_scope, reopen_module_scope, reopen_expires_at, correlation_id)
		VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		e.HistoryID, t.tenant, e.PeriodID, string(e.FromState), string(e.ToState), string(e.Command), e.Acc14WorkflowRef,
		e.ControlSnapshotRef, e.RequestedBy, e.Reason, e.RecordedAt, e.ExpectedVersion, e.ResultingVersion, e.DecisionFingerprint,
		rb, rm, rexp, e.CorrelationID)
	if err != nil {
		return fmt.Errorf("insert history: %w", err)
	}
	return nil
}

func (t *pgTx) ListHistory(ctx context.Context, periodID string) ([]domain.HistoryEntry, error) {
	if !validUUID(periodID) {
		return nil, nil
	}
	rows, err := t.tx.Query(ctx, `
		SELECT history_id::text, tenant_id, period_id::text, from_state, to_state, command, acc14_workflow_ref, control_snapshot_ref,
			requested_by, reason, recorded_at, expected_version, resulting_version, decision_fingerprint,
			reopen_book_scope, reopen_module_scope, reopen_expires_at, correlation_id
		FROM period_state_history WHERE tenant_id = $1 AND period_id = $2::uuid
		ORDER BY resulting_version, recorded_at, history_id`, t.tenant, periodID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.HistoryEntry
	for rows.Next() {
		var h domain.HistoryEntry
		var from, to, cmd string
		var rb, rm *string
		var rexp *time.Time
		if err := rows.Scan(&h.HistoryID, &h.TenantID, &h.PeriodID, &from, &to, &cmd, &h.Acc14WorkflowRef, &h.ControlSnapshotRef,
			&h.RequestedBy, &h.Reason, &h.RecordedAt, &h.ExpectedVersion, &h.ResultingVersion, &h.DecisionFingerprint,
			&rb, &rm, &rexp, &h.CorrelationID); err != nil {
			return nil, err
		}
		h.FromState, h.ToState, h.Command = domain.State(from), domain.State(to), domain.Command(cmd)
		h.RecordedAt = h.RecordedAt.UTC()
		if rexp != nil {
			w := domain.ReopenWindow{ExpiresAt: rexp.UTC()}
			if rb != nil {
				w.BookScope = *rb
			}
			if rm != nil {
				w.ModuleScope = *rm
			}
			h.Reopen = &w
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (t *pgTx) CalendarUsage(ctx context.Context, calendarID, versionID string) (*string, bool, error) {
	var latest *string
	var anyNotOpen bool
	err := t.tx.QueryRow(ctx, `
		SELECT to_char(max(end_date),'YYYY-MM-DD'), COALESCE(bool_or(state <> 'OPEN'), false)
		FROM accounting_periods
		WHERE tenant_id = $1 AND calendar_id = $2 AND ($3 = '' OR calendar_version_id = $3)`,
		t.tenant, calendarID, versionID).Scan(&latest, &anyNotOpen)
	return latest, anyNotOpen, err
}

// ── outbox ───────────────────────────────────────────────────────────────────

// Enqueue writes the event in the CALLER's transaction: a change that commits
// always has its event, and a change that rolls back never leaks one.
func (t *pgTx) Enqueue(ctx context.Context, e service.OutboxEntry) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO accounting_period_outbox (tenant_id, object_id, event_type, payload)
		VALUES ($1, $2::uuid, $3, $4)`, e.TenantID, e.ObjectID, e.EventType, e.Payload)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", e.EventType, err)
	}
	return nil
}

// OutboxRecord is one claimed, unpublished event.
type OutboxRecord struct {
	OutboxID  int64
	EventType string
	Key       string
	Body      []byte
}

// withRelay runs fn with app.outbox_relay installed instead of a tenant: the
// relay drains every tenant's backlog and is admitted by the outbox policy's
// explicit second disjunct.
func (s *PgStore) withRelay(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin relay transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
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
			SELECT outbox_id, event_type, object_id::text, payload
			FROM accounting_period_outbox
			WHERE published_at IS NULL
			ORDER BY created_at, outbox_id
			LIMIT $1
			FOR UPDATE SKIP LOCKED`, limit)
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
			if _, uerr := tx.Exec(ctx, `
				UPDATE accounting_period_outbox SET attempts = attempts + 1, last_error = $2 WHERE outbox_id = ANY($1)`, ids, err.Error()); uerr != nil {
				return fmt.Errorf("record publish failure: %w (original: %v)", uerr, err)
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit publish failure: %w (original: %v)", cerr, err)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE accounting_period_outbox SET published_at = now() WHERE outbox_id = ANY($1)`, ids); err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		return nil
	})
}

// OutboxDepth reports the unpublished backlog and the age of its oldest entry.
func (s *PgStore) OutboxDepth(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*), min(created_at) FROM accounting_period_outbox WHERE published_at IS NULL`).Scan(&pending, &oldest); err != nil {
			return fmt.Errorf("outbox depth: %w", err)
		}
		if oldest != nil {
			oldestAge = time.Since(*oldest)
		}
		return nil
	})
	return pending, oldestAge, err
}
