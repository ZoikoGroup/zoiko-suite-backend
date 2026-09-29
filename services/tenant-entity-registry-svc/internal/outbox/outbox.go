// Package outbox provides the transactional outbox that replaces this
// service's fire-and-forget event publishing.
//
// ORG §9.2 requires the authoritative write path to use "named commands,
// expected version, idempotency and transactional outbox". The first three
// live in internal/registry; this is the fourth.
//
// WHAT THIS FIXES. Every publish here used to be `go s.events.Publish...` — a
// goroutine on a context detached from the request, writing straight to Kafka
// AFTER the business transaction had already committed. A broker blip or a
// crash in that window leaves this service holding the authoritative fact
// while nothing downstream ever hears about it. For a master-data registry
// that is worse than it sounds: the estate's tax, accounting and reporting
// services key off legal-entity identity, and one lost LegalEntityProfileAmended
// means every one of them keeps calculating against a name, registry number or
// jurisdiction that this service no longer considers current.
//
// The outbox makes the event part of the same transaction as the fact it
// attests. Either both land or neither does, and a relay delivers what landed.
// Delivery is at-least-once, so every consumer dedupes on event_id.
//
// PORTED, deliberately, from identity-context-svc/internal/outbox rather than
// written afresh: the event_outbox table in migration 000006 is the same shape
// as that service's, including the app.outbox_relay named capability, so the
// estate has one outbox pattern to reason about instead of two dialects of one
// idea. Divergence here should be treated as a bug in one of the two.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// Record is one pending event.
type Record struct {
	EventID      string
	EventType    string
	TenantID     string
	PartitionKey string
	// Payload is the fully rendered platform event envelope. Stored rendered
	// rather than reassembled at publish time so what is delivered is what was
	// decided, even if the envelope struct changes shape in a later release.
	Payload  []byte
	Attempts int
}

// Enqueuer is the write half, as the publisher sees it.
//
// Two methods rather than one because the callers differ in kind. A resolver
// that has a transaction open wants the event in THAT transaction; a caller
// with nothing else to commit wants the outbox to open its own. Collapsing
// them would force every caller to manufacture a transaction it does not need.
type Enqueuer interface {
	// EnqueueTx writes the event inside an existing transaction.
	EnqueueTx(ctx context.Context, tx pgx.Tx, rec Record) error
	// Enqueue writes the event in a transaction of its own.
	Enqueue(ctx context.Context, rec Record) error
}

// Store is the Postgres-backed outbox.
type Store struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewStore(pool *pgxpool.Pool, log *zap.Logger) *Store {
	return &Store{pool: pool, log: log}
}

// EnqueueTx writes the event inside tx.
//
// The caller is responsible for having set app.tenant_id on tx — the RLS
// policy on event_outbox requires it, exactly like every other table here. A
// caller that forgot will get a policy violation rather than a silent
// cross-tenant write, which is the failure mode worth having.
//
// ON CONFLICT DO NOTHING on the event id makes a retried transaction
// idempotent. Event ids are fresh UUIDs per call, so a natural collision is
// not expected; what this guards is the same logical event being enqueued
// twice by a retried caller.
func (s *Store) EnqueueTx(ctx context.Context, tx pgx.Tx, rec Record) error {
	if rec.EventID == "" || rec.EventType == "" {
		return errors.New("outbox: event_id and event_type are required")
	}
	if !json.Valid(rec.Payload) {
		return fmt.Errorf("outbox: event %q payload is not valid JSON", rec.EventType)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO event_outbox (event_id, event_type, tenant_id, partition_key, payload)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (event_id) DO NOTHING`,
		rec.EventID, rec.EventType, rec.TenantID, rec.PartitionKey, rec.Payload,
	)
	if err != nil {
		return fmt.Errorf("outbox: enqueue %q: %w", rec.EventType, err)
	}
	return nil
}

// Enqueue writes the event in its own transaction, scoped to rec.TenantID.
//
// An event with no tenant — a resolution that failed before one was resolved —
// is written under the relay capability rather than a tenant scope. There is
// no tenant to scope it to, and refusing to record a failed resolution because
// it never got far enough to name a tenant would lose exactly the events an
// investigation needs.
func (s *Store) Enqueue(ctx context.Context, rec Record) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("outbox: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded on the commit path

	if rec.TenantID != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", rec.TenantID); err != nil {
			return fmt.Errorf("outbox: set_config app.tenant_id: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
			return fmt.Errorf("outbox: set_config app.outbox_relay: %w", err)
		}
	}

	if err := s.EnqueueTx(ctx, tx, rec); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── Relay ────────────────────────────────────────────────────────────────────

// MessageWriter is the one method the relay needs from *kafka.Writer, narrowed
// so the relay is testable without a broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...KafkaMessage) error
}

// KafkaMessage mirrors kafka.Message's two fields the relay sets. Declared
// here rather than importing kafka-go into this package so a test fake does
// not drag the broker client in with it; cmd/server adapts between the two.
type KafkaMessage struct {
	Key   []byte
	Value []byte
}

// RelayConfig tunes the drain loop.
type RelayConfig struct {
	// BatchSize bounds one drain pass. Large enough that a backlog clears in
	// reasonable time, small enough that one pass cannot hold a transaction
	// open long enough to matter.
	BatchSize int
	// PollInterval is how long the relay waits when it finds nothing. It is
	// NOT the publish latency for a normal event: a drain that finds a full
	// batch immediately loops again, so a burst is delivered at broker speed
	// and only an idle relay sleeps.
	PollInterval time.Duration
	// MaxAttempts is where a poison event stops being retried. It is not
	// deleted — it stays in the table with its last_error, which is how an
	// operator finds it. An event retried forever would keep a broken payload
	// at the head of the queue indefinitely.
	MaxAttempts int
	// BaseBackoff is the first retry delay; subsequent ones double up to
	// MaxBackoff.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// DefaultRelayConfig is tuned for the local stack and is a reasonable
// production starting point.
func DefaultRelayConfig() RelayConfig {
	return RelayConfig{
		BatchSize:    100,
		PollInterval: 1 * time.Second,
		MaxAttempts:  12,
		BaseBackoff:  2 * time.Second,
		MaxBackoff:   5 * time.Minute,
	}
}

// RelayStore is the relay's persistence contract.
//
// Extracted from the relay so the DRAIN LOGIC is testable without Postgres.
// What that logic decides is not SQL — how many to take, when to stop a pass,
// how long to wait before retrying, whether a poison event blocks the queue —
// and those are the parts that go wrong. The SQL below is covered by the
// integration tests alongside the other store code.
type RelayStore interface {
	Claim(ctx context.Context, maxAttempts, batchSize int) ([]Record, error)
	MarkPublished(ctx context.Context, eventID string) error
	MarkFailed(ctx context.Context, eventID string, attempts int, retryIn time.Duration, cause error) error
	PendingCount(ctx context.Context) (int, error)
	DeadLetterCount(ctx context.Context, maxAttempts int) (int, error)
}

// Relay drains the outbox to Kafka.
//
// One instance per process is fine, and several are safe: the claim query uses
// FOR UPDATE SKIP LOCKED, so two relays divide the backlog rather than
// duplicating it. Duplicates are still possible across a crash between the
// Kafka write and the published_at update — which is why delivery is
// at-least-once and every consumer in the estate dedupes on event_id.
type Relay struct {
	store  RelayStore
	writer MessageWriter
	cfg    RelayConfig
	log    *zap.Logger

	// published counts successful deliveries, for the metric the dashboard
	// reads. Plain int64 guarded by the single-goroutine Run loop.
	published int64
	failed    int64
}

// NewRelay builds the production relay over Postgres.
func NewRelay(pool *pgxpool.Pool, writer MessageWriter, cfg RelayConfig, log *zap.Logger) *Relay {
	return NewRelayWithStore(&PgRelayStore{pool: pool}, writer, cfg, log)
}

// NewRelayWithStore builds a relay over any RelayStore. Used by tests.
func NewRelayWithStore(store RelayStore, writer MessageWriter, cfg RelayConfig, log *zap.Logger) *Relay {
	if cfg.BatchSize <= 0 {
		cfg = DefaultRelayConfig()
	}
	return &Relay{store: store, writer: writer, cfg: cfg, log: log}
}

// Stats reports what the relay has done, for the health endpoint and metrics.
func (r *Relay) Stats() (published, failed int64) {
	return r.published, r.failed
}

// Run drains until ctx is cancelled.
//
// An unreachable broker is not fatal and never has been: this is a Tier 0
// authentication service and authentication does not need Kafka. Events
// accumulate in Postgres and are delivered when the broker returns, which is
// the entire reason for durably storing them first.
func (r *Relay) Run(ctx context.Context) {
	r.log.Info("outbox relay started",
		zap.Int("batch_size", r.cfg.BatchSize),
		zap.Duration("poll_interval", r.cfg.PollInterval),
	)
	for {
		if ctx.Err() != nil {
			r.log.Info("outbox relay stopped",
				zap.Int64("published", r.published),
				zap.Int64("failed", r.failed),
			)
			return
		}

		n, err := r.drainOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.log.Warn("outbox drain failed", zap.Error(err))
		}

		// A full batch means there is probably more waiting: loop straight
		// back rather than sleeping a second per batch through a backlog.
		if n == r.cfg.BatchSize {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(r.cfg.PollInterval):
		}
	}
}

// DrainOnce runs a single pass and reports how many events were published.
// Exported for the shutdown path and for tests, which want a deterministic
// single pass rather than a loop.
func (r *Relay) DrainOnce(ctx context.Context) (int, error) {
	return r.drainOnce(ctx)
}

// drainOnce claims a batch, publishes it, and marks the outcome.
//
// The claim and the mark are separate transactions ON PURPOSE. Holding one
// transaction across the Kafka round trip would keep a row lock for the whole
// of a broker timeout, and a slow broker would then block every enqueue behind
// it — turning an event-delivery problem into an authentication outage.
func (r *Relay) drainOnce(ctx context.Context) (int, error) {
	batch, err := r.store.Claim(ctx, r.cfg.MaxAttempts, r.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}

	// ONE WRITE FOR THE WHOLE BATCH, then per-record only if that fails.
	//
	// THIS COST FOUR HOURS PER MINUTE OF LOAD. The relay used to call
	// WriteMessages once per record, and segmentio/kafka-go's Writer is a
	// BATCHING writer: a synchronous WriteMessages returns when the batch is
	// flushed, and a batch holding one message is not flushed until
	// BatchTimeout expires. The default BatchTimeout is one second, so the
	// outbox drained at almost exactly one event per second regardless of
	// backlog, broker health or batch size — measured at 1.03/s against a
	// 14,800-event backlog, with zero errors and zero retries, which is why
	// nothing looked wrong. A sixty-second load run left four hours of drain
	// behind it.
	//
	// Passing the whole claimed batch in one call fills kafka-go's batch
	// immediately and costs one round trip for up to BatchSize events.
	//
	// The slow path below is the original per-record loop, kept verbatim: it
	// is what produces an exact per-event verdict when a write fails, and it
	// is worth a second round trip on the rare path to keep that. A partial
	// batch failure may redeliver events that did reach the topic — the same
	// property MarkPublished already documents, and the reason consumers
	// dedupe.
	msgs := make([]KafkaMessage, len(batch))
	for i, rec := range batch {
		msgs[i] = KafkaMessage{Key: []byte(rec.PartitionKey), Value: rec.Payload}
	}
	if err := r.writer.WriteMessages(ctx, msgs...); err == nil {
		for _, rec := range batch {
			if err := r.store.MarkPublished(ctx, rec.EventID); err != nil {
				r.log.Error("outbox: event published but not marked — will redeliver",
					zap.String("event_id", rec.EventID), zap.Error(err))
			}
			r.published++
		}
		return len(batch), nil
	}

	published := 0
	for _, rec := range batch {
		writeErr := r.writer.WriteMessages(ctx, KafkaMessage{
			Key:   []byte(rec.PartitionKey),
			Value: rec.Payload,
		})
		if writeErr != nil {
			r.failed++
			if err := r.store.MarkFailed(ctx, rec.EventID, rec.Attempts, r.backoffFor(rec.Attempts), writeErr); err != nil {
				r.log.Error("outbox: could not record publish failure",
					zap.String("event_id", rec.EventID), zap.Error(err))
			}
			// Stop the pass on the first failure. The broker is almost
			// certainly down for the rest of the batch too, and hammering it
			// with ninety-nine more writes turns one backoff into a hundred.
			break
		}
		if err := r.store.MarkPublished(ctx, rec.EventID); err != nil {
			// The event IS on the topic; only the bookkeeping failed. It will
			// be redelivered on the next pass, which is why consumers dedupe.
			r.log.Error("outbox: event published but not marked — will redeliver",
				zap.String("event_id", rec.EventID), zap.Error(err))
		}
		r.published++
		published++
	}
	return published, nil
}

// backoffFor is the retry delay after `attempts` failures.
//
// Exponential from BaseBackoff, capped at MaxBackoff. The CAP matters more
// than the growth: without it, the twelfth attempt on a two-second base would
// be scheduled two and a quarter hours out, and an operator who had just fixed
// the broker would watch a healthy service deliver nothing for the rest of the
// afternoon.
func (r *Relay) backoffFor(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	backoff := time.Duration(float64(r.cfg.BaseBackoff) * math.Pow(2, float64(attempts)))
	// The overflow check is not theoretical: at BaseBackoff=2s the shift
	// passes the int64 nanosecond ceiling around attempt 62, and a negative
	// duration would schedule the retry in the PAST — turning a poison event
	// into a hot loop.
	if backoff > r.cfg.MaxBackoff || backoff <= 0 {
		backoff = r.cfg.MaxBackoff
	}
	return backoff
}

// PendingCount reports how many events are waiting, for the health endpoint.
//
// A pending count that only grows is the signal that the relay has stopped or
// the broker is gone, and it is the one number worth alerting on: everything
// else about this service can look healthy while events pile up unseen.
func (r *Relay) PendingCount(ctx context.Context) (int, error) {
	return r.store.PendingCount(ctx)
}

// DeadLetterCount reports events that have exhausted their attempts.
func (r *Relay) DeadLetterCount(ctx context.Context) (int, error) {
	return r.store.DeadLetterCount(ctx, r.cfg.MaxAttempts)
}

// ── Postgres RelayStore ──────────────────────────────────────────────────────

// PgRelayStore is the production RelayStore.
type PgRelayStore struct {
	pool *pgxpool.Pool
}

func NewPgRelayStore(pool *pgxpool.Pool) *PgRelayStore { return &PgRelayStore{pool: pool} }

// Claim locks a batch of due events and returns them.
//
// FOR UPDATE SKIP LOCKED is what makes multiple relay instances safe: each
// takes rows the others have not, rather than blocking on them.
//
// app.outbox_relay is the named RLS capability described in migration 000007.
// This file is the only place in the service that sets it.
func (s *PgRelayStore) Claim(ctx context.Context, maxAttempts, batchSize int) ([]Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("outbox claim: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded on the commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return nil, fmt.Errorf("outbox claim: set relay scope: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT event_id, event_type, tenant_id, partition_key, payload, attempts
		  FROM event_outbox
		 WHERE published_at IS NULL
		   AND next_attempt_at <= NOW()
		   AND attempts < $1
		 ORDER BY created_at
		 LIMIT $2
		 FOR UPDATE SKIP LOCKED`,
		maxAttempts, batchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("outbox claim: query: %w", err)
	}

	var batch []Record
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.EventID, &rec.EventType, &rec.TenantID,
			&rec.PartitionKey, &rec.Payload, &rec.Attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("outbox claim: scan: %w", err)
		}
		batch = append(batch, rec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox claim: rows: %w", err)
	}

	// The lock is released by this commit. Between here and the Kafka write
	// the rows are unlocked, so a second relay could pick them up — which
	// costs a duplicate delivery, not a lost one. Duplicates are what the
	// estate's dedupe is for; losses are not recoverable.
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("outbox claim: commit: %w", err)
	}
	return batch, nil
}

func (s *PgRelayStore) MarkPublished(ctx context.Context, eventID string) error {
	return s.withRelayScope(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE event_outbox
			   SET published_at = NOW(), last_error = NULL
			 WHERE event_id = $1 AND published_at IS NULL`, eventID)
		return err
	})
}

// MarkFailed records the error and schedules the retry.
func (s *PgRelayStore) MarkFailed(ctx context.Context, eventID string, _ int, retryIn time.Duration, cause error) error {
	return s.withRelayScope(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE event_outbox
			   SET attempts        = attempts + 1,
			       next_attempt_at = NOW() + $2::interval,
			       last_error      = $3
			 WHERE event_id = $1`,
			eventID, fmt.Sprintf("%d milliseconds", retryIn.Milliseconds()), cause.Error())
		return err
	})
}

func (s *PgRelayStore) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := s.withRelayScope(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM event_outbox WHERE published_at IS NULL`).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("outbox pending count: %w", err)
	}
	return n, nil
}

func (s *PgRelayStore) DeadLetterCount(ctx context.Context, maxAttempts int) (int, error) {
	var n int
	err := s.withRelayScope(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM event_outbox
			  WHERE published_at IS NULL AND attempts >= $1`, maxAttempts).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("outbox dead letter count: %w", err)
	}
	return n, nil
}

func (s *PgRelayStore) withRelayScope(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded on the commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
