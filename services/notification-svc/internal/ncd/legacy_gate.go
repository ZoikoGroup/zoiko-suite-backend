package ncd

import (
	"context"

	"zoiko.io/notification-svc/internal/domain"
)

// GatedDeliverer puts the canonical suppression check in front of the
// pre-NCD send path (POST /v1/notifications, its resend, and the retry
// worker). Before it, that path consulted no suppression list at all: a
// hard-bounced, complained or security-held address was mailed again on
// every send, and the email_suppressions rows the webhook processor wrote
// were read by nothing but the ledger pipeline.
//
// It is the last step before the provider — INV-24, "suppression is checked
// immediately before provider submission" — and it fails closed: if the
// suppression store cannot be read, nothing is sent and the attempt is
// retryable (NP-56).
type GatedDeliverer struct {
	Inner EmailDeliverer
	Svc   *Service
}

// Deliver checks suppression, then delegates.
func (g GatedDeliverer) Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome {
	if n.Channel == domain.ChannelEmail && n.RecipientAddress != "" && n.TenantID != "" {
		r, err := g.Svc.LegacySendSuppressed(ctx, n.TenantID, n.RecipientPrincipalID, ChannelEmail, n.RecipientAddress)
		if err != nil {
			return domain.DeliveryOutcome{Retryable: true, ProviderName: "suppression-gate",
				Reason: "suppression state could not be read, so nothing was sent (fail closed, NP-56): " + err.Error()}
		}
		if r != nil {
			return domain.DeliveryOutcome{Retryable: false, ProviderName: "suppression-gate",
				Reason: string(r.Code) + " " + ReasonNames[r.Code] + ": " + r.Detail}
		}
	}
	return g.Inner.Deliver(ctx, n)
}
