// Package outbox implements the transactional outbox for this service.
//
// An event is INSERTed into outbox_events inside the SAME transaction as the
// state change it describes (Insert takes the caller's pgx.Tx), so the change
// and the notice that it happened commit or roll back together. The Relay then
// drains unpublished rows to Kafka with FOR UPDATE SKIP LOCKED: delivery may
// fail and be retried (at-least-once), the fact is never lost, and a failed
// broker never fails or silently swallows a business command — which is what
// the previous fire-and-forget publish after commit did.
//
// This file is deliberately free of service-specific imports so the same
// package can be copied verbatim into sibling services. Pattern:
// payment-authorization-svc/internal/outbox.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
)

// MaxPublishAttempts is the number of failed deliveries after which a row stops
// being polled (it stays in the table, visible, for an operator to inspect).
const MaxPublishAttempts = 10

// Event is one row to be written inside a domain transaction.
type Event struct {
	OutboxEventID string
	AggregateType string
	AggregateID   string
	EventType     string
	TenantID      string
	LegalEntityID string
	ActorID       string
	CorrelationID string
	// DedupeKey, when set, makes the insert idempotent: a second Insert with the
	// same (tenant, event type, dedupe key) is a no-op instead of a duplicate
	// row. Used for facts that must be emitted at most once per business object
	// whatever happens on replay.
	DedupeKey string
	Payload   any
}

// Envelope is the platform event envelope (Doc 03 §19), written as the outbox
// payload at transaction time so it carries the actor and correlation of the
// request that caused the change rather than of the relay that delivers it.
type Envelope struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SchemaVersion string `json:"schema_version"`
	SourceService string `json:"source_service"`
	// SourceVersion is the version of the source object the event describes
	// (spec §17: procurement events carry the source version).
	SourceVersion int       `json:"source_version,omitempty"`
	TenantID      string    `json:"tenant_id,omitempty"`
	LegalEntityID string    `json:"legal_entity_id,omitempty"`
	ActorID       string    `json:"actor_id,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	EmittedAt     time.Time `json:"emitted_at"`
	Payload       any       `json:"payload"`
}

// Insert writes e using the caller's transaction. It MUST be called inside the
// transaction that applies the business mutation. Returns inserted=false when
// a DedupeKey collided (the event already exists).
func Insert(ctx context.Context, tx pgx.Tx, e Event) (inserted bool, err error) {
	if tx == nil {
		return false, fmt.Errorf("outbox insert: transaction is nil")
	}
	if e.OutboxEventID == "" {
		e.OutboxEventID = uuid.NewString()
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return false, fmt.Errorf("marshal outbox payload: %w", err)
	}
	var dedupe, corr, actor *string
	if e.DedupeKey != "" {
		dedupe = &e.DedupeKey
	}
	if e.CorrelationID != "" {
		corr = &e.CorrelationID
	}
	if e.ActorID != "" {
		actor = &e.ActorID
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (
			outbox_event_id, aggregate_type, aggregate_id, event_type,
			tenant_id, legal_entity_id, actor_id, correlation_id, dedupe_key, payload
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id, event_type, dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
		e.OutboxEventID, e.AggregateType, e.AggregateID, e.EventType,
		e.TenantID, e.LegalEntityID, actor, corr, dedupe, payload)
	if err != nil {
		return false, fmt.Errorf("insert outbox event: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// NewEnvelope builds the envelope for an event about to be inserted.
func NewEnvelope(outboxEventID, source, eventType string, e Event, sourceVersion int) Envelope {
	return Envelope{
		EventID:       "evt-" + outboxEventID,
		EventType:     eventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: source,
		SourceVersion: sourceVersion,
		TenantID:      e.TenantID,
		LegalEntityID: e.LegalEntityID,
		ActorID:       e.ActorID,
		CorrelationID: e.CorrelationID,
		EmittedAt:     time.Now().UTC(),
		Payload:       e.Payload,
	}
}

// Publisher is the narrow interface the Relay needs to emit to Kafka.
type Publisher interface {
	PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error
}

// PgxPool is the database surface the Relay needs.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Relay polls outbox_events and publishes unpublished rows. FOR UPDATE SKIP
// LOCKED keeps several replicas from publishing the same row twice.
type Relay struct {
	pool      PgxPool
	publisher Publisher
	interval  time.Duration
	batchSize int
	log       *zap.Logger
}

// NewRelay constructs a Relay.
func NewRelay(pool PgxPool, publisher Publisher, interval time.Duration, batchSize int, log *zap.Logger) *Relay {
	if interval <= 0 {
		interval = 1500 * time.Millisecond
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	return &Relay{pool: pool, publisher: publisher, interval: interval, batchSize: batchSize, log: log}
}

// Start runs the polling loop until ctx is cancelled.
func (r *Relay) Start(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	r.log.Info("outbox relay started", zap.Duration("interval", r.interval), zap.Int("batch_size", r.batchSize))
	for {
		select {
		case <-ctx.Done():
			r.log.Info("outbox relay stopped")
			return
		case <-t.C:
			r.RelayOnce(ctx)
		}
	}
}

// RelayOnce publishes one batch and returns how many rows were delivered.
func (r *Relay) RelayOnce(ctx context.Context) int {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		r.log.Error("outbox relay: begin failed", zap.Error(err))
		return 0
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT outbox_event_id, aggregate_id, event_type, payload, publish_attempts
		FROM outbox_events
		WHERE published_at IS NULL AND publish_attempts < $2
		ORDER BY created_at ASC, outbox_event_id ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, r.batchSize, MaxPublishAttempts)
	if err != nil {
		r.log.Error("outbox relay: poll failed", zap.Error(err))
		return 0
	}
	type pending struct {
		id, aggregateID, eventType string
		payload                    []byte
		attempts                   int
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.aggregateID, &p.eventType, &p.payload, &p.attempts); err != nil {
			rows.Close()
			r.log.Error("outbox relay: scan failed", zap.Error(err))
			return 0
		}
		batch = append(batch, p)
	}
	rows.Close()
	if rows.Err() != nil {
		r.log.Error("outbox relay: poll iteration failed", zap.Error(rows.Err()))
		return 0
	}

	delivered := 0
	for _, p := range batch {
		if pubErr := r.publisher.PublishOutbox(ctx, p.id, p.aggregateID, p.payload); pubErr != nil {
			_, _ = tx.Exec(ctx, `UPDATE outbox_events SET publish_attempts = publish_attempts + 1, last_error = $2 WHERE outbox_event_id = $1`, p.id, pubErr.Error())
			if p.attempts+1 >= MaxPublishAttempts {
				r.log.Error("outbox relay: event reached dead-letter ceiling", zap.String("outbox_event_id", p.id), zap.String("event_type", p.eventType), zap.Error(pubErr))
			} else {
				r.log.Warn("outbox relay: publish failed, will retry", zap.String("outbox_event_id", p.id), zap.String("event_type", p.eventType), zap.Error(pubErr))
			}
			continue
		}
		delivered++
		_, _ = tx.Exec(ctx, `UPDATE outbox_events SET published_at = now() WHERE outbox_event_id = $1`, p.id)
	}
	if err := tx.Commit(ctx); err != nil {
		r.log.Error("outbox relay: commit failed", zap.Error(err))
		return 0
	}
	return delivered
}
