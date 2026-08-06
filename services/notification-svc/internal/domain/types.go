package domain

import "time"

type Notification struct {
	NotificationID       string     `json:"notification_id"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	RecipientPrincipalID string     `json:"recipient_principal_id"`
	Channel              string     `json:"channel"` // EMAIL, SMS, IN_APP, WEBHOOK
	Subject              string     `json:"subject"`
	Body                 string     `json:"body"`
	Status               string     `json:"status"` // PENDING, SENT, FAILED
	SourceEventType      string     `json:"source_event_type,omitempty"`
	SourceReference      string     `json:"source_reference,omitempty"`
	CorrelationID        string     `json:"correlation_id"`
	FailureReason        string     `json:"failure_reason,omitempty"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	CreatedAt            time.Time  `json:"created_at"`
	SentAt               *time.Time `json:"sent_at,omitempty"`
}

type SendNotificationRequest struct {
	RecipientPrincipalID string `json:"recipient_principal_id"`
	LegalEntityID        string `json:"legal_entity_id"`
	Channel              string `json:"channel"`
	Subject              string `json:"subject"`
	Body                 string `json:"body"`
	SourceEventType      string `json:"source_event_type,omitempty"`
	SourceReference      string `json:"source_reference,omitempty"`
	CorrelationID        string `json:"correlation_id"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrNotificationNotFound    = errorString("notification not found")
	ErrAuthorizationDenied     = errorString("authorization denied for notification action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("notification store unavailable")
)
