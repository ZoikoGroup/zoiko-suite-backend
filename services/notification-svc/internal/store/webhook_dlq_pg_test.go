package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/webhook"
)

func TestWebhookStore_AttemptLookup_PlatformScope(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-lookup-test"
	intentID := uuid.NewString()
	renderID := uuid.NewString()
	attemptID := uuid.NewString()
	providerMsgID := "<provider-msg-uuid-12345@zoikosuite.com>"

	// 1. Seed Intent, Render, and Attempt in tenantID
	intent := &ledger.MessageIntent{
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "principal-001",
		RecipientEmail:       "recipient@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassM1,
		TemplateKey:          "ZS-MKT-001",
		SourceEventType:      "test.event",
		DeduplicationKey:     "dedup-lookup-1",
		CorrelationID:        "corr-lookup-1",
		Status:               ledger.IntentStatusDispatched,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	_, _, err := s.CreateMessageIntent(ctx, intent)
	require.NoError(t, err)

	render := &ledger.MessageRender{
		RenderID:        renderID,
		MessageIntentID: intentID,
		TenantID:        tenantID,
		ContentHash:     "hash1",
		Subject:         "Promo",
		BodyHTML:        "<p>Promo</p>",
		BodyText:        "Promo",
		RenderedAt:      time.Now().UTC(),
	}
	err = s.RecordMessageRender(ctx, render)
	require.NoError(t, err)

	attempt := &ledger.DeliveryAttempt{
		ProviderAttemptID: attemptID,
		MessageIntentID:   intentID,
		RenderID:          renderID,
		TenantID:          tenantID,
		SenderStream:      ledger.StreamMarketing,
		FromAddress:       "hello@news.zoikosuite.com",
		ToAddress:         "customer@example.com",
		ProviderName:      "ses",
		ProviderMessageID: &providerMsgID,
		Status:            ledger.AttemptStatusAccepted,
		AttemptNumber:     1,
		AttemptedAt:       time.Now().UTC(),
	}
	err = s.RecordDeliveryAttempt(ctx, attempt)
	require.NoError(t, err)

	// 2. Lookup by ProviderMessageID without passing tenant in context (platform scope hatch)
	lookup, err := s.LookupAttemptByProviderMessageID(ctx, providerMsgID)
	require.NoError(t, err)
	require.NotNil(t, lookup)

	assert.Equal(t, attemptID, lookup.ProviderAttemptID)
	assert.Equal(t, intentID, lookup.MessageIntentID)
	assert.Equal(t, tenantID, lookup.TenantID)
	assert.Equal(t, "MARKETING", lookup.SenderStream)
	assert.Equal(t, "customer@example.com", lookup.RecipientAddress)
}

func TestWebhookStore_RecordDeliveryEventIdempotent_RLS(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-event-test"
	intentID := uuid.NewString()
	renderID := uuid.NewString()
	attemptID := uuid.NewString()
	eventID := uuid.NewString()

	// Seed Intent, Render, Attempt
	_, _, err := s.CreateMessageIntent(ctx, &ledger.MessageIntent{
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "principal-001",
		RecipientEmail:       "transactional@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassT0,
		TemplateKey:          "ZS-IA-001",
		SourceEventType:      "test.event",
		DeduplicationKey:     "dedup-event-1",
		CorrelationID:        "corr-event-1",
		Status:               ledger.IntentStatusDispatched,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	})
	require.NoError(t, err)

	err = s.RecordMessageRender(ctx, &ledger.MessageRender{
		RenderID:        renderID,
		MessageIntentID: intentID,
		TenantID:        tenantID,
		ContentHash:     "hash2",
		Subject:         "Confirm",
		BodyHTML:        "<p>Confirm</p>",
		BodyText:        "Confirm",
		RenderedAt:      time.Now().UTC(),
	})
	require.NoError(t, err)

	err = s.RecordDeliveryAttempt(ctx, &ledger.DeliveryAttempt{
		ProviderAttemptID: attemptID,
		MessageIntentID:   intentID,
		RenderID:          renderID,
		TenantID:          tenantID,
		SenderStream:      ledger.StreamTransactional,
		FromAddress:       "notifications@notify.zoikosuite.com",
		ToAddress:         "user@example.com",
		ProviderName:      "smtp",
		Status:            ledger.AttemptStatusAccepted,
		AttemptNumber:     1,
		AttemptedAt:       time.Now().UTC(),
	})
	require.NoError(t, err)

	// First insertion of delivery event
	event := &ledger.DeliveryEvent{
		DeliveryEventID:   eventID,
		ProviderAttemptID: attemptID,
		MessageIntentID:   intentID,
		TenantID:          tenantID,
		EventType:         ledger.DeliveryEventDelivered,
		RawPayload:        json.RawMessage(`{"status":"delivered"}`),
		OccurredAt:        time.Now().UTC(),
	}

	inserted, err := s.RecordDeliveryEventIdempotent(ctx, event)
	require.NoError(t, err)
	assert.True(t, inserted, "First insertion must succeed")

	// Duplicate insertion -> idempotent, returns false
	inserted2, err := s.RecordDeliveryEventIdempotent(ctx, event)
	require.NoError(t, err)
	assert.False(t, inserted2, "Second duplicate insertion must return false")
}

func TestWebhookStore_DLQ_Routing_And_RLS(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantA := "tenant-dlq-a"
	tenantB := "tenant-dlq-b"
	dlqID := uuid.NewString()

	retryAt := time.Now().UTC().Add(1 * time.Minute)
	item := &webhook.DLQItem{
		DLQID:        dlqID,
		TenantID:     tenantA,
		ProviderName: "ses",
		EventType:    "BOUNCE",
		RawPayload:   json.RawMessage(`{"raw":"bounce-data"}`),
		ErrorReason:  "network timeout during processing",
		IsRetryable:  true,
		RetryCount:   0,
		NextRetryAt:  &retryAt,
		Status:       webhook.DLQStatusFailed,
		ReceivedAt:   time.Now().UTC(),
	}

	// 1. Route to DLQ
	err := s.RouteToDLQ(ctx, item)
	require.NoError(t, err)

	// 2. Tenant A can read it
	fetchedA, err := s.GetDLQItem(ctx, tenantA, dlqID)
	require.NoError(t, err)
	assert.Equal(t, dlqID, fetchedA.DLQID)
	assert.Equal(t, webhook.DLQStatusFailed, fetchedA.Status)

	// 3. Tenant B CANNOT read it (RLS tenant isolation)
	_, err = s.GetDLQItem(ctx, tenantB, dlqID)
	require.Error(t, err, "Tenant B must not see Tenant A's DLQ item")
	assert.ErrorIs(t, err, store.ErrDLQItemNotFound)

	// 4. Update status under tenant A
	err = s.UpdateDLQStatus(ctx, tenantA, dlqID, webhook.DLQStatusReprocessed, 1, nil, "")
	require.NoError(t, err)

	fetchedUpdated, err := s.GetDLQItem(ctx, tenantA, dlqID)
	require.NoError(t, err)
	assert.Equal(t, webhook.DLQStatusReprocessed, fetchedUpdated.Status)
	assert.Equal(t, 1, fetchedUpdated.RetryCount)
}
