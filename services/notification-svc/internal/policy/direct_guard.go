package policy

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
)

// DirectSendGuard puts the delivery controls the ledger pipeline already has in
// front of the direct send path (ZS-SVC-Y-001 INV-24, INV-30, NP-13, NP-56).
//
// WHY. POST /v1/notifications, the retry worker and an explicit resend all reach
// the provider through one Deliverer, and until this guard that Deliverer asked
// nobody anything: a recipient who had hard-bounced, complained or been
// suppressed was still mailed, and the kill switch did not apply. The ledger
// pipeline (POST /v1/notifications/events/ingest) checked both. Controls that
// exist on one of two send paths are not controls.
//
// WHERE. At the Deliverer, because that is the last point before the provider is
// called, which is what INV-24 asks for ("suppression is checked immediately
// before provider submission"), and because the first attempt, every retry and
// every resend pass through it. The ledger pipeline keeps the unwrapped
// Deliverer: it evaluates its own policy, with a real communication class, before
// rendering.
//
// ASSUMPTION, stated rather than hidden: a direct send carries no communication
// class, so it is treated as TRANSACTIONAL / T0. Under the precedence rules that
// means hard bounces, complaints and administrative suppressions block it and an
// unsubscribe does not (a transactional notice is not marketing). When direct
// sends gain an explicit purpose class (Y-001 NCD-01) this default should give
// way to it.
//
// FAILURE MODES. A suppressed address is a terminal FAILED outcome, not a retry:
// the situation does not improve with time. A kill switch is an operator pause,
// so the outcome is retryable and the notification is delivered after the switch
// is lifted. A suppression lookup that itself fails blocks delivery (fail
// closed, INV-30) and is retryable.
type DirectSendGuard struct {
	inner       Deliverer
	suppression SuppressionChecker
	killSwitch  KillSwitch
	log         *zap.Logger
}

// Deliverer is the transport the guard wraps.
type Deliverer interface {
	Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome
}

// KillSwitch reports whether delivery is halted for a tenant and template. The
// ledger's KillSwitchManager satisfies it.
type KillSwitch interface {
	Check(ctx context.Context, tenantID, templateKey string) (engaged bool, reason string)
}

// NewDirectSendGuard wraps inner. A nil suppression checker or kill switch is
// refused: a guard that quietly guards nothing is the defect this type exists to
// remove.
func NewDirectSendGuard(inner Deliverer, suppression SuppressionChecker, killSwitch KillSwitch, log *zap.Logger) (*DirectSendGuard, error) {
	if inner == nil || suppression == nil || killSwitch == nil {
		return nil, fmt.Errorf("direct send guard needs a deliverer, a suppression checker and a kill switch")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &DirectSendGuard{inner: inner, suppression: suppression, killSwitch: killSwitch, log: log}, nil
}

// Deliver applies the controls and, only if they pass, hands the notification to
// the wrapped Deliverer. Only EMAIL is guarded: an IN_APP notice is delivered by
// being recorded and has no address to suppress.
func (g *DirectSendGuard) Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome {
	if n.Channel != domain.ChannelEmail {
		return g.inner.Deliver(ctx, n)
	}

	if engaged, reason := g.killSwitch.Check(ctx, n.TenantID, n.TemplateID); engaged {
		g.log.Warn("direct send held by kill switch; the provider was not called",
			zap.String("notification_id", n.NotificationID), zap.String("tenant_id", n.TenantID), zap.String("reason", reason))
		return domain.DeliveryOutcome{
			Reason:    "delivery halted by kill switch: " + reason,
			Retryable: true,
		}
	}

	suppressed, why, err := g.suppression.IsEmailSuppressed(ctx, n.TenantID, n.RecipientAddress, ledger.StreamTransactional, ledger.ClassT0)
	if err != nil {
		// Fail closed (INV-30): an unreadable suppression list is not permission.
		g.log.Error("direct send held: suppression lookup failed; the provider was not called",
			zap.String("notification_id", n.NotificationID), zap.String("tenant_id", n.TenantID), zap.Error(err))
		return domain.DeliveryOutcome{
			Reason:    "suppression check unavailable; delivery withheld: " + err.Error(),
			Retryable: true,
		}
	}
	if suppressed {
		g.log.Info("direct send refused: address is suppressed; the provider was not called",
			zap.String("notification_id", n.NotificationID), zap.String("tenant_id", n.TenantID), zap.String("reason", why))
		return domain.DeliveryOutcome{
			Reason:    "recipient address is suppressed (" + why + ")",
			Retryable: false,
		}
	}

	return g.inner.Deliver(ctx, n)
}
