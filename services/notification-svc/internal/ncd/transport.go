package ncd

import (
	"context"
	"strings"
	"time"

	"zoiko.io/notification-svc/internal/domain"
)

// EmailDeliverer is the existing SMTP router (internal/deliver.Router).
type EmailDeliverer interface {
	Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome
}

// InboxWriter places an in-app notice in the recipient's authenticated
// inbox — the same register GET /v1/notifications and the bell read — so an
// NCD communication and a legacy notice arrive in one place.
type InboxWriter interface {
	DeliverInApp(ctx context.Context, tenantID string, n domain.Notification) error
}

// RouterTransport adapts the service's existing transports to bindings.
// Provider credentials stay in the transport's configuration (SMTP_* from
// the environment / secret store), never in a binding or a template (INV-26).
type RouterTransport struct {
	Email EmailDeliverer
	Inbox InboxWriter
}

// Submit sends one message through the binding's provider.
func (t RouterTransport) Submit(ctx context.Context, b Binding, m Message) SubmitOutcome {
	switch b.ProviderName {
	case "in-app":
		if t.Inbox == nil {
			return SubmitOutcome{Reason: "no inbox writer configured", ProviderName: "in-app"}
		}
		now := time.Now().UTC()
		err := t.Inbox.DeliverInApp(ctx, m.TenantID, domain.Notification{
			NotificationID: m.AttemptID, TenantID: m.TenantID, LegalEntityID: m.LegalEntityID,
			RecipientPrincipalID: m.RecipientPrincipalID, Channel: domain.ChannelInApp,
			Subject: m.Subject, Body: m.Body, Status: domain.StatusSent,
			SourceEventType: "ncd.communication", SourceReference: m.CommunicationID,
			CorrelationID: m.IdempotencyToken, CreatedByPrincipalID: workerPrincipal, CreatedAt: now, SentAt: &now,
			ProviderResponse: "in-app inbox; attempt " + m.AttemptID,
		})
		if err != nil {
			// The register write did not commit, so nothing reached the
			// inbox: a retry cannot duplicate (the write is idempotent on the
			// attempt token anyway).
			return SubmitOutcome{Retryable: true, Reason: "inbox write failed: " + err.Error(), ProviderName: "in-app"}
		}
		return SubmitOutcome{Accepted: true, DeliveredNow: true, ProviderName: "in-app", ProviderMessageID: "inbox:" + m.AttemptID}
	case "smtp":
		if t.Email == nil {
			return SubmitOutcome{Reason: "no email transport configured", ProviderName: "smtp"}
		}
		out := t.Email.Deliver(ctx, domain.Notification{
			NotificationID: m.AttemptID, TenantID: m.TenantID, LegalEntityID: m.LegalEntityID,
			RecipientPrincipalID: m.RecipientPrincipalID, RecipientAddress: m.To, Channel: domain.ChannelEmail,
			Subject: m.Subject, Body: m.Body, CorrelationID: m.CorrelationID,
			// The provider idempotency token of §6.1, carried in the message
			// so a provider export can be reconciled against the attempt.
			Headers: map[string]string{"X-Zoiko-Idempotency-Token": m.IdempotencyToken, "X-Zoiko-Communication-Id": m.CommunicationID},
		})
		return SubmitOutcome{
			Accepted: out.Delivered, Unknown: out.Unknown, Retryable: out.Retryable, Reason: out.Reason,
			ProviderName: out.ProviderName, ProviderMessageID: messageID(out.ProviderResponse),
		}
	}
	return SubmitOutcome{Reason: "binding " + b.BindingID + " names provider " + b.ProviderName + ", which has no transport", ProviderName: b.ProviderName}
}

// messageID extracts "message-id=<...>" from the SMTP receipt.
func messageID(receipt string) string {
	const k = "message-id="
	i := strings.Index(receipt, k)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(receipt[i+len(k):])
}
