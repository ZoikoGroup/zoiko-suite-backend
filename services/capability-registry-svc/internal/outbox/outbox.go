package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
)

type Publisher interface {
	PublishOutbox(ctx context.Context, entityID string, payload []byte) error
}

type pool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type pendingEvent struct {
	id      string
	entity  string
	payload []byte
}

type Relay struct {
	db        pool
	publisher Publisher
	interval  time.Duration
	batchSize int
	logger    *zap.Logger
}

func NewRelay(db pool, publisher Publisher, interval time.Duration, batchSize int, logger *zap.Logger) *Relay {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	return &Relay{db: db, publisher: publisher, interval: interval, batchSize: batchSize, logger: logger}
}

func (r *Relay) Start(ctx context.Context) {
	if err := r.RelayOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		r.logger.Error("outbox relay cycle failed", zap.Error(err))
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.RelayOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				r.logger.Error("outbox relay cycle failed", zap.Error(err))
			}
		}
	}
}

func (r *Relay) RelayOnce(ctx context.Context) error {
	token := uuid.NewString()
	rows, err := r.db.Query(ctx, `
		WITH selected AS (
			SELECT outbox_event_id
			FROM outbox_events
			WHERE published_at IS NULL
			  AND (claimed_until IS NULL OR claimed_until < NOW())
			ORDER BY created_at, outbox_event_id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE outbox_events AS events
		SET claim_token = $1, claimed_until = NOW() + INTERVAL '30 seconds'
		FROM selected
		WHERE events.outbox_event_id = selected.outbox_event_id
		RETURNING events.outbox_event_id::text, events.entity_id, events.payload
	`, token, r.batchSize)
	if err != nil {
		return fmt.Errorf("claim pending outbox events: %w", err)
	}
	var pending []pendingEvent
	for rows.Next() {
		var event pendingEvent
		if err := rows.Scan(&event.id, &event.entity, &event.payload); err != nil {
			rows.Close()
			return fmt.Errorf("scan pending outbox event: %w", err)
		}
		pending = append(pending, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read pending outbox events: %w", err)
	}
	rows.Close()

	var failures []error
	for _, event := range pending {
		publishCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		publishErr := r.publisher.PublishOutbox(publishCtx, event.entity, event.payload)
		cancel()
		if publishErr != nil {
			if markErr := r.markFailed(ctx, token, event.id, publishErr); markErr != nil {
				failures = append(failures, markErr)
			}
			r.logger.Warn("outbox event publish failed; retained for retry",
				zap.String("outbox_event_id", event.id), zap.Error(publishErr))
			failures = append(failures, fmt.Errorf("publish outbox event %s: %w", event.id, publishErr))
			continue
		}
		if err := r.markPublished(ctx, token, event.id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (r *Relay) markPublished(ctx context.Context, token, eventID string) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE outbox_events
		SET published_at = NOW(), claim_token = NULL, claimed_until = NULL, last_error = NULL
		WHERE outbox_event_id = $1 AND claim_token = $2
	`, eventID, token)
	if err != nil {
		return fmt.Errorf("mark outbox event %s published: %w", eventID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("mark outbox event %s published: relay claim was lost", eventID)
	}
	return nil
}

func (r *Relay) markFailed(ctx context.Context, token, eventID string, publishErr error) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE outbox_events
		SET publish_attempts = publish_attempts + 1, last_error = $3,
		    claim_token = NULL, claimed_until = NULL
		WHERE outbox_event_id = $1 AND claim_token = $2
	`, eventID, token, publishErr.Error())
	if err != nil {
		return fmt.Errorf("record outbox event %s failure: %w", eventID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("record outbox event %s failure: relay claim was lost", eventID)
	}
	return nil
}
