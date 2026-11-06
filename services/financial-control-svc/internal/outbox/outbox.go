// Package outbox implements the Transactional Outbox pattern for financial-control-svc
// per ZS-EVENT-001 (durable event facts) and ZS-CONTROL-001 §26.
//
// An event is written to outbox_events inside the SAME database transaction
// as the authoritative workflow state mutation and transition history, guaranteeing
// that "the fact is durably true" is never decoupled from event publication.
//
// A background Relay then polls unpublished rows using FOR UPDATE SKIP LOCKED
// to provide multi-replica concurrency safety and at-least-once delivery to Kafka.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
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
	CorrelationID *string
	Headers       map[string]string
	Payload       any
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

// Insert writes e into outbox_events using the caller's active pgx.Tx transaction.
// It MUST be called within the same transaction that applies the business mutation.
func Insert(ctx context.Context, tx pgx.Tx, e Event) error {
	if tx == nil {
		return fmt.Errorf("outbox insert: transaction is nil")
	}
	if e.OutboxEventID == "" {
		// UUIDv7 (ZS-EVENT-001 s5): time-ordered and immutable once allocated here.
		if id, err := uuid.NewV7(); err == nil {
			e.OutboxEventID = id.String()
		} else {
			e.OutboxEventID = uuid.NewString()
		}
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
// Delivery is one outbox row handed to the broker publisher.
type Delivery struct {
	OutboxEventID string
	AggregateType string
	AggregateID   string
	EventType     string
	CorrelationID string
	TenantID      string
	LegalEntityID string
	ActorID       string
	// OccurredAt is when the fact was committed (the outbox row's created_at), not relay time.
	OccurredAt time.Time
	Payload    []byte
}

type Publisher interface {
	PublishOutbox(ctx context.Context, d Delivery) error
}

// Prober reports whether the Kafka broker backing this relay's publisher is
// currently reachable. The Relay uses it to tell "the broker is down" apart
// from "this specific event was rejected": without that distinction, a
// broker outage spends every event's publish_attempts on retries that can
// never succeed, and the first events to reach MaxPublishAttempts are
// permanently dead-lettered financial control facts (run certifications,
// exceptions) that were never actually malformed — only unlucky enough to
// be polled during the outage.
type Prober interface {
	Probe(ctx context.Context) error
}

// PgxPool defines the database interface needed by the Relay.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Relay polls outbox_events for unpublished rows and publishes them to Kafka.
// Uses FOR UPDATE SKIP LOCKED to prevent duplicate publishes across multiple replicas.
type Relay struct {
	pool      PgxPool
	publisher Publisher
	interval  time.Duration
	batchSize int
	prober    Prober
	log       *zap.Logger
}

// WithProber attaches a broker-reachability check. Without one, the Relay
// keeps its original behaviour of charging every failed publish against the
// event's attempt budget.
func (r *Relay) WithProber(p Prober) *Relay {
	r.prober = p
	return r
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
		       payload, publish_attempts, created_at
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

	brokerDown := false
	for i, e := range pending {
		if brokerDown {
			// Already confirmed down this tick: every remaining event would fail
			// for the same reason. Leave them untouched for the next tick rather
			// than spending each one's own probe and attempt budget.
			continue
		}

		actor := ""
		if e.ActorID != nil {
			actor = *e.ActorID
		}
		correlation := ""
		if e.CorrelationID != nil {
			correlation = *e.CorrelationID
		}

		pubErr := r.publisher.PublishOutbox(ctx, Delivery{OutboxEventID: e.OutboxEventID, AggregateType: e.AggregateType,
			AggregateID: e.AggregateID, EventType: e.EventType, CorrelationID: correlation, TenantID: e.TenantID,
			LegalEntityID: e.LegalEntityID, ActorID: actor, OccurredAt: e.CreatedAt, Payload: e.Payload})
		if pubErr == nil {
			_, _ = tx.Exec(ctx, `
				UPDATE outbox_events
				SET published_at = NOW()
				WHERE outbox_event_id = $1
			`, e.OutboxEventID)
			continue
		}

		if r.prober != nil {
			if probeErr := r.prober.Probe(ctx); probeErr != nil {
				// The broker itself is unreachable, not just this event. Charging
				// publish_attempts here would eventually dead-letter a genuine,
				// well-formed control fact purely because of an outage's timing.
				// Release this and every remaining event in the batch uncharged;
				// they are retried from scratch next tick.
				brokerDown = true
				r.log.Warn("outbox relay: broker unreachable, releasing batch without charging attempts",
					zap.Int("released", len(pending)-i),
					zap.Error(probeErr),
				)
				continue
			}
		}

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
	}

	if err := tx.Commit(ctx); err != nil {
		r.log.Error("outbox relay: commit failed", zap.Error(err))
	}
}
