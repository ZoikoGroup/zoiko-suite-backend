package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
)

var (
	ErrIntentNotFound = errors.New("message intent not found")
	ErrRenderNotFound = errors.New("message render not found")
)

// CreateMessageIntent inserts an intent. If an intent with the same (tenant_id, deduplication_key)
// already exists, it returns created=false and fetches the existing record (idempotent replay).
func (s *PgStore) CreateMessageIntent(ctx context.Context, intent *ledger.MessageIntent) (bool, *ledger.MessageIntent, error) {
	if intent.TenantID == "" {
		return false, nil, fmt.Errorf("missing tenant_id")
	}
	if intent.MessageIntentID == "" {
		return false, nil, fmt.Errorf("missing message_intent_id")
	}
	if intent.DeduplicationKey == "" {
		return false, nil, fmt.Errorf("missing deduplication_key")
	}
	if intent.Channel == "" {
		intent.Channel = "EMAIL"
	}
	if intent.Status == "" {
		intent.Status = ledger.IntentStatusPending
	}
	now := time.Now().UTC()
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = now
	}
	if intent.UpdatedAt.IsZero() {
		intent.UpdatedAt = now
	}

	var created bool
	var res *ledger.MessageIntent

	err := s.withRLS(ctx, intent.TenantID, func(tx pgx.Tx) error {
		const insertSQL = `
			INSERT INTO message_intents (
				message_intent_id, tenant_id, legal_entity_id, recipient_principal_id,
				recipient_email, channel, communication_class, template_key,
				event_id, source_event_type, deduplication_key, correlation_id,
				causation_id, status, failure_reason, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
			ON CONFLICT (tenant_id, deduplication_key) DO NOTHING;
		`
		tag, insertErr := tx.Exec(ctx, insertSQL,
			intent.MessageIntentID, intent.TenantID, intent.LegalEntityID, intent.RecipientPrincipalID,
			intent.RecipientEmail, intent.Channel, string(intent.CommunicationClass), intent.TemplateKey,
			intent.EventID, intent.SourceEventType, intent.DeduplicationKey, intent.CorrelationID,
			intent.CausationID, string(intent.Status), intent.FailureReason, intent.CreatedAt, intent.UpdatedAt,
		)
		if insertErr != nil {
			return fmt.Errorf("insert message_intent: %w", insertErr)
		}

		if tag.RowsAffected() > 0 {
			created = true
			res = intent
			return nil
		}

		// Replay detected: fetch existing intent
		created = false
		const selectSQL = `
			SELECT message_intent_id, tenant_id, legal_entity_id, recipient_principal_id,
			       recipient_email, channel, communication_class, template_key,
			       COALESCE(event_id, ''), source_event_type, deduplication_key, correlation_id,
			       causation_id, status, failure_reason, created_at, updated_at
			FROM message_intents
			WHERE tenant_id = $1 AND deduplication_key = $2;
		`
		row := tx.QueryRow(ctx, selectSQL, intent.TenantID, intent.DeduplicationKey)
		existing := &ledger.MessageIntent{}
		var classStr, statusStr string
		if scanErr := row.Scan(
			&existing.MessageIntentID, &existing.TenantID, &existing.LegalEntityID, &existing.RecipientPrincipalID,
			&existing.RecipientEmail, &existing.Channel, &classStr, &existing.TemplateKey,
			&existing.EventID, &existing.SourceEventType, &existing.DeduplicationKey, &existing.CorrelationID,
			&existing.CausationID, &statusStr, &existing.FailureReason, &existing.CreatedAt, &existing.UpdatedAt,
		); scanErr != nil {
			return fmt.Errorf("fetch existing message_intent: %w", scanErr)
		}
		existing.CommunicationClass = ledger.CommunicationClass(classStr)
		existing.Status = ledger.IntentStatus(statusStr)
		res = existing
		return nil
	})

	if err != nil {
		return false, nil, err
	}
	return created, res, nil
}

// RecordMessageRender stores the exact rendered output and content hash.
func (s *PgStore) RecordMessageRender(ctx context.Context, render *ledger.MessageRender) error {
	if render.RenderID == "" || render.MessageIntentID == "" || render.TenantID == "" {
		return fmt.Errorf("render_id, message_intent_id and tenant_id are required")
	}
	if render.Locale == "" {
		render.Locale = "en-US"
	}
	if render.RenderedAt.IsZero() {
		render.RenderedAt = time.Now().UTC()
	}

	return s.withRLS(ctx, render.TenantID, func(tx pgx.Tx) error {
		const sql = `
			INSERT INTO message_renders (
				render_id, message_intent_id, tenant_id, template_key,
				template_version, locale, content_hash, subject,
				body_html, body_text, rendered_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);
		`
		_, err := tx.Exec(ctx, sql,
			render.RenderID, render.MessageIntentID, render.TenantID, render.TemplateKey,
			render.TemplateVersion, render.Locale, render.ContentHash, render.Subject,
			render.BodyHTML, render.BodyText, render.RenderedAt,
		)
		if err != nil {
			return fmt.Errorf("insert message_render: %w", err)
		}
		return nil
	})
}

// RecordDeliveryAttempt stores a provider delivery attempt.
func (s *PgStore) RecordDeliveryAttempt(ctx context.Context, attempt *ledger.DeliveryAttempt) error {
	if attempt.ProviderAttemptID == "" || attempt.MessageIntentID == "" || attempt.RenderID == "" || attempt.TenantID == "" {
		return fmt.Errorf("attempt_id, intent_id, render_id and tenant_id are required")
	}
	if attempt.AttemptedAt.IsZero() {
		attempt.AttemptedAt = time.Now().UTC()
	}
	if attempt.AttemptNumber <= 0 {
		attempt.AttemptNumber = 1
	}

	return s.withRLS(ctx, attempt.TenantID, func(tx pgx.Tx) error {
		const sql = `
			INSERT INTO delivery_attempts (
				provider_attempt_id, message_intent_id, render_id, tenant_id,
				sender_stream, from_address, to_address, provider_name,
				provider_message_id, status, failure_reason, attempt_number,
				attempted_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);
		`
		_, err := tx.Exec(ctx, sql,
			attempt.ProviderAttemptID, attempt.MessageIntentID, attempt.RenderID, attempt.TenantID,
			string(attempt.SenderStream), attempt.FromAddress, attempt.ToAddress, attempt.ProviderName,
			attempt.ProviderMessageID, string(attempt.Status), attempt.FailureReason, attempt.AttemptNumber,
			attempt.AttemptedAt,
		)
		if err != nil {
			return fmt.Errorf("insert delivery_attempt: %w", err)
		}
		return nil
	})
}

// RecordDeliveryEvent stores external delivery/webhook outcome evidence.
func (s *PgStore) RecordDeliveryEvent(ctx context.Context, event *ledger.DeliveryEvent) error {
	if event.DeliveryEventID == "" || event.ProviderAttemptID == "" || event.MessageIntentID == "" || event.TenantID == "" {
		return fmt.Errorf("event_id, attempt_id, intent_id and tenant_id are required")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}

	return s.withRLS(ctx, event.TenantID, func(tx pgx.Tx) error {
		const sql = `
			INSERT INTO delivery_events (
				delivery_event_id, provider_attempt_id, message_intent_id,
				tenant_id, event_type, raw_payload, occurred_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7);
		`
		_, err := tx.Exec(ctx, sql,
			event.DeliveryEventID, event.ProviderAttemptID, event.MessageIntentID,
			event.TenantID, string(event.EventType), event.RawPayload, event.OccurredAt,
		)
		if err != nil {
			return fmt.Errorf("insert delivery_event: %w", err)
		}
		return nil
	})
}

// UpdateIntentStatus modifies the status and failure reason of a message intent.
func (s *PgStore) UpdateIntentStatus(ctx context.Context, tenantID, intentID string, status ledger.IntentStatus, failureReason *string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const sql = `
			UPDATE message_intents
			SET status = $1, failure_reason = $2, updated_at = now()
			WHERE tenant_id = $3 AND message_intent_id = $4;
		`
		tag, err := tx.Exec(ctx, sql, string(status), failureReason, tenantID, intentID)
		if err != nil {
			return fmt.Errorf("update message_intent: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrIntentNotFound
		}
		return nil
	})
}

// GetMessageIntent retrieves an intent under strict tenant RLS.
func (s *PgStore) GetMessageIntent(ctx context.Context, tenantID, intentID string) (*ledger.MessageIntent, error) {
	var intent *ledger.MessageIntent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const sql = `
			SELECT message_intent_id, tenant_id, legal_entity_id, recipient_principal_id,
			       recipient_email, channel, communication_class, template_key,
			       COALESCE(event_id, ''), source_event_type, deduplication_key, correlation_id,
			       causation_id, status, failure_reason, created_at, updated_at
			FROM message_intents
			WHERE tenant_id = $1 AND message_intent_id = $2;
		`
		row := tx.QueryRow(ctx, sql, tenantID, intentID)
		existing := &ledger.MessageIntent{}
		var classStr, statusStr string
		if scanErr := row.Scan(
			&existing.MessageIntentID, &existing.TenantID, &existing.LegalEntityID, &existing.RecipientPrincipalID,
			&existing.RecipientEmail, &existing.Channel, &classStr, &existing.TemplateKey,
			&existing.EventID, &existing.SourceEventType, &existing.DeduplicationKey, &existing.CorrelationID,
			&existing.CausationID, &statusStr, &existing.FailureReason, &existing.CreatedAt, &existing.UpdatedAt,
		); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return ErrIntentNotFound
			}
			return mapPgError(scanErr)
		}
		existing.CommunicationClass = ledger.CommunicationClass(classStr)
		existing.Status = ledger.IntentStatus(statusStr)
		intent = existing
		return nil
	})

	if err != nil {
		if errors.Is(err, domain.ErrNotificationNotFound) {
			return nil, ErrIntentNotFound
		}
		return nil, err
	}
	return intent, nil
}

// GetRenderByIntent fetches the rendered output associated with an intent.
func (s *PgStore) GetRenderByIntent(ctx context.Context, tenantID, intentID string) (*ledger.MessageRender, error) {
	var render *ledger.MessageRender
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const sql = `
			SELECT render_id, message_intent_id, tenant_id, template_key,
			       template_version, locale, content_hash, subject,
			       body_html, body_text, rendered_at
			FROM message_renders
			WHERE tenant_id = $1 AND message_intent_id = $2;
		`
		row := tx.QueryRow(ctx, sql, tenantID, intentID)
		r := &ledger.MessageRender{}
		if scanErr := row.Scan(
			&r.RenderID, &r.MessageIntentID, &r.TenantID, &r.TemplateKey,
			&r.TemplateVersion, &r.Locale, &r.ContentHash, &r.Subject,
			&r.BodyHTML, &r.BodyText, &r.RenderedAt,
		); scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return ErrRenderNotFound
			}
			return mapPgError(scanErr)
		}
		render = r
		return nil
	})

	if err != nil {
		return nil, err
	}
	return render, nil
}
