// Package outbox implements the transactional outbox for this service.
//
// An event is INSERTed into outbox_events inside the SAME transaction as the
// state change it describes (Insert takes the caller's pgx.Tx), so the change
// and the notice that it happened commit or roll back together. The Relay then
// drains unpublished rows to Kafka with FOR UPDATE SKIP LOCKED: delivery may
// fail and be retried (at-least-once), the fact is never lost, and a failed
// broker never fails a business command.
//
// Shape copied from purchase-request-svc/internal/outbox. Differences: tenant
// and legal entity are text (this service's tables key tenants as VARCHAR),
// the platform envelope is built by Insert itself, and the relay's
// bookkeeping UPDATEs are checked rather than discarded.
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

// SchemaVersion is carried on every envelope (the audit consumer requires it).
const SchemaVersion = "1.0"

// Event is one row to be written inside a domain transaction.
type Event struct {
	AggregateType string
	AggregateID   string
	EventType     string
	TenantID      string
	LegalEntityID string
	ActorID       string
	CorrelationID string
	Payload       any
}

// Envelope is the platform event envelope written as the outbox payload at
// transaction time, so it carries the actor and correlation of the request
// that caused the change rather than of the relay that delivers it.
type Envelope struct {
	EventID       string    `json:"event_id"`
	EventType     string    `json:"event_type"`
	EventVersion  string    `json:"event_version"`
	SchemaVersion string    `json:"schema_version"`
	SourceService string    `json:"source_service"`
	SubjectID     string    `json:"subject_id,omitempty"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id,omitempty"`
	ActorID       string    `json:"actor_id,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`
	Payload       any       `json:"payload"`
}

// Insert writes e, wrapped in the platform envelope, using the caller's
// transaction. It MUST be called inside the transaction that applies the
// business mutation. It returns the outbox event id.
func Insert(ctx context.Context, tx pgx.Tx, sourceService string, e Event) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("outbox insert: transaction is nil")
	}
	if e.TenantID == "" || e.EventType == "" || e.AggregateID == "" {
		return "", fmt.Errorf("outbox insert: tenant, event type and aggregate id are required")
	}
	id := uuid.NewString()
	env := Envelope{
		EventID:       "evt-" + id,
		EventType:     e.EventType,
		EventVersion:  SchemaVersion,
		SchemaVersion: SchemaVersion,
		SourceService: sourceService,
		SubjectID:     e.AggregateID,
		TenantID:      e.TenantID,
		LegalEntityID: e.LegalEntityID,
		ActorID:       e.ActorID,
		CorrelationID: e.CorrelationID,
		OccurredAt:    time.Now().UTC(),
		Payload:       e.Payload,
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("marshal outbox payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (
			outbox_event_id, aggregate_type, aggregate_id, event_type,
			tenant_id, legal_entity_id, actor_id, correlation_id, payload
		) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9)`,
		id, e.AggregateType, e.AggregateID, e.EventType,
		e.TenantID, e.LegalEntityID, e.ActorID, e.CorrelationID, payload); err != nil {
		return "", fmt.Errorf("insert outbox event: %w", err)
	}
	return id, nil
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
			if _, err := r.RelayOnce(ctx); err != nil {
				r.log.Error("outbox relay pass failed", zap.Error(err))
			}
		}
	}
}

// RelayOnce publishes one batch and returns how many rows were delivered. A
// failed publish is recorded on the row (attempts, last_error) and retried on
// a later pass; a database failure is returned.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The relay drains every tenant's backlog; outbox_events is under FORCE
	// RLS, so it declares itself (the policy's relay disjunct) instead of
	// running unscoped, which would see no rows at all. Transaction-local.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return 0, fmt.Errorf("set relay scope: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT outbox_event_id, aggregate_id, event_type, payload, publish_attempts
		FROM outbox_events
		WHERE published_at IS NULL AND publish_attempts < $2
		ORDER BY created_at ASC, outbox_event_id ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, r.batchSize, MaxPublishAttempts)
	if err != nil {
		return 0, fmt.Errorf("poll: %w", err)
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
			return 0, fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("poll iteration: %w", err)
	}

	delivered := 0
	for _, p := range batch {
		if pubErr := r.publisher.PublishOutbox(ctx, p.id, p.aggregateID, p.payload); pubErr != nil {
			if _, err := tx.Exec(ctx, `UPDATE outbox_events SET publish_attempts = publish_attempts + 1, last_error = $2 WHERE outbox_event_id = $1`, p.id, pubErr.Error()); err != nil {
				return 0, fmt.Errorf("record failed attempt: %w", err)
			}
			if p.attempts+1 >= MaxPublishAttempts {
				r.log.Error("outbox relay: event reached dead-letter ceiling", zap.String("outbox_event_id", p.id), zap.String("event_type", p.eventType), zap.Error(pubErr))
			} else {
				r.log.Warn("outbox relay: publish failed, will retry", zap.String("outbox_event_id", p.id), zap.String("event_type", p.eventType), zap.Error(pubErr))
			}
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox_events SET published_at = now() WHERE outbox_event_id = $1`, p.id); err != nil {
			// Rolled back: the rows stay unpublished and are re-sent on the
			// next pass (at-least-once; consumers de-duplicate on X-Event-ID).
			return 0, fmt.Errorf("mark published: %w", err)
		}
		delivered++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return delivered, nil
}
