package decisionledger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The writer's table, created by the writer's own migration (the same
// definition in each: authorization-svc, jurisdiction-rules-svc,
// obligations-svc, evidence-requirements-svc):
//
//	CREATE TABLE decision_log_outbox (
//	    decision_id     VARCHAR(64) PRIMARY KEY,
//	    tenant_id       VARCHAR(64) NOT NULL,
//	    record          JSONB       NOT NULL,
//	    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
//	    attempts        INT         NOT NULL DEFAULT 0,
//	    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
//	    delivered_at    TIMESTAMPTZ,
//	    parked_at       TIMESTAMPTZ,
//	    last_error      TEXT);
//
// Row-level security admits a row to its own tenant (app.tenant_id) and to the
// relay, whose transactions set app.decision_log_relay = 'on'.
const table = "decision_log_outbox"

// RelayMarker is the transaction-local setting the relay's statements carry;
// each writer's RLS policy on decision_log_outbox admits it across tenants.
const RelayMarker = "app.decision_log_relay"

// Execer is what Enqueue needs: a pgx.Tx, or a writer's own transaction wrapper.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Enqueue writes rec into decision_log_outbox through q, which must be the
// transaction of the decision rec records, so the two commit together or not
// at all. A record already queued under the same decision_id is left as it
// is (a retried command writes no second record).
func Enqueue(ctx context.Context, q Execer, rec Record) error {
	if err := rec.Normalize(); err != nil {
		return err
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("decisionledger: marshal record: %w", err)
	}
	_, err = q.Exec(ctx, `INSERT INTO `+table+` (decision_id, tenant_id, record) VALUES ($1, $2, $3)
		ON CONFLICT (decision_id) DO NOTHING`, rec.DecisionID, rec.TenantID, body)
	if err != nil {
		return fmt.Errorf("decisionledger: enqueue %s: %w", rec.DecisionID, err)
	}
	return nil
}

// Beginner is what PgQueue needs from *pgxpool.Pool.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PgQueue is Queue over decision_log_outbox.
type PgQueue struct {
	db  Beginner
	now func() time.Time
}

// NewPgQueue binds a queue to the writer's pool.
func NewPgQueue(db Beginner) *PgQueue {
	return &PgQueue{db: db, now: func() time.Time { return time.Now().UTC() }}
}

func (q *PgQueue) asRelay(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := q.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('`+RelayMarker+`', 'on', true)`); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Claim implements Queue.
func (q *PgQueue) Claim(ctx context.Context, limit int, lease time.Duration) ([]Pending, error) {
	now := q.now()
	var out []Pending
	err := q.asRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE `+table+`
			SET next_attempt_at = $3, attempts = attempts + 1
			WHERE decision_id IN (
				SELECT decision_id FROM `+table+`
				WHERE delivered_at IS NULL AND parked_at IS NULL AND next_attempt_at <= $1
				ORDER BY next_attempt_at
				LIMIT $2
				FOR UPDATE SKIP LOCKED)
			RETURNING decision_id, tenant_id, record, attempts`, now, limit, now.Add(lease))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p Pending
			var raw []byte
			if err := rows.Scan(&p.DecisionID, &p.TenantID, &raw, &p.Attempts); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &p.Record); err != nil {
				return fmt.Errorf("decisionledger: undecodable record %s: %w", p.DecisionID, err)
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("decisionledger: claim: %w", err)
	}
	return out, nil
}

func (q *PgQueue) exec(ctx context.Context, sql string, args ...any) error {
	return q.asRelay(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// MarkDelivered implements Queue.
func (q *PgQueue) MarkDelivered(ctx context.Context, decisionID string) error {
	return q.exec(ctx, `UPDATE `+table+` SET delivered_at = $2, last_error = NULL WHERE decision_id = $1`, decisionID, q.now())
}

// MarkRetry implements Queue.
func (q *PgQueue) MarkRetry(ctx context.Context, decisionID, lastError string, next time.Time) error {
	return q.exec(ctx, `UPDATE `+table+` SET last_error = $2, next_attempt_at = $3 WHERE decision_id = $1`, decisionID, lastError, next)
}

// MarkParked implements Queue: the row is kept for an operator and never
// retried; Backlog counts it.
func (q *PgQueue) MarkParked(ctx context.Context, decisionID, lastError string) error {
	return q.exec(ctx, `UPDATE `+table+` SET last_error = $2, parked_at = $3 WHERE decision_id = $1`, decisionID, lastError, q.now())
}

// Backlog is the relay's health: records not yet in the ledger, records
// parked, and the age of the oldest pending one (0 when none).
type Backlog struct {
	Pending       int64
	Parked        int64
	OldestPending time.Duration
}

// Backlog reads the queue's health across tenants.
func (q *PgQueue) Backlog(ctx context.Context) (Backlog, error) {
	var b Backlog
	var oldest *time.Time
	err := q.asRelay(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COUNT(*) FILTER (WHERE delivered_at IS NULL AND parked_at IS NULL),
			       COUNT(*) FILTER (WHERE parked_at IS NOT NULL),
			       MIN(created_at) FILTER (WHERE delivered_at IS NULL AND parked_at IS NULL)
			FROM `+table).Scan(&b.Pending, &b.Parked, &oldest)
	})
	if err != nil {
		return Backlog{}, fmt.Errorf("decisionledger: backlog: %w", err)
	}
	if oldest != nil {
		b.OldestPending = q.now().Sub(*oldest)
	}
	return b, nil
}

// PurgeDelivered deletes rows the ledger has held for longer than olderThan
// (never pending or parked rows; the ledger is the durable record).
// olderThan below one hour is refused.
func (q *PgQueue) PurgeDelivered(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan < time.Hour {
		return 0, fmt.Errorf("decisionledger: purge retention below one hour refused (%s)", olderThan)
	}
	var n int64
	err := q.asRelay(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE delivered_at IS NOT NULL AND delivered_at < $1`, q.now().Add(-olderThan))
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("decisionledger: purge: %w", err)
	}
	return n, nil
}
