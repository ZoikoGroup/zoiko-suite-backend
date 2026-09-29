package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/events"
)

// ── transactional outbox ─────────────────────────────────────────────────────

// OutboxRecord is one claimed, unpublished event.
type OutboxRecord struct {
	OutboxID  int64
	EventType string
	Key       string
	Body      []byte
}

// enqueue writes one sealed envelope into event_outbox on the caller's open
// transaction.
//
// Sharing the transaction is the whole point — see CompleteDelivery and the
// other transition methods that call it, for what the alternative cost.
//
// It is unexported and takes a tx rather than a context, so there is no way to
// enqueue an event OUTSIDE the transaction that records the fact it describes.
// An exported EnqueueEvent(ctx, ...) would have re-created the original defect
// with a table in the middle of it.
func enqueue(ctx context.Context, tx pgx.Tx, tenantID string, out events.Outbound) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO event_outbox (tenant_id, event_type, aggregate_key, payload)
		VALUES ($1, $2, $3, $4)
	`, tenantID, out.EventType, out.Key, out.Body)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", out.EventType, err)
	}
	return nil
}

// withRelay runs fn with app.outbox_relay installed instead of a tenant.
//
// The relay is the one write path in this service that legitimately crosses
// tenants: it drains every tenant's backlog from a single loop. Rather than
// letting it run unscoped — which under FORCE ROW LEVEL SECURITY would simply
// see nothing, and would present as a relay that publishes nothing while
// reporting no error at all — it names itself, so migration 000010's policy
// admits it by an explicit, auditable disjunct rather than by the absence of a
// control.
//
// A DIFFERENT flag from app.platform_scope, which 000004 grants over
// `notifications`. That hatch is SELECT-only and exists so the retry worker can
// discover work; this one must also UPDATE, to mark a batch published. Reusing
// one name would have silently widened the retry hatch from read to write
// across every tenant's notification bodies.
//
// set_config(..., true) is transaction-local, so the flag cannot survive on a
// pooled connection into somebody's request.
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
// for the duration of the caller's drain.
//
// FOR UPDATE SKIP LOCKED is what makes more than one replica safe: a second
// relay claims the next batch instead of blocking on, or duplicating, this one.
func (s *PgStore) ClaimOutbox(ctx context.Context, limit int, fn func([]OutboxRecord) error) error {
	return s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT outbox_id, event_type, aggregate_key, payload
			FROM event_outbox
			WHERE published_at IS NULL
			ORDER BY created_at, outbox_id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		`, limit)
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

		// fn publishes. It runs while the rows are still locked and BEFORE the
		// marking commits, so a crash mid-publish rolls the marking back and
		// the events are re-delivered rather than lost. At-least-once, chosen
		// deliberately: a duplicate notification.sent makes a consumer
		// re-handle an event it has already seen — which the event_id and the
		// notification_id both let it detect — while a lost one leaves a
		// governed notice whose issue nobody was ever told about.
		if err := fn(claimed); err != nil {
			ids := make([]int64, 0, len(claimed))
			for _, r := range claimed {
				ids = append(ids, r.OutboxID)
			}
			// Recorded on the rows themselves, so a stuck event can be
			// diagnosed from the table without correlating against logs.
			if _, uerr := tx.Exec(ctx, `
				UPDATE event_outbox
				SET attempts = attempts + 1, last_error = $2
				WHERE outbox_id = ANY($1)
			`, ids, err.Error()); uerr != nil {
				return fmt.Errorf("record publish failure: %w (original: %v)", uerr, err)
			}
			// Committed: the attempt count and the error are worth keeping
			// even though the publish failed. published_at is untouched, so
			// the rows are claimed again on the next tick.
			if cerr := tx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit publish failure: %w (original: %v)", cerr, err)
			}
			return err
		}

		ids := make([]int64, 0, len(claimed))
		for _, r := range claimed {
			ids = append(ids, r.OutboxID)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE event_outbox SET published_at = now() WHERE outbox_id = ANY($1)
		`, ids); err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		return nil
	})
}

// OutboxDepth reports the unpublished backlog and the age of its oldest entry.
//
// Both, not just the depth. A backlog of ten that is three seconds old is a
// service under load; a backlog of ten that is an hour old is a relay that has
// stopped — and on this service that means an hour of governed notices whose
// issue no consumer has been told about, while the register shows every one of
// them concluded. Depth alone cannot tell those apart, which is why the alert
// rule keys on the age.
func (s *PgStore) OutboxDepth(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		row := tx.QueryRow(ctx, `
			SELECT count(*), min(created_at) FROM event_outbox WHERE published_at IS NULL
		`)
		if err := row.Scan(&pending, &oldest); err != nil {
			return fmt.Errorf("outbox depth: %w", err)
		}
		if oldest != nil {
			oldestAge = time.Since(*oldest)
		}
		return nil
	})
	return pending, oldestAge, err
}
