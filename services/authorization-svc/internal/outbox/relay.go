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
			if _, err := r.RelayOnce(ctx); err != nil && ctx.Err() == nil {
				r.log.Error("outbox relay pass failed", zap.Error(err))
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

	published := 0
	for _, p := range batch {
		if werr := r.writer.WriteMessages(ctx, kafka.Message{Key: []byte(p.key), Value: p.value}); werr != nil {
			r.log.Error("outbox relay: publish failed — row stays pending",
				zap.String("outbox_event_id", p.id),
				zap.String("event_type", p.eventType),
				zap.Int("attempts", p.attempts+1),
				zap.Error(werr))
			if _, err := tx.Exec(ctx,
				`UPDATE outbox_events SET publish_attempts = publish_attempts + 1, last_error = $2 WHERE outbox_event_id = $1`,
				p.id, werr.Error()); err != nil {
				return published, err
			}
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox_events SET published_at = NOW() WHERE outbox_event_id = $1`, p.id); err != nil {
			return published, err
		}
		published++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return published, nil
}
