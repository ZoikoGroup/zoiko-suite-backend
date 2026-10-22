// Package outbox implements the Transactional Outbox pattern for workflow-svc
// per ZS-STATE-001 Invariant I-13 and doc7 backlog item 32.
//
// An event is written to outbox_events inside the SAME database transaction
// as the authoritative workflow state mutation and transition history, guaranteeing
// that "the fact is durably true" is never decoupled from event publication.
//
// A background Relay then polls unpublished rows using FOR UPDATE SKIP LOCKED
// to provide multi-replica concurrency safety and at-least-once delivery.
// Each row names its destination: "kafka" (domain events) or "decision_log"
// (the governance ledger record of an approval decision, 000020), and one
// Relay serves one destination.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
)

// MaxPublishAttempts is how many consecutive failed deliveries park an event
// (it then needs an operator). With the backoff below — 2^n seconds, capped
// at five minutes — that is about 16 hours of continuous failure. It used to
// be 10 attempts at a 1.5 s poll: a Kafka outage of ~15 seconds stranded
// every pending event for good.
const MaxPublishAttempts = 200

// Destinations.
const (
	DestinationKafka       = "kafka"
	DestinationDecisionLog = "decision_log"
)

// Event is one row to be written to outbox_events inside a domain transaction.
type Event struct {
	OutboxEventID string
	AggregateType string
	AggregateID   string
	EventType     string
	TenantID      string
	LegalEntityID string
	ActorID       *string
	CorrelationID *string
	Headers       map[string]string
	Payload       any
	// Destination defaults to DestinationKafka.
	Destination string
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
	CorrelationID *string
	Headers       map[string]string
	Payload       json.RawMessage
	Attempts      int
	CreatedAt     time.Time
}

// Actor and Correlation are the nullable columns as plain strings.
func (e StoredEvent) Actor() string {
	if e.ActorID == nil {
		return ""
	}
	return *e.ActorID
}

// Correlation returns the correlation id, "" when unset.
func (e StoredEvent) Correlation() string {
	if e.CorrelationID == nil {
		return ""
	}
	return *e.CorrelationID
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
	if e.Destination == "" {
		e.Destination = DestinationKafka
	}
	payloadJSON, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}

	var headersJSON []byte
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
			tenant_id, legal_entity_id, actor_id, correlation_id, headers, payload, destination
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`
	_, err = tx.Exec(ctx, sql,
		e.OutboxEventID, e.AggregateType, e.AggregateID, e.EventType,
		e.TenantID, e.LegalEntityID, e.ActorID, e.CorrelationID, headersJSON, payloadJSON, e.Destination,
	)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// ErrPermanent marks a delivery failure no retry can fix (the destination
// already holds a different record under this id). The relay parks the event
// at once instead of retrying it for hours.
var ErrPermanent = errors.New("delivery refused permanently")

// Deliverer sends one stored event to the relay's destination.
type Deliverer interface {
	Deliver(ctx context.Context, e StoredEvent) error
}

// DelivererFunc adapts a function to Deliverer.
type DelivererFunc func(ctx context.Context, e StoredEvent) error

// Deliver calls f.
func (f DelivererFunc) Deliver(ctx context.Context, e StoredEvent) error { return f(ctx, e) }

// PgxPool defines the database interface needed by the Relay.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Relay polls outbox_events for one destination's unpublished, due rows and
// delivers them. FOR UPDATE SKIP LOCKED keeps replicas from claiming the same
// row; a failed delivery is retried with exponential backoff.
type Relay struct {
	pool        PgxPool
	deliverer   Deliverer
	destination string
	interval    time.Duration
	batchSize   int
	log         *zap.Logger
}

// NewRelay constructs a Relay for destination.
func NewRelay(pool PgxPool, deliverer Deliverer, destination string, interval time.Duration, batchSize int, log *zap.Logger) *Relay {
	if interval <= 0 {
		interval = 1500 * time.Millisecond
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	if destination == "" {
		destination = DestinationKafka
	}
	return &Relay{
		pool:        pool,
		deliverer:   deliverer,
		destination: destination,
		interval:    interval,
		batchSize:   batchSize,
		log:         log,
	}
}

// Start runs the polling loop until ctx is cancelled.
func (r *Relay) Start(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.log.Info("outbox relay: started",
		zap.String("destination", r.destination),
		zap.Duration("interval", r.interval),
		zap.Int("batch_size", r.batchSize),
	)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("outbox relay: stopping due to context cancellation", zap.String("destination", r.destination))
			return
		case <-ticker.C:
			r.RelayOnce(ctx)
		}
	}
}

// RelayOnce processes a single batch of due events for this destination.
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
		       payload, publish_attempts, created_at
		FROM outbox_events
		WHERE published_at IS NULL
		  AND destination = $3
		  AND next_attempt_at <= NOW()
		  AND publish_attempts < $2
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`
	rows, err := tx.Query(ctx, pollSQL, r.batchSize, MaxPublishAttempts, r.destination)
	if err != nil {
		r.log.Error("outbox relay: poll failed", zap.Error(err))
		return
	}

	var pending []StoredEvent
	for rows.Next() {
		var e StoredEvent
		var actorID, correlationID *string
		var headersJSON []byte
		if scanErr := rows.Scan(
			&e.OutboxEventID, &e.AggregateType, &e.AggregateID, &e.EventType,
			&e.TenantID, &e.LegalEntityID, &actorID, &correlationID, &headersJSON,
			&e.Payload, &e.Attempts, &e.CreatedAt,
		); scanErr != nil {
			r.log.Error("outbox relay: scan failed", zap.Error(scanErr))
			rows.Close()
			return
		}
		e.ActorID = actorID
		e.CorrelationID = correlationID
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
		if delErr := r.deliverer.Deliver(ctx, e); delErr != nil {
			newAttempts := e.Attempts + 1
			if errors.Is(delErr, ErrPermanent) {
				newAttempts = MaxPublishAttempts
			}
			if _, err := tx.Exec(ctx, `
				UPDATE outbox_events
				SET publish_attempts = GREATEST(publish_attempts + 1, $3),
				    last_error = $2,
				    next_attempt_at = NOW() + LEAST(POWER(2, LEAST(publish_attempts, 16)), 300) * INTERVAL '1 second'
				WHERE outbox_event_id = $1
			`, e.OutboxEventID, delErr.Error(), newAttempts); err != nil {
				r.log.Error("outbox relay: recording a failed delivery failed", zap.Error(err))
			}
			if newAttempts >= MaxPublishAttempts {
				r.log.Error("outbox relay: event parked after repeated failures — needs an operator",
					zap.String("destination", r.destination),
					zap.String("outbox_event_id", e.OutboxEventID),
					zap.String("event_type", e.EventType),
					zap.Int("publish_attempts", newAttempts),
					zap.Error(delErr),
				)
			} else {
				r.log.Warn("outbox relay: delivery failed, will retry with backoff",
					zap.String("destination", r.destination),
					zap.String("outbox_event_id", e.OutboxEventID),
					zap.String("event_type", e.EventType),
					zap.Int("publish_attempts", newAttempts),
					zap.Error(delErr),
				)
			}
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE outbox_events
			SET published_at = NOW(), last_error = NULL
			WHERE outbox_event_id = $1
		`, e.OutboxEventID); err != nil {
			r.log.Error("outbox relay: marking a delivery failed", zap.Error(err))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		r.log.Error("outbox relay: commit failed", zap.Error(err))
	}
}
