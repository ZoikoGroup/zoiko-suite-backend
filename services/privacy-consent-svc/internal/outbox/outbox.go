// Package outbox is privacy-consent-svc's own transactional outbox
// (ZS-EVENT-001 §6/§7: write in the same transaction as the fact, deliver
// at-least-once). It replaces the old events.KafkaPublisher call the
// handler made AFTER the store's transaction had already committed: a
// crash, a panic, or a broker outage between those two steps lost the
// event with the write already durable and no way to tell anything had
// gone wrong — the publisher's own "event dropped" log line was the whole
// error handling.
//
// Enqueue writes a Record inside the caller's existing pgx.Tx, so the event
// either commits with the evidence row it reports or not at all. Relay
// drains event_outbox on a schedule and is the only thing that ever talks
// to Kafka for an event this service produces.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Record is one event to enqueue. TenantID/EntityID/ActorID/CorrelationID
// are carried as plain columns (not just inside Payload) so the relay and
// any operator query can filter without parsing JSON.
type Record struct {
	EventType     string
	TenantID      string
	EntityID      string
	ActorID       string
	CorrelationID string
	Payload       []byte
}

// Enqueue writes rec to event_outbox inside tx. The caller's transaction
// commit is what makes the event durable; Enqueue itself never talks to
// Kafka.
func Enqueue(ctx context.Context, tx pgx.Tx, rec Record) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO event_outbox (outbox_event_id, event_type, tenant_id, entity_id, actor_id, correlation_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		"evt-"+uuid.New().String(), rec.EventType, rec.TenantID, rec.EntityID, rec.ActorID, rec.CorrelationID, rec.Payload,
	)
	if err != nil {
		return fmt.Errorf("outbox.Enqueue: %w", err)
	}
	return nil
}

// Envelope is the on-the-wire shape written to Kafka — kept identical to
// events.Event's field names so a consumer migrating from the old
// fire-and-forget publisher sees no shape change, only a delivery
// guarantee it didn't have before.
type Envelope struct {
	EventID       string      `json:"event_id"`
	EventType     string      `json:"event_type"`
	EventVersion  string      `json:"event_version"`
	SchemaVersion string      `json:"schema_version"`
	SourceService string      `json:"source_service"`
	EntityID      string      `json:"entity_id"`
	TenantID      string      `json:"tenant_id,omitempty"`
	ActorID       string      `json:"actor_id,omitempty"`
	CorrelationID string      `json:"correlation_id,omitempty"`
	OccurredAt    time.Time   `json:"occurred_at"`
	Payload       interface{} `json:"payload"`
}

// BuildPayload renders the Envelope JSON that Enqueue's Payload parameter
// expects, so a store method building a Record never has to know the wire
// shape itself.
func BuildPayload(eventType, entityID, tenantID, actorID, correlationID string, payload interface{}) ([]byte, error) {
	env := Envelope{
		EventID:       "evt-" + uuid.New().String(),
		EventType:     eventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "privacy-consent-svc",
		EntityID:      entityID,
		TenantID:      tenantID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("outbox.BuildPayload: %w", err)
	}
	return data, nil
}

// MessageWriter is the Kafka dependency the relay needs — narrow enough to
// fake in tests without a broker.
type MessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Relay drains event_outbox and publishes to Kafka, retrying with backoff
// until it succeeds. Nothing it does ever deletes a row or marks one
// published without Kafka actually having acknowledged it.
type Relay struct {
	pool        *pgxpool.Pool
	writer      MessageWriter
	log         *zap.Logger
	batchSize   int
	maxAttempts int

	published         int64
	failed            int64
	bookkeepingFailed int64
}

// DefaultBatchSize and DefaultMaxAttempts are conservative: a privacy
// evidence stream is low-volume, and the correctness property (never lose
// an event) matters far more than throughput.
const (
	DefaultBatchSize   = 50
	DefaultMaxAttempts = 50
)

func NewRelay(pool *pgxpool.Pool, writer MessageWriter, log *zap.Logger) *Relay {
	return &Relay{pool: pool, writer: writer, log: log, batchSize: DefaultBatchSize, maxAttempts: DefaultMaxAttempts}
}

// Stats reports what the relay has done, for the health/readiness surface.
func (r *Relay) Stats() (published, failed int64) {
	return r.published, r.failed
}

// BookkeepingFailures counts delivery-bookkeeping UPDATEs that did not
// persist (a published event that could not be marked published, or a
// failure reason that could not be recorded). Non-zero means events are
// being redelivered more than they should be.
func (r *Relay) BookkeepingFailures() int64 { return r.bookkeepingFailed }

// ErrNoPool is returned by a relay built without a database pool. A nil
// pool used to reach pool.Begin and panic the relay goroutine, taking the
// whole process down.
var ErrNoPool = errors.New("outbox relay has no database pool")

// Run polls at interval until ctx is cancelled. An unreachable broker is
// never fatal: this service's other paths (reads, idempotency) do not
// depend on Kafka, and a broker outage should degrade event delivery
// latency, not take the process down.
func (r *Relay) Run(ctx context.Context, interval time.Duration) {
	if r == nil {
		return
	}
	if r.log == nil {
		r.log = zap.NewNop()
	}
	if r.pool == nil {
		r.log.Warn("cannot start outbox relay: database pool is nil")
		return
	}
	r.log.Info("event outbox relay started", zap.Duration("poll_interval", interval))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("event outbox relay stopped",
				zap.Int64("published", r.published), zap.Int64("failed", r.failed))
			return
		case <-ticker.C:
			if _, err := r.DrainOnce(ctx); err != nil {
				r.log.Warn("outbox drain failed — will retry next tick", zap.Error(err))
			}
		}
	}
}

type claimedRow struct {
	id            string
	eventType     string
	tenantID      string
	correlationID string
	payload       []byte
	attempts      int
}

// DrainOnce claims one batch and publishes it. Claiming (an UPDATE that
// advances next_attempt_at) and the actual Kafka write are deliberately
// NOT in the same transaction: holding a row lock across a broker round
// trip would let one slow publish stall every other claim behind it. The
// claim's backoff is what stops a concurrent relay replica, or this same
// relay's next tick, from re-sending a message that is simply slow rather
// than failed.
func (r *Relay) DrainOnce(ctx context.Context) (int, error) {
	if r == nil || r.pool == nil {
		return 0, ErrNoPool
	}
	batch, err := r.claim(ctx)
	if err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}

	var bookkeepingErrs []error
	for _, row := range batch {
		msg := kafka.Message{Key: []byte(row.tenantID + ":" + row.correlationID), Value: row.payload}
		if row.correlationID == "" {
			msg.Key = []byte(row.id)
		}
		if err := r.writer.WriteMessages(ctx, msg); err != nil {
			r.failed++
			r.log.Warn("outbox publish failed — will retry",
				zap.String("outbox_event_id", row.id), zap.String("event_type", row.eventType),
				zap.Int("attempts", row.attempts), zap.Error(err))
			if rerr := r.recordFailure(ctx, row.id, err); rerr != nil {
				bookkeepingErrs = append(bookkeepingErrs, rerr)
			}
			if row.attempts >= r.maxAttempts {
				r.log.Error("outbox event exceeded max attempts — still retrying, now visible as a stuck event",
					zap.String("outbox_event_id", row.id), zap.String("event_type", row.eventType),
					zap.Int("attempts", row.attempts))
			}
			continue
		}
		r.published++
		if merr := r.markPublished(ctx, row.id); merr != nil {
			bookkeepingErrs = append(bookkeepingErrs, merr)
		}
	}
	return len(batch), errors.Join(bookkeepingErrs...)
}

func (r *Relay) claim(ctx context.Context) ([]claimedRow, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNoPool
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return nil, fmt.Errorf("claim: set app.outbox_relay: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT outbox_event_id, event_type, tenant_id, correlation_id, payload, attempts
		FROM event_outbox
		WHERE published_at IS NULL AND next_attempt_at <= NOW()
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`,
		r.batchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("claim: query: %w", err)
	}
	var batch []claimedRow
	for rows.Next() {
		var c claimedRow
		if err := rows.Scan(&c.id, &c.eventType, &c.tenantID, &c.correlationID, &c.payload, &c.attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("claim: scan: %w", err)
		}
		batch = append(batch, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim: rows: %w", err)
	}
	rows.Close()
	if len(batch) == 0 {
		return nil, tx.Commit(ctx)
	}

	ids := make([]string, len(batch))
	for i, c := range batch {
		ids[i] = c.id
	}
	// Pushing next_attempt_at forward NOW — before the publish is even
	// attempted — is what makes the claim real: a second relay (or this
	// one's next tick, if this publish takes longer than the tick interval)
	// will not see these rows again until the backoff elapses.
	if _, err := tx.Exec(ctx, `
		UPDATE event_outbox
		SET attempts = attempts + 1,
		    next_attempt_at = NOW() + (LEAST(POWER(2, attempts), 300) || ' seconds')::interval
		WHERE outbox_event_id = ANY($1)`,
		ids,
	); err != nil {
		return nil, fmt.Errorf("claim: mark in-flight: %w", err)
	}
	for i := range batch {
		batch[i].attempts++
	}
	return batch, tx.Commit(ctx)
}

// relayExec runs one bookkeeping UPDATE under the app.outbox_relay RLS
// capability and insists it touched exactly one row.
//
// Both bookkeeping statements used to run as a bare pool.Exec with no
// capability set. event_outbox is FORCE ROW LEVEL SECURITY and its policy
// only admits a row when app.tenant_id matches or app.outbox_relay = 'true',
// so under the restricted application role those UPDATEs matched ZERO rows
// and returned no error: a successfully delivered event was never marked
// published and was re-sent on every backoff expiry forever, and no failure
// reason was ever recorded. Checking RowsAffected turns that silent no-op
// into a loud error.
//
// The context is detached from the caller's cancellation (but bounded): a
// shutdown that cancels the relay context mid-batch must still be able to
// record that a message the broker already acknowledged was delivered.
func (r *Relay) relayExec(ctx context.Context, what, sql string, args ...any) error {
	if r == nil || r.pool == nil {
		return ErrNoPool
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	tx, err := r.pool.Begin(bctx)
	if err != nil {
		return fmt.Errorf("outbox %s: begin: %w", what, err)
	}
	defer func() { _ = tx.Rollback(bctx) }()

	// Transaction-local (is_local = true): the capability cannot leak onto
	// the pooled connection for the next caller.
	if _, err := tx.Exec(bctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return fmt.Errorf("outbox %s: set app.outbox_relay: %w", what, err)
	}
	tag, err := tx.Exec(bctx, sql, args...)
	if err != nil {
		return fmt.Errorf("outbox %s: %w", what, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("outbox %s: expected to update exactly 1 row, updated %d (row missing, or RLS hid it)", what, tag.RowsAffected())
	}
	if err := tx.Commit(bctx); err != nil {
		return fmt.Errorf("outbox %s: commit: %w", what, err)
	}
	return nil
}

func (r *Relay) markPublished(ctx context.Context, id string) error {
	err := r.relayExec(ctx, "markPublished", `
		UPDATE event_outbox SET published_at = NOW(), last_error = NULL
		WHERE outbox_event_id = $1 AND published_at IS NULL`, id)
	if err != nil {
		r.bookkeepingFailed++
		r.log.Error("broker acknowledged an outbox event but it could not be marked published — it will be redelivered once its backoff elapses (safe under at-least-once, but investigate)",
			zap.String("outbox_event_id", id), zap.Error(err))
	}
	return err
}

func (r *Relay) recordFailure(ctx context.Context, id string, cause error) error {
	err := r.relayExec(ctx, "recordFailure", `
		UPDATE event_outbox SET last_error = $2
		WHERE outbox_event_id = $1 AND published_at IS NULL`, id, cause.Error())
	if err != nil {
		r.bookkeepingFailed++
		r.log.Error("failed to persist outbox failure reason", zap.String("outbox_event_id", id), zap.Error(err))
	}
	return err
}
