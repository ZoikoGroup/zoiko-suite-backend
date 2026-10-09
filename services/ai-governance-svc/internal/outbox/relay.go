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

	"zoiko.io/ai-governance-svc/internal/events"
)

type EventPublisher interface {
	PublishEvent(context.Context, events.Event) error
}

type Relay struct {
	pool      *pgxpool.Pool
	publisher EventPublisher
	logger    *zap.Logger
	pollEvery time.Duration
	leaseFor  time.Duration
}

func NewRelay(pool *pgxpool.Pool, publisher EventPublisher, logger *zap.Logger) *Relay {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Relay{
		pool:      pool,
		publisher: publisher,
		logger:    logger,
		pollEvery: time.Second,
		leaseFor:  time.Minute,
	}
}

func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.pollEvery)
	defer ticker.Stop()

	for {
		worked, err := r.PublishOne(ctx)
		if err != nil {
			r.logger.Error("outbox relay attempt failed", zap.Error(err))
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Relay) PublishOne(ctx context.Context) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin outbox claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var row outboxRow
	err = tx.QueryRow(ctx, `
		SELECT o.outbox_id, o.event_id, o.event_type, o.aggregate_id,
			o.tenant_id, o.actor_id, o.correlation_id, o.occurred_at, o.payload, o.attempts
		FROM ai_governance_outbox o
		WHERE o.published_at IS NULL
			AND o.available_at <= NOW()
			AND (o.lease_until IS NULL OR o.lease_until <= NOW())
			AND NOT EXISTS (
				SELECT 1
				FROM ai_governance_outbox previous
				WHERE previous.aggregate_type = o.aggregate_type
					AND previous.aggregate_id = o.aggregate_id
					AND previous.outbox_id < o.outbox_id
					AND previous.published_at IS NULL
			)
		ORDER BY o.outbox_id
		FOR UPDATE OF o SKIP LOCKED
		LIMIT 1
	`).Scan(
		&row.id, &row.eventID, &row.eventType, &row.entityID,
		&row.tenantID, &row.actorID, &row.correlationID, &row.occurredAt, &row.payload, &row.attempts,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim outbox event: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE ai_governance_outbox
		SET lease_until = NOW() + $2 * INTERVAL '1 second', attempts = attempts + 1
		WHERE outbox_id = $1
	`, row.id, r.leaseFor.Seconds()); err != nil {
		return false, fmt.Errorf("lease outbox event %s: %w", row.eventID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit outbox lease %s: %w", row.eventID, err)
	}

	var payload any
	if err := json.Unmarshal(row.payload, &payload); err != nil {
		return true, r.deferRetry(ctx, row, fmt.Errorf("decode outbox payload: %w", err))
	}
	evt := events.Event{
		EventID:       row.eventID,
		EventType:     row.eventType,
		EventVersion:  "1.0",
		SchemaVersion: "1.0",
		SourceService: "ai-governance-svc",
		EntityID:      row.entityID,
		TenantID:      row.tenantID,
		ActorID:       row.actorID,
		CorrelationID: row.correlationID,
		OccurredAt:    row.occurredAt,
		Payload:       payload,
	}
	if err := r.publisher.PublishEvent(ctx, evt); err != nil {
		return true, r.deferRetry(ctx, row, err)
	}
	if _, err := r.pool.Exec(ctx, `
		UPDATE ai_governance_outbox
		SET published_at = NOW(), lease_until = NULL, last_error = NULL
		WHERE outbox_id = $1 AND published_at IS NULL
	`, row.id); err != nil {
		return true, fmt.Errorf("mark outbox event %s published: %w", row.eventID, err)
	}
	return true, nil
}

type outboxRow struct {
	id            int64
	eventID       string
	eventType     string
	entityID      string
	tenantID      string
	actorID       string
	correlationID string
	occurredAt    time.Time
	payload       []byte
	attempts      int
}

func (r *Relay) deferRetry(ctx context.Context, row outboxRow, cause error) error {
	attempt := row.attempts + 1
	backoffSeconds := math.Min(math.Pow(2, float64(min(attempt, 8))), 300)
	_, err := r.pool.Exec(ctx, `
		UPDATE ai_governance_outbox
		SET available_at = NOW() + make_interval(secs => $2),
			lease_until = NULL,
			last_error = $3
		WHERE outbox_id = $1 AND published_at IS NULL
	`, row.id, backoffSeconds, cause.Error())
	if err != nil {
		return fmt.Errorf("record retry for outbox event %s after %v: %w", row.eventID, cause, err)
	}
	return fmt.Errorf("outbox event %s will retry: %w", row.eventID, cause)
}
