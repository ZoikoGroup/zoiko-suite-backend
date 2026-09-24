// Package ledger implements the canonical Delivery Ledger, Template Engine,
// and Orchestration primitives for the ZoikoSuite Email Communications System
// per ZS-COMMS-EMAIL-001 v2.0 §3.
package ledger

import (
	"encoding/json"
	"time"
)

// CommunicationClass defines the governed priority and preference treatment per §4.
type CommunicationClass string

const (
	ClassS0 CommunicationClass = "S0" // Security / legal critical
	ClassT0 CommunicationClass = "T0" // Core transactional
	ClassA1 CommunicationClass = "A1" // Administrative operational
	ClassL1 CommunicationClass = "L1" // Lifecycle / education
	ClassM1 CommunicationClass = "M1" // Commercial / marketing
)

// SenderStream defines the isolated domain/subdomain stream per §7.
type SenderStream string

const (
	StreamCritical      SenderStream = "CRITICAL"      // security@security.zoikosuite.com
	StreamTransactional SenderStream = "TRANSACTIONAL" // notifications@notify.zoikosuite.com / billing@
	StreamOperational   SenderStream = "OPERATIONAL"   // updates@updates.zoikosuite.com
	StreamMarketing     SenderStream = "MARKETING"     // hello@news.zoikosuite.com
)

// IntentStatus defines the lifecycle status of a governed message intent.
type IntentStatus string

const (
	IntentStatusPending    IntentStatus = "PENDING"
	IntentStatusRendered   IntentStatus = "RENDERED"
	IntentStatusDispatched IntentStatus = "DISPATCHED"
	IntentStatusDelivered  IntentStatus = "DELIVERED"
	IntentStatusFailed     IntentStatus = "FAILED"
	IntentStatusKilled     IntentStatus = "KILLED"
)

// AttemptStatus defines the outcome of an individual provider attempt.
type AttemptStatus string

const (
	AttemptStatusQueued   AttemptStatus = "QUEUED"
	AttemptStatusAccepted AttemptStatus = "ACCEPTED"
	AttemptStatusFailed   AttemptStatus = "FAILED"
	AttemptStatusRetrying AttemptStatus = "RETRYING"
)

// DeliveryEventType defines the provider/mailbox telemetry receipt event.
type DeliveryEventType string

const (
	DeliveryEventAccepted   DeliveryEventType = "ACCEPTED"
	DeliveryEventDelivered  DeliveryEventType = "DELIVERED"
	DeliveryEventBounced    DeliveryEventType = "BOUNCED"
	DeliveryEventComplained DeliveryEventType = "COMPLAINED"
	DeliveryEventDropped    DeliveryEventType = "DROPPED"
)

// MessageIntent records one governed communication decision for an event and recipient.
// Per ZS-COMMS-EMAIL-001 §3, this is the root record linking domain cause to delivery evidence.
type MessageIntent struct {
	MessageIntentID      string             `json:"message_intent_id"`
	TenantID             string             `json:"tenant_id"`
	LegalEntityID        string             `json:"legal_entity_id"`
	RecipientPrincipalID string             `json:"recipient_principal_id"`
	RecipientEmail       string             `json:"recipient_email"`
	Channel              string             `json:"channel"` // EMAIL, IN_APP
	CommunicationClass   CommunicationClass `json:"communication_class"`
	TemplateKey          string             `json:"template_key"`
	EventID              string             `json:"event_id,omitempty"`
	SourceEventType      string             `json:"source_event_type"`
	DeduplicationKey     string             `json:"deduplication_key"`
	CorrelationID        string             `json:"correlation_id"`
	CausationID          *string            `json:"causation_id,omitempty"`
	Status               IntentStatus       `json:"status"`
	FailureReason        *string            `json:"failure_reason,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
	UpdatedAt            time.Time          `json:"updated_at"`
}

// MessageRender records the exact resolved template, locale, variables, and content hash.
type MessageRender struct {
	RenderID        string    `json:"render_id"`
	MessageIntentID string    `json:"message_intent_id"`
	TenantID        string    `json:"tenant_id"`
	TemplateKey     string    `json:"template_key"`
	TemplateVersion string    `json:"template_version"`
	Locale          string    `json:"locale"`
	ContentHash     string    `json:"content_hash"`
	Subject         string    `json:"subject"`
	BodyHTML        string    `json:"body_html"`
	BodyText        string    `json:"body_text"`
	RenderedAt      time.Time `json:"rendered_at"`
}

// DeliveryAttempt records an individual provider submission attempt.
type DeliveryAttempt struct {
	ProviderAttemptID string        `json:"provider_attempt_id"`
	MessageIntentID   string        `json:"message_intent_id"`
	RenderID          string        `json:"render_id"`
	TenantID          string        `json:"tenant_id"`
	SenderStream      SenderStream  `json:"sender_stream"`
	FromAddress       string        `json:"from_address"`
	ToAddress         string        `json:"to_address"`
	ProviderName      string        `json:"provider_name"`
	ProviderMessageID *string       `json:"provider_message_id,omitempty"`
	Status            AttemptStatus `json:"status"`
	FailureReason     *string       `json:"failure_reason,omitempty"`
	AttemptNumber     int           `json:"attempt_number"`
	AttemptedAt       time.Time     `json:"attempted_at"`
}

// DeliveryEvent records external delivery evidence received from ESP webhooks or callbacks.
type DeliveryEvent struct {
	DeliveryEventID   string            `json:"delivery_event_id"`
	ProviderAttemptID string            `json:"provider_attempt_id"`
	MessageIntentID   string            `json:"message_intent_id"`
	TenantID          string            `json:"tenant_id"`
	EventType         DeliveryEventType `json:"event_type"`
	RawPayload        json.RawMessage   `json:"raw_payload,omitempty"`
	OccurredAt        time.Time         `json:"occurred_at"`
}

// EventIngestRequest is the canonical payload sent to /v1/notifications/events/ingest.
type EventIngestRequest struct {
	EventID              string            `json:"event_id"`
	EventType            string            `json:"event_type"`
	RecipientPrincipalID string            `json:"recipient_principal_id"`
	RecipientEmail       string            `json:"recipient_email,omitempty"`
	LegalEntityID        string            `json:"legal_entity_id"`
	TemplateKey          string            `json:"template_key"`
	Variables            map[string]string `json:"variables"`
	CorrelationID        string            `json:"correlation_id"`
	CausationID          *string           `json:"causation_id,omitempty"`
}

// SuppressionReason defines the recognized reason for address suppression per ZS-COMMS-EMAIL-001 §4, §7.
type SuppressionReason string

const (
	SuppressionReasonHardBounce   SuppressionReason = "HARD_BOUNCE"
	SuppressionReasonComplaint    SuppressionReason = "COMPLAINT"
	SuppressionReasonUnsubscribe  SuppressionReason = "UNSUBSCRIBE"
	SuppressionReasonAdmin        SuppressionReason = "ADMIN_SUPPRESSED"
)

// EmailSuppression records an active deliverability or preference suppression entry in email_suppressions.
type EmailSuppression struct {
	SuppressionID  string            `json:"suppression_id"`
	TenantID       string            `json:"tenant_id"`
	RecipientEmail string            `json:"recipient_email"`
	Reason         SuppressionReason `json:"reason"`
	SourceStream   string            `json:"source_stream"`
	ProviderName   *string           `json:"provider_name,omitempty"`
	RawMetadata    json.RawMessage   `json:"raw_metadata,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

// PolicyDecision defines the outcome of evaluating the 8-level precedence policy before rendering.
type PolicyDecision struct {
	Allowed         bool   `json:"allowed"`
	PrecedenceLevel int    `json:"precedence_level"`
	RuleName        string `json:"rule_name"`
	Reason          string `json:"reason,omitempty"`
}
