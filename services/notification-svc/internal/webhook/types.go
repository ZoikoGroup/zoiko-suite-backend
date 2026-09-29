package webhook

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrAttemptNotFound = errors.New("attempt not found")
)

// NormalizedEventType defines the standardized delivery feedback event types.
type NormalizedEventType string

const (
	EventTypeDelivered   NormalizedEventType = "DELIVERED"
	EventTypeBounce      NormalizedEventType = "BOUNCE"
	EventTypeComplaint   NormalizedEventType = "COMPLAINT"
	EventTypeUnsubscribe NormalizedEventType = "UNSUBSCRIBE"
	EventTypeDropped     NormalizedEventType = "DROPPED"
)

// BounceClassification classifies bounce severity into permanent (hard) vs temporary (soft).
type BounceClassification string

const (
	BounceHard      BounceClassification = "HARD"
	BounceSoft      BounceClassification = "SOFT"
	BounceTransient BounceClassification = "TRANSIENT"
)

// WebhookEvent represents a provider-agnostic, normalized feedback event.
type WebhookEvent struct {
	EventID           string               `json:"event_id"`
	Provider          string               `json:"provider"`
	EventType         NormalizedEventType  `json:"event_type"`
	BounceType        BounceClassification `json:"bounce_type,omitempty"`
	RecipientEmail    string               `json:"recipient_email"`
	ProviderMessageID string               `json:"provider_message_id,omitempty"`
	TenantID          string               `json:"tenant_id,omitempty"`
	MessageIntentID   string               `json:"message_intent_id,omitempty"`
	ProviderAttemptID string               `json:"provider_attempt_id,omitempty"`
	SourceStream      string               `json:"source_stream,omitempty"`
	DiagnosticCode    string               `json:"diagnostic_code,omitempty"`
	OccurredAt        time.Time            `json:"occurred_at"`
	RawPayload        json.RawMessage      `json:"raw_payload"`
}

// DLQStatus represents the lifecycle of a record in webhook_dlq.
type DLQStatus string

const (
	DLQStatusFailed      DLQStatus = "FAILED"
	DLQStatusReprocessed DLQStatus = "REPROCESSED"
	DLQStatusAbandoned   DLQStatus = "ABANDONED"
)

// DLQItem models an unprocessable or failing webhook record in webhook_dlq.
type DLQItem struct {
	DLQID           string          `json:"dlq_id"`
	TenantID        string          `json:"tenant_id"`
	ProviderName    string          `json:"provider_name"`
	EventType       string          `json:"event_type"`
	RawPayload      json.RawMessage `json:"raw_payload"`
	ErrorReason     string          `json:"error_reason"`
	IsRetryable     bool            `json:"is_retryable"`
	RetryCount      int             `json:"retry_count"`
	NextRetryAt     *time.Time      `json:"next_retry_at,omitempty"`
	Status          DLQStatus       `json:"status"`
	ReceivedAt      time.Time       `json:"received_at"`
	LastProcessedAt *time.Time      `json:"last_processed_at,omitempty"`
}

// AttemptLookupResult contains the resolved metadata for a matched delivery attempt.
type AttemptLookupResult struct {
	ProviderAttemptID string `json:"provider_attempt_id"`
	MessageIntentID   string `json:"message_intent_id"`
	TenantID          string `json:"tenant_id"`
	SenderStream      string `json:"sender_stream"`
	RecipientAddress  string `json:"recipient_address"`
}
