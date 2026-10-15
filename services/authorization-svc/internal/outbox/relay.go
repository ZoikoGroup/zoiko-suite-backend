// Package outbox relays decision events from outbox_events to Kafka.
//
// The store writes each decision's events in the same transaction as the
// decision row (domain.RecordAccessDecisionParams.Events); this relay publishes
// them afterwards, at least once. Rows are claimed with FOR UPDATE SKIP LOCKED,
// so replicas share the work without publishing a row twice concurrently.
//
// There is deliberately no dead-letter ceiling. A row that keeps failing stays
// pending and is logged on every attempt: giving up after N tries would be the
// silent loss this table exists to remove, only later.
package outbox

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Writer is the one method the relay needs from *kafka.Writer.
type Writer interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Beginner is the one method the relay needs from *pgxpool.Pool.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type Relay struct {
	db        Beginner
	writer    Writer
	interval  time.Duration
	batchSize int
	log       *zap.Logger
}

func NewRelay(db Beginner, writer Writer, interval time.Duration, batchSize int, log *zap.Logger) *Relay {
	if interval <= 0 {
		interval = time.Second
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	return &Relay{db: db, writer: writer, interval: interval, batchSize: batchSize, log: log}
}

// Run relays until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	r.log.Info("outbox relay started", zap.Duration("interval", r.interval), zap.Int("batch_size", r.batchSize))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A full batch means more are waiting: drain without waiting a
			// tick, so a backlog clears at Kafka speed, not batchSize/second.
			for {
				n, err := r.RelayOnce(ctx)
				if err != nil && ctx.Err() == nil {
					r.log.Error("outbox relay pass failed", zap.Error(err))
				}
				if err != nil || n < r.batchSize || ctx.Err() != nil {
					break
				}
			}
		}
	}
}

const claimSQL = `
	SELECT outbox_event_id, event_type, message_key, message_value, publish_attempts
	  FROM outbox_events
	 WHERE published_at IS NULL
	 ORDER BY created_at
	 LIMIT $1
	 FOR UPDATE SKIP LOCKED`

type pending struct {
	id, eventType, key string
	value              []byte
	attempts           int
}

// RelayOnce publishes one batch and reports how many rows it published.
//
// ONE WriteMessages call per batch (Governance audit N-04, 13 Oct 2026). It
// used to write row by row, and kafka-go flushes an incomplete batch only
// after Writer.BatchTimeout (1s by default), so every row cost a second: the
// relay drained about one event per second while every authorization decision
// added one, and the IAM, session and SoD events the 000026/000029/000030
// triggers write queued behind them — a revocation reached downstream services
// as late as the backlog was long, while this transaction held the claimed
// rows' locks. A partial failure comes back as kafka.WriteErrors, one entry
// per message, so only the rows that failed stay pending.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, claimSQL, r.batchSize)
	if err != nil {
		return 0, err
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.eventType, &p.key, &p.value, &p.attempts); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}

	msgs := make([]kafka.Message, len(batch))
	for i, p := range batch {
		msgs[i] = kafka.Message{Key: []byte(p.key), Value: p.value}
	}
	failed := make([]error, len(batch))
	wholeBatch := false
	if werr := r.writer.WriteMessages(ctx, msgs...); werr != nil {
		var perMessage kafka.WriteErrors
		if errors.As(werr, &perMessage) && len(perMessage) == len(batch) {
			copy(failed, perMessage)
		} else {
			wholeBatch = true
			for i := range failed {
				failed[i] = werr
			}
			// One line for the batch, not one per row: during a broker outage
			// the per-row lines were thousands a minute.
			r.log.Error("outbox relay: publish failed for the whole batch — rows stay pending",
				zap.Int("rows", len(batch)), zap.Int("oldest_attempts", batch[0].attempts+1), zap.Error(werr))
		}
	}

	var publishedIDs, failedIDs []string
	lastErr := ""
	for i, p := range batch {
		if failed[i] == nil {
			publishedIDs = append(publishedIDs, p.id)
			continue
		}
		failedIDs = append(failedIDs, p.id)
		lastErr = failed[i].Error()
		if wholeBatch {
			continue
		}
		r.log.Error("outbox relay: publish failed — row stays pending",
			zap.String("outbox_event_id", p.id),
			zap.String("event_type", p.eventType),
			zap.Int("attempts", p.attempts+1),
			zap.Error(failed[i]))
	}
	if len(publishedIDs) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE outbox_events SET published_at = NOW() WHERE outbox_event_id = ANY($1::uuid[])`,
			publishedIDs); err != nil {
			return 0, err
		}
	}
	if len(failedIDs) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE outbox_events SET publish_attempts = publish_attempts + 1, last_error = $2 WHERE outbox_event_id = ANY($1::uuid[])`,
			failedIDs, lastErr); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(publishedIDs), nil
}
