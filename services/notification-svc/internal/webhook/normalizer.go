package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrEmptyPayload      = errors.New("empty webhook payload")
	ErrUnsupportedFormat = errors.New("unsupported webhook payload format")
	ErrMissingRecipient  = errors.New("missing recipient email in webhook event")
	ErrMissingEventType  = errors.New("missing or unrecognized event type in webhook event")
)

// Normalizer parses and standardizes raw webhook payloads into a slice of WebhookEvents.
type Normalizer struct{}

func NewNormalizer() *Normalizer {
	return &Normalizer{}
}

// Normalize parses raw JSON payload from provider and returns normalized events.
func (n *Normalizer) Normalize(provider string, payload []byte) ([]*WebhookEvent, error) {
	if len(payload) == 0 {
		return nil, ErrEmptyPayload
	}

	p := strings.ToLower(strings.TrimSpace(provider))
	switch p {
	case "ses", "aws":
		return n.normalizeSES(payload)
	case "sendgrid":
		return n.normalizeSendGrid(payload)
	default:
		return n.normalizeStandard(provider, payload)
	}
}

// normalizeStandard handles canonical Zoiko format or generic single-event payloads.
func (n *Normalizer) normalizeStandard(provider string, payload []byte) ([]*WebhookEvent, error) {
	// Try parsing as array first
	var rawArray []map[string]interface{}
	if err := json.Unmarshal(payload, &rawArray); err == nil && len(rawArray) > 0 {
		var events []*WebhookEvent
		for _, item := range rawArray {
			ev, err := n.mapGenericMap(provider, item, payload)
			if err != nil {
				return nil, err
			}
			events = append(events, ev)
		}
		return events, nil
	}

	// Parse as single object
	var rawMap map[string]interface{}
	if err := json.Unmarshal(payload, &rawMap); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}

	ev, err := n.mapGenericMap(provider, rawMap, payload)
	if err != nil {
		return nil, err
	}
	return []*WebhookEvent{ev}, nil
}

func (n *Normalizer) mapGenericMap(provider string, m map[string]interface{}, raw []byte) (*WebhookEvent, error) {
	getString := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}

	rawType := getString("event_type", "event", "type", "eventType")
	if rawType == "" {
		return nil, ErrMissingEventType
	}

	email := getString("recipient_email", "recipient", "email", "to")
	if email == "" {
		return nil, ErrMissingRecipient
	}

	eventType, bounceType := classifyEventType(rawType, getString("bounce_type", "subtype", "reason"))
	providerMsgID := getString("provider_message_id", "message_id", "messageId", "sg_message_id")
	tenantID := getString("tenant_id", "tenantId")
	intentID := getString("message_intent_id", "intent_id", "intentId")
	attemptID := getString("provider_attempt_id", "attempt_id", "attemptId")
	stream := getString("source_stream", "stream", "sender_stream")
	diag := getString("diagnostic_code", "reason", "error", "description")

	eventID := getString("event_id", "id", "eventId")
	if eventID == "" {
		// Deterministic hash based on event properties for idempotency
		h := sha256.New()
		h.Write([]byte(fmt.Sprintf("%s:%s:%s:%s:%s", provider, eventType, email, providerMsgID, diag)))
		eventID = hex.EncodeToString(h.Sum(nil))[:32]
	}

	occurredAt := time.Now().UTC()
	if tsStr := getString("occurred_at", "timestamp", "time"); tsStr != "" {
		if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
			occurredAt = t.UTC()
		}
	}

	return &WebhookEvent{
		EventID:           eventID,
		Provider:          provider,
		EventType:         eventType,
		BounceType:        bounceType,
		RecipientEmail:    strings.ToLower(email),
		ProviderMessageID: providerMsgID,
		TenantID:          tenantID,
		MessageIntentID:   intentID,
		ProviderAttemptID: attemptID,
		SourceStream:      stream,
		DiagnosticCode:    diag,
		OccurredAt:        occurredAt,
		RawPayload:        raw,
	}, nil
}

// normalizeSES handles AWS SES SNS notifications.
func (n *Normalizer) normalizeSES(payload []byte) ([]*WebhookEvent, error) {
	var sesEnvelope struct {
		Type      string `json:"Type"`
		Message   string `json:"Message"`
		EventType string `json:"eventType"`
		Bounce    struct {
			BounceType        string `json:"bounceType"`
			BouncedRecipients []struct {
				EmailAddress   string `json:"emailAddress"`
				DiagnosticCode string `json:"diagnosticCode"`
			} `json:"bouncedRecipients"`
		} `json:"bounce"`
		Complaint struct {
			ComplainedRecipients []struct {
				EmailAddress string `json:"emailAddress"`
			} `json:"complainedRecipients"`
		} `json:"complaint"`
		Mail struct {
			MessageID   string   `json:"messageId"`
			Destination []string `json:"destination"`
			Headers     []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
		} `json:"mail"`
	}

	if err := json.Unmarshal(payload, &sesEnvelope); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}

	// Unwrap SNS envelope if present
	if sesEnvelope.Type == "Notification" && sesEnvelope.Message != "" {
		return n.normalizeSES([]byte(sesEnvelope.Message))
	}

	var events []*WebhookEvent
	msgID := sesEnvelope.Mail.MessageID
	var tenantID string
	for _, h := range sesEnvelope.Mail.Headers {
		if strings.EqualFold(h.Name, "X-Zoiko-Tenant-Id") {
			tenantID = h.Value
		}
	}

	switch strings.ToLower(sesEnvelope.EventType) {
	case "bounce":
		bType := BounceHard
		if strings.EqualFold(sesEnvelope.Bounce.BounceType, "Transient") {
			bType = BounceSoft
		}
		for _, r := range sesEnvelope.Bounce.BouncedRecipients {
			events = append(events, &WebhookEvent{
				EventID:           uuid.NewString(),
				Provider:          "ses",
				EventType:         EventTypeBounce,
				BounceType:        bType,
				RecipientEmail:    strings.ToLower(r.EmailAddress),
				ProviderMessageID: msgID,
				TenantID:          tenantID,
				DiagnosticCode:    r.DiagnosticCode,
				OccurredAt:        time.Now().UTC(),
				RawPayload:        payload,
			})
		}
	case "complaint":
		for _, r := range sesEnvelope.Complaint.ComplainedRecipients {
			events = append(events, &WebhookEvent{
				EventID:           uuid.NewString(),
				Provider:          "ses",
				EventType:         EventTypeComplaint,
				RecipientEmail:    strings.ToLower(r.EmailAddress),
				ProviderMessageID: msgID,
				TenantID:          tenantID,
				OccurredAt:        time.Now().UTC(),
				RawPayload:        payload,
			})
		}
	case "delivery":
		for _, dest := range sesEnvelope.Mail.Destination {
			events = append(events, &WebhookEvent{
				EventID:           uuid.NewString(),
				Provider:          "ses",
				EventType:         EventTypeDelivered,
				RecipientEmail:    strings.ToLower(dest),
				ProviderMessageID: msgID,
				TenantID:          tenantID,
				OccurredAt:        time.Now().UTC(),
				RawPayload:        payload,
			})
		}
	default:
		// Fall back to standard parser if not SES specific structure
		return n.normalizeStandard("ses", payload)
	}

	return events, nil
}

// normalizeSendGrid handles SendGrid event webhook array.
func (n *Normalizer) normalizeSendGrid(payload []byte) ([]*WebhookEvent, error) {
	var sgEvents []struct {
		Event       string `json:"event"`
		Type        string `json:"type"`
		Email       string `json:"email"`
		SGMessageID string `json:"sg_message_id"`
		Reason      string `json:"reason"`
		TenantID    string `json:"tenant_id"`
		Timestamp   int64  `json:"timestamp"`
	}

	if err := json.Unmarshal(payload, &sgEvents); err != nil {
		return n.normalizeStandard("sendgrid", payload)
	}

	var events []*WebhookEvent
	for _, sg := range sgEvents {
		if strings.TrimSpace(sg.Email) == "" {
			continue
		}
		evType, bType := classifyEventType(sg.Event, sg.Type)
		occ := time.Now().UTC()
		if sg.Timestamp > 0 {
			occ = time.Unix(sg.Timestamp, 0).UTC()
		}

		events = append(events, &WebhookEvent{
			EventID:           uuid.NewString(),
			Provider:          "sendgrid",
			EventType:         evType,
			BounceType:        bType,
			RecipientEmail:    strings.ToLower(sg.Email),
			ProviderMessageID: sg.SGMessageID,
			TenantID:          sg.TenantID,
			DiagnosticCode:    sg.Reason,
			OccurredAt:        occ,
			RawPayload:        payload,
		})
	}

	return events, nil
}

func classifyEventType(rawType, rawBounceType string) (NormalizedEventType, BounceClassification) {
	t := strings.ToLower(strings.TrimSpace(rawType))
	b := strings.ToLower(strings.TrimSpace(rawBounceType))

	switch {
	case strings.Contains(t, "delivered") || strings.Contains(t, "delivery"):
		return EventTypeDelivered, ""
	case strings.Contains(t, "complaint") || strings.Contains(t, "spam"):
		return EventTypeComplaint, ""
	case strings.Contains(t, "unsubscribe") || strings.Contains(t, "unsub"):
		return EventTypeUnsubscribe, ""
	case strings.Contains(t, "dropped"):
		return EventTypeDropped, ""
	case strings.Contains(t, "bounce"):
		if strings.Contains(b, "soft") || strings.Contains(b, "transient") {
			return EventTypeBounce, BounceSoft
		}
		return EventTypeBounce, BounceHard
	default:
		return EventTypeDropped, ""
	}
}
