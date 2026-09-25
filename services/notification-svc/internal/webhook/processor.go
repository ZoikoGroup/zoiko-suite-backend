package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/ledger"
)

var (
	ErrInvalidEvent = errors.New("invalid webhook event")
)

// WebhookStore abstracts the storage operations required for webhook processing and DLQ.
type WebhookStore interface {
	LookupAttemptByProviderMessageID(ctx context.Context, providerMessageID string) (*AttemptLookupResult, error)
	RecordDeliveryEventIdempotent(ctx context.Context, event *ledger.DeliveryEvent) (bool, error)
	AddSuppression(ctx context.Context, supp *ledger.EmailSuppression) error
	RouteToDLQ(ctx context.Context, item *DLQItem) error
	GetDLQItem(ctx context.Context, tenantID, dlqID string) (*DLQItem, error)
	UpdateDLQStatus(ctx context.Context, tenantID, dlqID string, status DLQStatus, retryCount int, nextRetryAt *time.Time, errorReason string) error
	ListRetryableDLQ(ctx context.Context, limit int) ([]*DLQItem, error)
}

// MetricsRecorder records webhook events and DLQ routing metrics.
type MetricsRecorder interface {
	RecordWebhookEvent(provider, eventType string)
	RecordDLQ(provider, status string)
}

// Processor manages the normalization, validation, ledger recording, suppression mapping, and DLQ routing.
type Processor struct {
	store      WebhookStore
	normalizer *Normalizer
	metrics    MetricsRecorder
	log        *zap.Logger
}

func NewProcessor(store WebhookStore, log *zap.Logger) *Processor {
	if log == nil {
		log = zap.NewNop()
	}
	return &Processor{
		store:      store,
		normalizer: NewNormalizer(),
		log:        log,
	}
}

// SetMetrics configures optional metrics instrumentation on the Processor.
func (p *Processor) SetMetrics(m MetricsRecorder) {
	p.metrics = m
}

// ProcessRawPayload parses, normalizes, and processes raw webhook payloads for a given provider.
func (p *Processor) ProcessRawPayload(ctx context.Context, provider string, payload []byte) error {
	events, err := p.normalizer.Normalize(provider, payload)
	if err != nil {
		p.log.Warn("malformed webhook payload; routing to DLQ",
			zap.String("provider", provider),
			zap.Error(err),
		)
		dlqItem := &DLQItem{
			DLQID:        uuid.NewString(),
			TenantID:     "SYSTEM_UNRESOLVED",
			ProviderName: provider,
			EventType:    "MALFORMED_PAYLOAD",
			RawPayload:   payload,
			ErrorReason:  err.Error(),
			IsRetryable:  false,
			Status:       DLQStatusFailed,
			ReceivedAt:   time.Now().UTC(),
		}
		_ = p.store.RouteToDLQ(ctx, dlqItem)
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}

	var firstErr error
	for _, ev := range events {
		if err := p.ProcessEvent(ctx, ev); err != nil {
			p.log.Error("failed to process webhook event",
				zap.String("event_id", ev.EventID),
				zap.String("provider", ev.Provider),
				zap.Error(err),
			)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ProcessEvent processes one normalized webhook event under tenant isolation rules.
func (p *Processor) ProcessEvent(ctx context.Context, ev *WebhookEvent) error {
	if strings.TrimSpace(ev.RecipientEmail) == "" || ev.EventType == "" {
		p.log.Warn("invalid webhook event structure; routing to DLQ",
			zap.String("event_id", ev.EventID),
			zap.String("recipient", ev.RecipientEmail),
		)
		dlqItem := &DLQItem{
			DLQID:        uuid.NewString(),
			TenantID:     ev.TenantID,
			ProviderName: ev.Provider,
			EventType:    string(ev.EventType),
			RawPayload:   ev.RawPayload,
			ErrorReason:  "missing recipient email or event type",
			IsRetryable:  false,
			Status:       DLQStatusFailed,
			ReceivedAt:   time.Now().UTC(),
		}
		_ = p.store.RouteToDLQ(ctx, dlqItem)
		return ErrInvalidEvent
	}

	// 1. Resolve tenant and attempt metadata if not provided
	if ev.TenantID == "" || ev.ProviderAttemptID == "" {
		lookupID := ev.ProviderMessageID
		if lookupID == "" {
			lookupID = ev.ProviderAttemptID
		}
		if lookupID != "" {
			lookup, err := p.store.LookupAttemptByProviderMessageID(ctx, lookupID)
			if err != nil {
				if errors.Is(err, ErrAttemptNotFound) || err.Error() == "attempt not found" {
					// Expected when provider message ID does not match any known attempt
				} else {
					p.log.Error("failed to lookup attempt by provider message id; routing to retryable DLQ",
						zap.String("lookup_id", lookupID),
						zap.Error(err),
					)
					p.routeRetryableDLQ(ctx, ev, err)
					return err
				}
			} else if lookup != nil {
				ev.TenantID = lookup.TenantID
				ev.ProviderAttemptID = lookup.ProviderAttemptID
				ev.MessageIntentID = lookup.MessageIntentID
				if ev.SourceStream == "" {
					ev.SourceStream = lookup.SenderStream
				}
			}
		}
	}

	// If tenant still unresolved, route safely to DLQ without corrupting multi-tenant state
	if ev.TenantID == "" {
		p.log.Warn("webhook event matches no tenant; routing to DLQ",
			zap.String("provider", ev.Provider),
			zap.String("provider_message_id", ev.ProviderMessageID),
			zap.String("recipient", ev.RecipientEmail),
		)
		dlqItem := &DLQItem{
			DLQID:        uuid.NewString(),
			TenantID:     "SYSTEM_UNRESOLVED",
			ProviderName: ev.Provider,
			EventType:    string(ev.EventType),
			RawPayload:   ev.RawPayload,
			ErrorReason:  "unresolved tenant and attempt for provider message id",
			IsRetryable:  false,
			Status:       DLQStatusFailed,
			ReceivedAt:   time.Now().UTC(),
		}
		_ = p.store.RouteToDLQ(ctx, dlqItem)
		return nil
	}

	// 2. Record Delivery Event in Ledger if attempt was identified
	if ev.ProviderAttemptID != "" && ev.MessageIntentID != "" {
		ledgerType := mapWebhookToLedgerType(ev.EventType)
		delivEvent := &ledger.DeliveryEvent{
			DeliveryEventID:   ev.EventID,
			ProviderAttemptID: ev.ProviderAttemptID,
			MessageIntentID:   ev.MessageIntentID,
			TenantID:          ev.TenantID,
			EventType:         ledgerType,
			RawPayload:        ev.RawPayload,
			OccurredAt:        ev.OccurredAt,
		}
		if _, err := p.store.RecordDeliveryEventIdempotent(ctx, delivEvent); err != nil {
			p.log.Error("failed to record delivery event; routing to DLQ",
				zap.String("event_id", ev.EventID),
				zap.String("tenant_id", ev.TenantID),
				zap.Error(err),
			)
			p.routeRetryableDLQ(ctx, ev, err)
			return err
		}
	}

	// 3. Map Bounces, Complaints, and Unsubscribes to email_suppressions
	if err := p.mapSuppression(ctx, ev); err != nil {
		p.log.Error("failed to apply suppression from webhook; routing to DLQ",
			zap.String("event_id", ev.EventID),
			zap.String("tenant_id", ev.TenantID),
			zap.Error(err),
		)
		p.routeRetryableDLQ(ctx, ev, err)
		return err
	}

	if p.metrics != nil {
		p.metrics.RecordWebhookEvent(ev.Provider, string(ev.EventType))
	}

	return nil
}

func (p *Processor) mapSuppression(ctx context.Context, ev *WebhookEvent) error {
	var reason ledger.SuppressionReason
	sourceStream := ev.SourceStream

	switch ev.EventType {
	case EventTypeBounce:
		// Only HARD bounces create a permanent suppression.
		// Soft bounces (transient mailbox full, etc.) do NOT suppress the recipient.
		if ev.BounceType == BounceSoft || ev.BounceType == BounceTransient {
			p.log.Info("soft bounce received; skipping permanent suppression",
				zap.String("tenant_id", ev.TenantID),
				zap.String("recipient", ev.RecipientEmail),
			)
			return nil
		}
		reason = ledger.SuppressionReasonHardBounce
		sourceStream = "ALL" // Hard bounce affects all streams for the recipient

	case EventTypeComplaint:
		reason = ledger.SuppressionReasonComplaint
		sourceStream = "ALL" // Spam complaint suppresses all streams

	case EventTypeUnsubscribe:
		reason = ledger.SuppressionReasonUnsubscribe
		if sourceStream == "" {
			sourceStream = "MARKETING" // Default unsubscribe to marketing
		}

	default:
		// Delivered, Dropped, or other events do not generate suppressions
		return nil
	}

	supp := &ledger.EmailSuppression{
		SuppressionID:  uuid.NewString(),
		TenantID:       ev.TenantID,
		RecipientEmail: strings.ToLower(strings.TrimSpace(ev.RecipientEmail)),
		Reason:         reason,
		SourceStream:   sourceStream,
		ProviderName:   &ev.Provider,
		RawMetadata:    ev.RawPayload,
		CreatedAt:      time.Now().UTC(),
	}

	return p.store.AddSuppression(ctx, supp)
}

func (p *Processor) routeRetryableDLQ(ctx context.Context, ev *WebhookEvent, procErr error) {
	retryAt := time.Now().UTC().Add(30 * time.Second)
	dlqItem := &DLQItem{
		DLQID:        uuid.NewString(),
		TenantID:     ev.TenantID,
		ProviderName: ev.Provider,
		EventType:    string(ev.EventType),
		RawPayload:   ev.RawPayload,
		ErrorReason:  procErr.Error(),
		IsRetryable:  true,
		RetryCount:   1,
		NextRetryAt:  &retryAt,
		Status:       DLQStatusFailed,
		ReceivedAt:   time.Now().UTC(),
	}
	_ = p.store.RouteToDLQ(ctx, dlqItem)
	if p.metrics != nil {
		p.metrics.RecordDLQ(ev.Provider, "routed")
	}
}

// ReprocessDLQItem attempts to reprocess an event from the DLQ.
func (p *Processor) ReprocessDLQItem(ctx context.Context, tenantID, dlqID string) error {
	item, err := p.store.GetDLQItem(ctx, tenantID, dlqID)
	if err != nil {
		return err
	}

	events, err := p.normalizer.Normalize(item.ProviderName, item.RawPayload)
	if err != nil {
		_ = p.store.UpdateDLQStatus(ctx, item.TenantID, item.DLQID, DLQStatusAbandoned, item.RetryCount+1, nil, err.Error())
		if p.metrics != nil {
			p.metrics.RecordDLQ(item.ProviderName, "abandoned")
		}
		return err
	}

	for _, ev := range events {
		if err := p.ProcessEvent(ctx, ev); err != nil {
			nextRetry := time.Now().UTC().Add(time.Duration(item.RetryCount+1) * time.Minute)
			_ = p.store.UpdateDLQStatus(ctx, item.TenantID, item.DLQID, DLQStatusFailed, item.RetryCount+1, &nextRetry, err.Error())
			return err
		}
	}

	if p.metrics != nil {
		p.metrics.RecordDLQ(item.ProviderName, "reprocessed")
	}

	return p.store.UpdateDLQStatus(ctx, item.TenantID, item.DLQID, DLQStatusReprocessed, item.RetryCount+1, nil, "")
}

// ProcessRetryableDLQ queries and reprocesses eligible retryable DLQ records.
func (p *Processor) ProcessRetryableDLQ(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	items, err := p.store.ListRetryableDLQ(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("list retryable dlq: %w", err)
	}

	var reprocessedCount int
	for _, it := range items {
		if err := p.ReprocessDLQItem(ctx, it.TenantID, it.DLQID); err == nil {
			reprocessedCount++
		} else {
			p.log.Warn("failed to reprocess dlq item",
				zap.String("dlq_id", it.DLQID),
				zap.String("tenant_id", it.TenantID),
				zap.Error(err),
			)
		}
	}
	return reprocessedCount, nil
}

func mapWebhookToLedgerType(wt NormalizedEventType) ledger.DeliveryEventType {
	switch wt {
	case EventTypeDelivered:
		return ledger.DeliveryEventDelivered
	case EventTypeBounce:
		return ledger.DeliveryEventBounced
	case EventTypeComplaint:
		return ledger.DeliveryEventComplained
	case EventTypeDropped:
		return ledger.DeliveryEventDropped
	default:
		return ledger.DeliveryEventAccepted
	}
}
