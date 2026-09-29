// Package outbox implements the Transactional Outbox pattern for accounts-receivable-svc
// per ZS-STATE-001 Invariant I-13.
//
// An event is written to outbox_events inside the SAME database transaction as the
// authoritative customer invoice mutation, guaranteeing state mutations and event
// publication are atomic.
//
// A background Relay polls unpublished rows using FOR UPDATE SKIP LOCKED
// to provide multi-replica concurrency safety and at-least-once delivery to Kafka.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
)

// MaxPublishAttempts defines the maximum number of publish attempts before an event
// is considered dead-lettered and excluded from active polling cycles.
const MaxPublishAttempts = 10

// Event is one row to be written to outbox_events inside a domain transaction.
type Event struct {
	OutboxEventID string
	AggregateType string
	AggregateID   string
	EventType     string
	TenantID      string
	LegalEntityID string
	ActorID       *string
	CorrelationID string
	Headers       map[string]string
	Payload       any
}

// VariantBEnvelope preserves the existing accounts-receivable-svc Kafka event contract.
// CRITICAL: Variant B explicitly includes event_id in the JSON body, formatted as "evt-" + outbox_event_id.
type VariantBEnvelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id,omitempty"`
	LegalEntityID string          `json:"legal_entity_id,omitempty"`
	ActorID       string          `json:"actor_id,omitempty"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// NewVariantBEnvelope constructs a Variant B envelope matching the accounts-receivable-svc platform contract.
// The event_id in the JSON body is strictly formatted as "evt-" + outboxEventID.
func NewVariantBEnvelope(outboxEventID, eventType, correlationID, tenantID, legalEntityID, actorID string, payload any) (VariantBEnvelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return VariantBEnvelope{}, fmt.Errorf("marshal inner event payload: %w", err)
	}
	eventID := outboxEventID
	if !strings.HasPrefix(eventID, "evt-") {
		eventID = "evt-" + eventID
	}
	return VariantBEnvelope{
		EventID:       eventID,
		EventType:     eventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "accounts-receivable-svc",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actorID,
		CorrelationID: correlationID,
		Payload:       raw,
	}, nil
}

// StoredEvent represents an unpublished event row fetched for relaying.
type StoredEvent struct {
	OutboxEventID string
	AggregateType string
	AggregateID   string
	EventType     string
	TenantID      string
	LegalEntityID string
	ActorID       *string
	CorrelationID string
	Headers       map[string]string
	Payload       json.RawMessage
	Attempts      int
}

// Insert writes e into outbox_events using the caller's active pgx.Tx transaction.
// It MUST be called within the same transaction that applies the business mutation.
func Insert(ctx context.Context, tx pgx.Tx, e Event) error {
	if tx == nil {
		return fmt.Errorf("outbox insert: transaction is nil")
	}
	if e.OutboxEventID == "" {
		e.OutboxEventID = uuid.NewString()
	}
	payloadJSON, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}

	headersJSON := []byte("{}")
	if len(e.Headers) > 0 {
		h, err := json.Marshal(e.Headers)
		if err != nil {
			return fmt.Errorf("marshal outbox headers: %w", err)
		}
		headersJSON = h
	}

	const sql = `
		INSERT INTO outbox_events (
			outbox_event_id, aggregate_type, aggregate_id, event_type,
			tenant_id, legal_entity_id, actor_id, correlation_id, headers, payload
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`
	_, err = tx.Exec(ctx, sql,
		e.OutboxEventID, e.AggregateType, e.AggregateID, e.EventType,
		e.TenantID, e.LegalEntityID, e.ActorID, e.CorrelationID, headersJSON, payloadJSON,
	)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// Publisher is the narrow interface the Relay depends on to emit events to Kafka.
type Publisher interface {
	PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error
}

// PgxPool defines the database interface needed by the Relay.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Relay polls outbox_events for unpublished rows and publishes them to Kafka.
// Uses FOR UPDATE SKIP LOCKED to prevent duplicate publishes across multiple replicas.
// Delivery semantics are strictly AT-LEAST-ONCE.
type Relay struct {
	pool      PgxPool
	publisher Publisher
	interval  time.Duration
	batchSize int
	log       *zap.Logger
}

// NewRelay constructs a new outbox Relay worker.
func NewRelay(pool PgxPool, publisher Publisher, interval time.Duration, batchSize int, log *zap.Logger) *Relay {
	if interval <= 0 {
		interval = 1500 * time.Millisecond
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	return &Relay{
		pool:      pool,
		publisher: publisher,
		interval:  interval,
		batchSize: batchSize,
		log:       log,
	}
}

// Start runs the polling loop until ctx is cancelled.
func (r *Relay) Start(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.log.Info("outbox relay: started",
		zap.Duration("interval", r.interval),
		zap.Int("batch_size", r.batchSize),
	)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("outbox relay: stopping due to context cancellation")
			return
		case <-ticker.C:
			r.RelayOnce(ctx)
		}
	}
}

// RelayOnce processes a single batch of unpublished outbox events using FOR UPDATE SKIP LOCKED.
func (r *Relay) RelayOnce(ctx context.Context) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		r.log.Error("outbox relay: begin tx failed", zap.Error(err))
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const pollSQL = `
		SELECT outbox_event_id, aggregate_type, aggregate_id, event_type,
		       tenant_id, legal_entity_id, actor_id, correlation_id, headers,
		       payload, publish_attempts
		FROM outbox_events
		WHERE published_at IS NULL
		  AND publish_attempts < $2
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`
	rows, err := tx.Query(ctx, pollSQL, r.batchSize, MaxPublishAttempts)
	if err != nil {
		r.log.Error("outbox relay: poll failed", zap.Error(err))
		return
	}

	var pending []StoredEvent
	for rows.Next() {
		var e StoredEvent
		var actorID *string
		var headersJSON []byte
		if scanErr := rows.Scan(
			&e.OutboxEventID, &e.AggregateType, &e.AggregateID, &e.EventType,
			&e.TenantID, &e.LegalEntityID, &actorID, &e.CorrelationID, &headersJSON,
			&e.Payload, &e.Attempts,
		); scanErr != nil {
			r.log.Error("outbox relay: scan failed", zap.Error(scanErr))
			rows.Close()
			return
		}
		e.ActorID = actorID
		if len(headersJSON) > 0 {
			_ = json.Unmarshal(headersJSON, &e.Headers)
		}
		pending = append(pending, e)
	}
	rows.Close()

	if len(pending) == 0 {
		return
	}

	for _, e := range pending {
		if pubErr := r.publisher.PublishOutbox(ctx, e.OutboxEventID, e.AggregateID, e.Payload); pubErr != nil {
			newAttempts := e.Attempts + 1
			_, _ = tx.Exec(ctx, `
				UPDATE outbox_events
				SET publish_attempts = publish_attempts + 1, last_error = $2
				WHERE outbox_event_id = $1
			`, e.OutboxEventID, pubErr.Error())
			if newAttempts >= MaxPublishAttempts {
				r.log.Error("outbox relay: event reached dead-letter ceiling, moving to dead-letter",
					zap.String("outbox_event_id", e.OutboxEventID),
					zap.String("event_type", e.EventType),
					zap.Int("publish_attempts", newAttempts),
					zap.Int("max_publish_attempts", MaxPublishAttempts),
					zap.Error(pubErr),
				)
			} else {
				r.log.Warn("outbox relay: publish failed, will retry",
					zap.String("outbox_event_id", e.OutboxEventID),
					zap.String("event_type", e.EventType),
					zap.Int("publish_attempts", newAttempts),
					zap.Error(pubErr),
				)
			}
		} else {
			_, _ = tx.Exec(ctx, `
				UPDATE outbox_events
				SET published_at = now(), publish_attempts = publish_attempts + 1, last_error = NULL
				WHERE outbox_event_id = $1
			`, e.OutboxEventID)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		r.log.Error("outbox relay: commit failed", zap.Error(err))
	}
}
