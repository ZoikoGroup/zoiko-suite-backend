// Package outbox relays the transactional outbox to Kafka — the publishing
// half of the "transactional outbox; event loss not allowed" requirement
// (Event Catalogue §6, GCP §2 invariant #10). store.RecordDecision writes the
// event in the decision's own transaction; this worker delivers it, at least
// once, for as long as the broker takes to come back.
package outbox

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/events"
)

const (
	// pollInterval is how often the worker polls for new events.
	pollInterval = 500 * time.Millisecond
	// batchSize is the maximum number of events claimed in one transaction.
	batchSize = 100
	// pruneInterval is how often published rows past retention are removed.
	pruneInterval = time.Minute
	// pruneBatch bounds one DELETE so a large backlog of old rows never holds
	// a long lock; several batches run per sweep.
	pruneBatch = 1000
	// maxPruneBatchesPerSweep bounds one sweep; the rest waits a minute.
	maxPruneBatchesPerSweep = 20
)

// Observer receives the relay's health: how many events wait and for how
// long, and how many published rows retention removed. Without it a stalled
// relay shows only in logs (audit R-8).
type Observer interface {
	ObserveOutboxBacklog(pending int, oldest time.Duration)
	ObserveOutboxPruned(n int)
}

// Option configures a Worker.
type Option func(*Worker)

// WithObserver reports backlog and pruning to o.
func WithObserver(o Observer) Option { return func(w *Worker) { w.observer = o } }

// WithRetention removes published rows older than d. Zero keeps them forever.
// Unpublished rows are never removed, however old.
func WithRetention(d time.Duration) Option { return func(w *Worker) { w.retention = d } }

// Worker polls the outbox table and publishes events to Kafka.
type Worker struct {
	pool      *pgxpool.Pool
	producer  events.MessageWriter
	log       *zap.Logger
	observer  Observer
	retention time.Duration

	cancel context.CancelFunc
	doneCh chan struct{}
}

// NewWorker constructs an outbox worker. The topic is the writer's own:
// kafka-go refuses a message that names a topic when the writer already does,
// which is how every publish this worker attempted used to fail.
func NewWorker(pool *pgxpool.Pool, producer events.MessageWriter, log *zap.Logger, opts ...Option) *Worker {
	w := &Worker{
		pool:     pool,
		producer: producer,
		log:      log,
		doneCh:   make(chan struct{}),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Start begins the outbox polling loop in a background goroutine.
func (w *Worker) Start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	go w.run(ctx)
}

// Stop cancels the loop — including a relay pass blocked on an unreachable
// broker, which used to hold shutdown for as long as kafka-go's timeouts across
// a whole batch — and waits for it to finish. Anything not marked published
// stays in the outbox and is relayed again (at least once).
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	<-w.doneCh
}

func (w *Worker) run(ctx context.Context) {
	defer close(w.doneCh)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	lastPrune := time.Time{}

	for {
		select {
		case <-ctx.Done():
			w.log.Info("outbox worker stopped")
			return
		case <-ticker.C:
			if _, err := w.RelayOnce(ctx); err != nil && ctx.Err() == nil {
				w.log.Error("outbox: relay pass failed", zap.Error(err))
			}
			if w.observer != nil {
				if pending, oldest, err := w.Backlog(ctx); err == nil {
					w.observer.ObserveOutboxBacklog(pending, oldest)
				} else if ctx.Err() == nil {
					w.log.Error("outbox: backlog query failed", zap.Error(err))
				}
			}
			if w.retention > 0 && time.Since(lastPrune) >= pruneInterval {
				lastPrune = time.Now()
				n, err := w.Prune(ctx, w.retention)
				if err != nil && ctx.Err() == nil {
					w.log.Error("outbox: retention sweep failed", zap.Error(err))
				}
				if n > 0 && w.observer != nil {
					w.observer.ObserveOutboxPruned(n)
				}
			}
		}
	}
}

// relayTx begins a transaction carrying the app.outbox_relay marker the
// outbox's row-level security policy admits (000012).
func (w *Worker) relayTx(ctx context.Context) (pgx.Tx, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.outbox_relay', 'on', true)`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("mark relay transaction: %w", err)
	}
	return tx, nil
}

// Backlog reports how many events await publishing across all tenants and how
// long the oldest has waited.
func (w *Worker) Backlog(ctx context.Context) (int, time.Duration, error) {
	tx, err := w.relayTx(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pending int
	var oldestSeconds float64
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(EXTRACT(EPOCH FROM now() - MIN(created_at)), 0)::float8
		FROM outbox WHERE published_at IS NULL`).Scan(&pending, &oldestSeconds); err != nil {
		return 0, 0, fmt.Errorf("backlog: %w", err)
	}
	return pending, time.Duration(oldestSeconds * float64(time.Second)), nil
}

// Prune deletes published rows whose publication is older than olderThan, in
// bounded batches, and returns how many it removed. Unpublished rows are never
// touched. The outbox is a delivery mechanism, not evidence: the decision and
// its replay manifests are the record, and they stay append-only.
func (w *Worker) Prune(ctx context.Context, olderThan time.Duration) (int, error) {
	total := 0
	for i := 0; i < maxPruneBatchesPerSweep; i++ {
		tx, err := w.relayTx(ctx)
		if err != nil {
			return total, err
		}
		tag, err := tx.Exec(ctx, `
			DELETE FROM outbox WHERE outbox_id IN (
				SELECT outbox_id FROM outbox
				WHERE published_at IS NOT NULL
				  AND published_at < now() - make_interval(secs => $1)
				ORDER BY outbox_id
				LIMIT $2)`, olderThan.Seconds(), pruneBatch)
		if err != nil {
			_ = tx.Rollback(ctx)
			return total, fmt.Errorf("prune: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return total, fmt.Errorf("prune commit: %w", err)
		}
		n := int(tag.RowsAffected())
		total += n
		if n < pruneBatch {
			break
		}
	}
	return total, nil
}

type pending struct {
	id          int64
	payload     []byte
	aggregateID string
	idemKey     string
}

// RelayOnce claims up to batchSize unpublished events, publishes each, and
// records the result, all in one transaction. It returns how many events were
// published.
//
// The transaction is marked app.outbox_relay = 'on', the one marker the
// outbox's row-level security policy (000012) admits without a tenant: the
// relay drains every tenant's events. Without it the runtime role saw an empty
// outbox and nothing was ever published.
//
// FOR UPDATE SKIP LOCKED lets several replicas relay at once without
// publishing the same row twice from two of them.
func (w *Worker) RelayOnce(ctx context.Context) (int, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT set_config('app.outbox_relay', 'on', true)`); err != nil {
		return 0, fmt.Errorf("mark relay transaction: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT outbox_id, payload, COALESCE(aggregate_id, ''), idempotency_key
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY outbox_id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, batchSize)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pending, error) {
		var p pending
		err := row.Scan(&p.id, &p.payload, &p.aggregateID, &p.idemKey)
		return p, err
	})
	if err != nil {
		return 0, fmt.Errorf("scan: %w", err)
	}

	published := 0
	for _, p := range batch {
		msg := kafka.Message{
			// Keyed by the aggregate (Event Catalogue §5), so every event about
			// one decision lands on one partition in order.
			Key:     []byte(p.aggregateID),
			Value:   p.payload,
			Headers: []kafka.Header{{Key: "Idempotency-Key", Value: []byte(p.idemKey)}},
		}
		if pubErr := w.producer.WriteMessages(ctx, msg); pubErr != nil {
			w.log.Error("outbox: publish failed; will retry",
				zap.Int64("outbox_id", p.id), zap.Error(pubErr))
			if _, err := tx.Exec(ctx,
				`UPDATE outbox SET attempts = attempts + 1, last_error = $1 WHERE outbox_id = $2`,
				pubErr.Error(), p.id); err != nil {
				return published, fmt.Errorf("record failed attempt: %w", err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = NOW() WHERE outbox_id = $1`, p.id); err != nil {
			return published, fmt.Errorf("mark published: %w", err)
		}
		published++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return published, nil
}
