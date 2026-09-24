package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/webhook"
)

var (
	ErrAttemptNotFound = webhook.ErrAttemptNotFound
	ErrDLQItemNotFound = errors.New("dlq item not found")
)

// LookupAttemptByProviderMessageID finds the delivery attempt matching a provider message ID or attempt UUID.
// It executes under app.platform_scope (SELECT-only) as a cross-tenant discovery hatch,
// ensuring the subsequent write can be scoped strictly to the discovered tenant.
func (s *PgStore) LookupAttemptByProviderMessageID(ctx context.Context, providerMessageID string) (*webhook.AttemptLookupResult, error) {
	if strings.TrimSpace(providerMessageID) == "" {
		return nil, ErrAttemptNotFound
	}

	trimmed := strings.TrimSpace(providerMessageID)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set platform scope: %w", err)
	}

	const query = `
		SELECT provider_attempt_id, message_intent_id, tenant_id, sender_stream, to_address
		FROM delivery_attempts
		WHERE provider_message_id = $1
		   OR provider_attempt_id::text = $1
		ORDER BY attempted_at DESC
		LIMIT 1;
	`
	var res webhook.AttemptLookupResult
	err = tx.QueryRow(ctx, query, trimmed).Scan(
		&res.ProviderAttemptID,
		&res.MessageIntentID,
		&res.TenantID,
		&res.SenderStream,
		&res.RecipientAddress,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAttemptNotFound
		}
		return nil, fmt.Errorf("lookup delivery attempt: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit platform scope tx: %w", err)
	}

	return &res, nil
}

// RecordDeliveryEventIdempotent records an external webhook delivery event under tenant RLS.
// It returns (true, nil) if newly inserted, or (false, nil) if already recorded (idempotent duplicate).
func (s *PgStore) RecordDeliveryEventIdempotent(ctx context.Context, event *ledger.DeliveryEvent) (bool, error) {
	if event.DeliveryEventID == "" || event.ProviderAttemptID == "" || event.MessageIntentID == "" || event.TenantID == "" {
		return false, fmt.Errorf("event_id, attempt_id, intent_id, and tenant_id are required")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}

	var inserted bool
	err := s.withRLS(ctx, event.TenantID, func(tx pgx.Tx) error {
		// 1. Check for existing identical event for idempotency
		const checkSQL = `
			SELECT 1 FROM delivery_events
			WHERE tenant_id = $1
			  AND (delivery_event_id = $2 OR (provider_attempt_id = $3 AND event_type = $4))
			LIMIT 1;
		`
		var exists int
		err := tx.QueryRow(ctx, checkSQL, event.TenantID, event.DeliveryEventID, event.ProviderAttemptID, string(event.EventType)).Scan(&exists)
		if err == nil {
			// Already recorded: idempotent success
			inserted = false
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check delivery_event exists: %w", err)
		}

		// 2. Insert new event
		const insertSQL = `
			INSERT INTO delivery_events (
				delivery_event_id, provider_attempt_id, message_intent_id,
				tenant_id, event_type, raw_payload, occurred_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7);
		`
		_, err = tx.Exec(ctx, insertSQL,
			event.DeliveryEventID, event.ProviderAttemptID, event.MessageIntentID,
			event.TenantID, string(event.EventType), event.RawPayload, event.OccurredAt,
		)
		if err != nil {
			return fmt.Errorf("insert delivery_event: %w", err)
		}

		// 3. Update delivery attempt status accordingly
		if event.EventType == ledger.DeliveryEventBounced || event.EventType == ledger.DeliveryEventDropped {
			const updateAttempt = `
				UPDATE delivery_attempts
				SET status = 'FAILED'
				WHERE tenant_id = $1 AND provider_attempt_id = $2;
			`
			_, _ = tx.Exec(ctx, updateAttempt, event.TenantID, event.ProviderAttemptID)
		} else if event.EventType == ledger.DeliveryEventDelivered {
			const updateAttempt = `
				UPDATE delivery_attempts
				SET status = 'ACCEPTED'
				WHERE tenant_id = $1 AND provider_attempt_id = $2;
			`
			_, _ = tx.Exec(ctx, updateAttempt, event.TenantID, event.ProviderAttemptID)
		}

		inserted = true
		return nil
	})

	if err != nil {
		return false, err
	}
	return inserted, nil
}

// RouteToDLQ writes an unprocessable or failing webhook event into webhook_dlq.
// It preserves RLS by utilizing the provided tenant or a system fallback tenant.
func (s *PgStore) RouteToDLQ(ctx context.Context, item *webhook.DLQItem) error {
	tenantID := strings.TrimSpace(item.TenantID)
	if tenantID == "" {
		tenantID = "SYSTEM_UNRESOLVED"
	}
	if item.DLQID == "" {
		item.DLQID = uuid.NewString()
	}
	if item.ReceivedAt.IsZero() {
		item.ReceivedAt = time.Now().UTC()
	}
	if item.Status == "" {
		item.Status = webhook.DLQStatusFailed
	}
	item.TenantID = tenantID

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const sql = `
			INSERT INTO webhook_dlq (
				dlq_id, tenant_id, provider_name, event_type, raw_payload,
				error_reason, is_retryable, retry_count, next_retry_at, status,
				received_at, last_processed_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);
		`
		_, err := tx.Exec(ctx, sql,
			item.DLQID, item.TenantID, item.ProviderName, item.EventType, item.RawPayload,
			item.ErrorReason, item.IsRetryable, item.RetryCount, item.NextRetryAt, string(item.Status),
			item.ReceivedAt, item.LastProcessedAt,
		)
		if err != nil {
			return fmt.Errorf("insert webhook_dlq: %w", err)
		}
		return nil
	})
}

// GetDLQItem retrieves a DLQ item by ID under tenant RLS.
func (s *PgStore) GetDLQItem(ctx context.Context, tenantID, dlqID string) (*webhook.DLQItem, error) {
	if strings.TrimSpace(tenantID) == "" {
		tenantID = "SYSTEM_UNRESOLVED"
	}

	var item webhook.DLQItem
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const sql = `
			SELECT dlq_id, tenant_id, provider_name, event_type, raw_payload,
			       error_reason, is_retryable, retry_count, next_retry_at, status,
			       received_at, last_processed_at
			FROM webhook_dlq
			WHERE tenant_id = $1 AND dlq_id = $2;
		`
		var statusStr string
		var eventType *string
		err := tx.QueryRow(ctx, sql, tenantID, dlqID).Scan(
			&item.DLQID, &item.TenantID, &item.ProviderName, &eventType, &item.RawPayload,
			&item.ErrorReason, &item.IsRetryable, &item.RetryCount, &item.NextRetryAt, &statusStr,
			&item.ReceivedAt, &item.LastProcessedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrDLQItemNotFound
			}
			return fmt.Errorf("query webhook_dlq: %w", err)
		}
		item.Status = webhook.DLQStatus(statusStr)
		if eventType != nil {
			item.EventType = *eventType
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return &item, nil
}

// ListRetryableDLQ finds retryable failed webhook events across tenants via platform scope hatch.
func (s *PgStore) ListRetryableDLQ(ctx context.Context, limit int) ([]*webhook.DLQItem, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set platform scope: %w", err)
	}

	const query = `
		SELECT dlq_id, tenant_id, provider_name, event_type, raw_payload,
		       error_reason, is_retryable, retry_count, next_retry_at, status,
		       received_at, last_processed_at
		FROM webhook_dlq
		WHERE is_retryable = true
		  AND status = 'FAILED'
		  AND (next_retry_at IS NULL OR next_retry_at <= now())
		ORDER BY received_at ASC
		LIMIT $1;
	`
	rows, err := tx.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("query retryable dlq: %w", err)
	}
	defer rows.Close()

	var items []*webhook.DLQItem
	for rows.Next() {
		var item webhook.DLQItem
		var statusStr string
		var eventType *string
		if err := rows.Scan(
			&item.DLQID, &item.TenantID, &item.ProviderName, &eventType, &item.RawPayload,
			&item.ErrorReason, &item.IsRetryable, &item.RetryCount, &item.NextRetryAt, &statusStr,
			&item.ReceivedAt, &item.LastProcessedAt,
		); err != nil {
			return nil, err
		}
		item.Status = webhook.DLQStatus(statusStr)
		if eventType != nil {
			item.EventType = *eventType
		}
		items = append(items, &item)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit dlq query tx: %w", err)
	}

	return items, nil
}

// UpdateDLQStatus updates the lifecycle state of a DLQ entry under tenant RLS.
func (s *PgStore) UpdateDLQStatus(
	ctx context.Context,
	tenantID, dlqID string,
	status webhook.DLQStatus,
	retryCount int,
	nextRetryAt *time.Time,
	errorReason string,
) error {
	if strings.TrimSpace(tenantID) == "" {
		tenantID = "SYSTEM_UNRESOLVED"
	}
	now := time.Now().UTC()

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const sql = `
			UPDATE webhook_dlq
			SET status = $1,
			    retry_count = $2,
			    next_retry_at = $3,
			    error_reason = $4,
			    last_processed_at = $5
			WHERE tenant_id = $6 AND dlq_id = $7;
		`
		_, err := tx.Exec(ctx, sql, string(status), retryCount, nextRetryAt, errorReason, now, tenantID, dlqID)
		if err != nil {
			return fmt.Errorf("update webhook_dlq status: %w", err)
		}
		return nil
	})
}
