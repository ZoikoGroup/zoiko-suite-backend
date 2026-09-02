// Package deliver hands a notification to the transport its channel names.
//
// It replaces handler.StubDeliverer, which reported every channel as delivered
// by logging it. That stub was honestly documented and still had a real
// consequence: an EMAIL notification was recorded SENT, published a
// notification.sent event, and told the operator it had gone out, when no
// provider had ever seen it. The frontend said so out loud on the send form —
// "no real provider is wired up — SENT means recorded, not received" — which
// is the kind of caveat that survives in a UI string long after everyone has
// stopped reading it.
//
// The Router below is deliberately not one adapter per channel behind a single
// abstraction. The four channels are not four flavours of the same operation:
//
//	IN_APP   is terminal here. The row in the register IS the delivery, so
//	         "delivering" it means doing nothing and saying so accurately.
//	EMAIL    leaves the platform through a provider.
//	SMS      withdrawn. The handler no longer accepts it; the branch below
//	         exists only to answer for rows written while it did.
//	WEBHOOK  is machine-to-machine. ZS-SVC-Y-001 §1.3 puts it outside NCD's
//	         scope entirely — XIC is authoritative for those exchanges.
//
// The last two refuse rather than pretend. A channel with no provider behind
// it produces a FAILED record naming the missing provider, which is a true
// statement about the platform; the alternative is the stub's claim, which was
// not.
package deliver

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
)

// Provider sends one message through one transport.
//
// Send returns a receipt — whatever the provider offers as evidence it took
// the message — or an error. An error is classified by the Router through
// Retryable, not by the provider returning a bool, so a provider
// implementation cannot accidentally report a permanent refusal as success.
type Provider interface {
	// Name identifies the provider in evidence and logs, e.g. "smtp".
	Name() string

	// Send hands over one message and returns acceptance evidence.
	Send(ctx context.Context, msg Message) (receipt string, err error)
}

// Message is one outbound message, already rendered.
//
// The body is HTML because every template in the catalogue is HTML. Subject
// and To are untrusted with respect to the transport: both reach an SMTP
// header, and providers are assembled from strings, so escaping and
// header-injection defence belong at the transport boundary, not here.
type Message struct {
	To       string
	Subject  string
	HTMLBody string

	// CorrelationID travels into the message headers so a delivered mail can
	// be tied back to the send that produced it without matching on subject.
	CorrelationID string
}

// RetryableError marks a provider failure as worth attempting again. A
// provider wraps its transient failures in this; everything else is treated as
// settled.
type RetryableError struct{ Err error }

func (e RetryableError) Error() string { return e.Err.Error() }
func (e RetryableError) Unwrap() error { return e.Err }

// Retryable wraps err as transient.
func Retryable(err error) error { return RetryableError{Err: err} }

// isRetryable reports whether a provider error asked to be retried.
func isRetryable(err error) bool {
	var r RetryableError
	return errors.As(err, &r)
}

// Router dispatches a notification to the transport for its channel. It
// implements handler.Deliverer.
type Router struct {
	email Provider
	log   *zap.Logger
}

// NewRouter builds a Router. email may be nil, which is how a deployment says
// no mail provider is configured: EMAIL then fails with that as the stated
// reason rather than being silently reported as sent.
func NewRouter(email Provider, log *zap.Logger) *Router {
	return &Router{email: email, log: log}
}

func (r *Router) Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome {
	switch n.Channel {
	case domain.ChannelInApp:
		return r.deliverInApp(n)
	case domain.ChannelEmail:
		return r.deliverEmail(ctx, n)
	case domain.ChannelSMS:
		// Unreachable for a NEW notification: the handler no longer accepts
		// SMS, so a send naming it is refused with 400 at the boundary rather
		// than recorded as a delivery that failed.
		//
		// Kept for the rows that already exist. SMS was accepted for a period
		// and every one of those notifications failed; if any is still PENDING
		// with a schedule, the retry worker will route it here, and it should
		// get a reason that says what actually happened rather than the
		// default branch's "unroutable channel", which would read as a bug.
		return domain.DeliveryOutcome{
			Reason: "SMS delivery has been withdrawn from this service and no provider was ever " +
				"configured; this notification was recorded but never transmitted",
			// Settled: no timer will make a withdrawn channel deliverable.
			Retryable: false,
		}
	case domain.ChannelWebhook:
		return domain.DeliveryOutcome{
			Reason: "WEBHOOK delivery is not owned by notification-svc; " +
				"machine-to-machine exchange is XIC's authority (ZS-SVC-Y-001 §1.3)",
			Retryable: false,
		}
	default:
		// Unreachable: the handler rejects an unknown channel at the request
		// boundary with 400. Kept because the alternative to an explicit
		// default here is a zero-value DeliveryOutcome, which reads as
		// "refused, no reason given" — the exact shape of the 'PIGEON' rows
		// migration 000002 had to preserve rather than delete.
		return domain.DeliveryOutcome{
			Reason: fmt.Sprintf("unroutable channel %q reached the delivery router", n.Channel),
		}
	}
}

// deliverInApp concludes an in-app notice.
//
// There is no transport. The notification row is already committed by the time
// this runs, the recipient reads it from this service's own register, and
// nothing further has to happen for it to have arrived. Saying "delivered" is
// therefore not a stub — it is the strongest true statement available about an
// in-app notice, and a stronger one than any remote channel can make, because
// no third party stands between the claim and the fact.
func (r *Router) deliverInApp(n domain.Notification) domain.DeliveryOutcome {
	return domain.DeliveryOutcome{
		Delivered:        true,
		ProviderResponse: "in-app; readable from the recipient's notification register",
	}
}

func (r *Router) deliverEmail(ctx context.Context, n domain.Notification) domain.DeliveryOutcome {
	if r.email == nil {
		return domain.DeliveryOutcome{
			Reason: "no email provider is configured (NOTIFICATION_EMAIL_PROVIDER unset); " +
				"the notification is recorded but was not transmitted",
			Retryable: false,
		}
	}
	if n.RecipientAddress == "" {
		// The handler resolves the address before the record is written, so
		// reaching here means the resolution path was bypassed. Refusing is
		// the point: an empty To would otherwise become an SMTP error at the
		// far end, reported as a provider failure for what is our own bug.
		return domain.DeliveryOutcome{
			Reason:    "no recipient address resolved for an EMAIL notification",
			Retryable: false,
		}
	}

	receipt, err := r.email.Send(ctx, Message{
		To:            n.RecipientAddress,
		Subject:       n.Subject,
		HTMLBody:      n.Body,
		CorrelationID: n.CorrelationID,
	})
	if err != nil {
		retry := isRetryable(err)
		r.log.Warn("email delivery failed",
			zap.String("notification_id", n.NotificationID),
			zap.String("provider", r.email.Name()),
			zap.Bool("retryable", retry),
			// The address is not logged. It is PII, it is already on the
			// notification row under RLS, and a log line is the one place it
			// would sit outside the tenant boundary.
			zap.Error(err))
		return domain.DeliveryOutcome{
			Reason:    fmt.Sprintf("%s: %s", r.email.Name(), err.Error()),
			Retryable: retry,
		}
	}

	return domain.DeliveryOutcome{
		Delivered:        true,
		ProviderResponse: receipt,
	}
}
