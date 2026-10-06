package policy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/ncd"
	"zoiko.io/notification-svc/internal/preference"
	"zoiko.io/notification-svc/internal/privacy"
)

// DirectSendGuard puts the delivery controls the ledger pipeline already has in
// front of the direct send path (ZS-SVC-Y-001 INV-24, INV-30, NP-13, NP-56) and,
// since the identity plan's step 5, runs them through the SAME policy engine.
//
// WHY. POST /v1/notifications, the retry worker and an explicit resend all reach
// the provider through one Deliverer, and until this guard that Deliverer asked
// nobody anything: a recipient who had hard-bounced, complained or been
// suppressed was still mailed, and the kill switch did not apply. The ledger
// pipeline (POST /v1/notifications/events/ingest) checked both. Controls that
// exist on one of two send paths are not controls.
//
// ONE GATE. The decision is made by the same PolicyResolver the ledger orchestrator
// uses (the precedence engine), with the message's own communication class. The
// rules therefore cannot drift between the two paths: what blocks a ledger message
// blocks the same direct message, and a class the engine does not know fails closed.
//
// WHERE. At the Deliverer, because that is the last point before the provider is
// called, which is what INV-24 asks for ("suppression is checked immediately
// before provider submission"), and because the first attempt, every retry and
// every resend pass through it. The ledger pipeline keeps the unwrapped
// Deliverer: it evaluates the engine itself, before rendering.
//
// CLASS. A notification states its class when it is created (migration 000019). The
// direct path accepts S0 (security), T0 (transactional) and A1 (operational); a
// notification that stated none is judged as T0, which is how every direct send was
// treated before classes existed. M1 (marketing) and L1 (lifecycle) are refused
// here even if a row somehow carries them: they need the stream identity and
// one-click unsubscribe headers only the ledger pipeline provides (INV-07).
//
// FAILURE MODES. A policy refusal is a terminal FAILED outcome, not a retry: the
// situation does not improve with time. A kill switch is an operator pause, so the
// outcome is retryable and the notification is delivered after the switch is lifted.
// A policy evaluation that itself fails blocks delivery (fail closed, INV-30) and is
// retryable.
type DirectSendGuard struct {
	inner      Deliverer
	policy     PolicyResolver
	killSwitch KillSwitch
	privacy    PrivacyGate
	prefs      PreferenceSource
	now        func() time.Time
	log        *zap.Logger
}

// PreferenceSource reads a recipient's convenience profile. A recipient who has expressed
// none returns domain.ErrPreferencesNotFound. The Postgres store satisfies it.
type PreferenceSource interface {
	GetPreferences(ctx context.Context, principalID string) (*domain.RecipientPreferences, error)
}

// WithPreferences enables recipient preferences (NCD-02 5.3): a routine (A1) message is
// held through the recipient's quiet hours and refused on a channel they muted. Security
// and transactional messages are never touched. A profile that cannot be read or applied
// withholds the message (retryable) rather than risk a 3am notification.
func (g *DirectSendGuard) WithPreferences(p PreferenceSource) *DirectSendGuard {
	g.prefs = p
	return g
}

// applyPreferences returns (outcome, true) when a preference stops this delivery now.
func (g *DirectSendGuard) applyPreferences(ctx context.Context, n domain.Notification, class ledger.CommunicationClass) (domain.DeliveryOutcome, bool) {
	if g.prefs == nil || !preference.Subject(string(class)) {
		return domain.DeliveryOutcome{}, false
	}
	p, err := g.prefs.GetPreferences(svcmiddleware.WithTenant(ctx, n.TenantID), n.RecipientPrincipalID)
	if errors.Is(err, domain.ErrPreferencesNotFound) {
		return domain.DeliveryOutcome{}, false
	}
	if err != nil {
		g.log.Error("direct send held: recipient preferences unreadable; the provider was not called",
			zap.String("notification_id", n.NotificationID), zap.Error(err))
		return domain.DeliveryOutcome{Reason: "recipient preferences unavailable; routine delivery withheld: " + err.Error(), Retryable: true}, true
	}
	d := preference.Evaluate(p, string(class), n.Channel, g.now())
	switch d.Effect {
	case preference.Mute:
		return domain.DeliveryOutcome{Reason: d.Reason, BlockCode: ncd.ChannelSuppressed}, true
	case preference.Defer:
		g.log.Info("direct send deferred by recipient quiet hours; the provider was not called",
			zap.String("notification_id", n.NotificationID), zap.Time("until", d.Until))
		return domain.DeliveryOutcome{Reason: d.Reason, Retryable: true, DeferUntil: d.Until, BlockCode: ncd.QuietHourDeferred}, true
	case preference.Unusable:
		return domain.DeliveryOutcome{Reason: d.Reason, Retryable: true, BlockCode: ncd.QuietHourDeferred}, true
	}
	return domain.DeliveryOutcome{}, false
}

// PrivacyGate asks the privacy authority whether an intent-bound message may use the
// recipient's data (ZS-SVC-Y-001 NCD-02 5.3). *privacy.Gate satisfies it.
type PrivacyGate interface {
	Check(ctx context.Context, n domain.Notification) privacy.Outcome
}

// WithPrivacyGate enables privacy enforcement. It runs after the local controls (kill
// switch, class, suppression) so the remote question is asked only for a message that
// would otherwise go out, and it is evidence-recording: the decision id and result are
// returned on the outcome and stored with the attempt.
func (g *DirectSendGuard) WithPrivacyGate(p PrivacyGate) *DirectSendGuard {
	g.privacy = p
	return g
}

// Deliverer is the transport the guard wraps.
type Deliverer interface {
	Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome
}

// PolicyResolver is the engine both send paths consult. The precedence engine
// satisfies it.
type PolicyResolver interface {
	Evaluate(ctx context.Context, intent *ledger.MessageIntent, stream ledger.SenderStream) (ledger.PolicyDecision, error)
}

// KillSwitch reports whether delivery is halted for a tenant and template. The
// ledger's KillSwitchManager satisfies it.
type KillSwitch interface {
	Check(ctx context.Context, tenantID, templateKey string) (engaged bool, reason string)
}

// NewDirectSendGuard wraps inner. A nil policy resolver or kill switch is refused: a
// guard that quietly guards nothing is the defect this type exists to remove.
func NewDirectSendGuard(inner Deliverer, policy PolicyResolver, killSwitch KillSwitch, log *zap.Logger) (*DirectSendGuard, error) {
	if inner == nil || policy == nil || killSwitch == nil {
		return nil, fmt.Errorf("direct send guard needs a deliverer, a policy resolver and a kill switch")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &DirectSendGuard{inner: inner, policy: policy, killSwitch: killSwitch, now: time.Now, log: log}, nil
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

	class := ledger.CommunicationClass(n.CommunicationClass)
	if class == "" {
		class = ledger.ClassT0
	}
	stream, ok := ledger.StreamForDirectClass(class)
	if !ok {
		g.log.Error("direct send refused: class is not permitted on the direct path; the provider was not called",
			zap.String("notification_id", n.NotificationID), zap.String("class", string(class)))
		return domain.DeliveryOutcome{
			Reason:    ncd.Format(ncd.MarketingPermissionBlock) + fmt.Sprintf(": communication class %q is not permitted on the direct send path; use the ledger pipeline", class),
			Retryable: false,
			BlockCode: ncd.MarketingPermissionBlock,
		}
	}

	decision, err := g.policy.Evaluate(ctx, &ledger.MessageIntent{
		TenantID:             n.TenantID,
		LegalEntityID:        n.LegalEntityID,
		RecipientPrincipalID: n.RecipientPrincipalID,
		RecipientEmail:       n.RecipientAddress,
		Channel:              n.Channel,
		CommunicationClass:   class,
	}, stream)
	if err != nil {
		// Fail closed (INV-30): a policy that cannot be evaluated is not permission.
		g.log.Error("direct send held: policy evaluation failed; the provider was not called",
			zap.String("notification_id", n.NotificationID), zap.String("tenant_id", n.TenantID), zap.Error(err))
		return domain.DeliveryOutcome{
			Reason:    "policy evaluation unavailable; delivery withheld: " + err.Error(),
			Retryable: true,
		}
	}
	if !decision.Allowed {
		g.log.Info("direct send refused by policy; the provider was not called",
			zap.String("notification_id", n.NotificationID), zap.String("tenant_id", n.TenantID),
			zap.String("class", string(class)), zap.String("rule", decision.RuleName), zap.String("reason", decision.Reason))
		return domain.DeliveryOutcome{
			Reason:    ncd.Format(ncd.ChannelSuppressed) + ": refused by delivery policy (" + decision.RuleName + "): " + decision.Reason,
			Retryable: false,
			BlockCode: ncd.ChannelSuppressed,
		}
	}

	if out, held := g.applyPreferences(ctx, n, class); held {
		return out
	}

	if g.privacy == nil {
		return g.inner.Deliver(ctx, n)
	}
	pv := g.privacy.Check(ctx, n)
	if !pv.Applies {
		// No intent: the legacy, ungoverned send has no privacy binding to enforce.
		return g.inner.Deliver(ctx, n)
	}
	if !pv.Allow {
		return domain.DeliveryOutcome{
			Reason:            pv.Reason,
			Retryable:         pv.Retryable,
			PrivacyDecisionID: pv.DecisionID,
			PrivacyResult:     pv.Result,
			BlockCode:         ncd.PrivacyPermissionBlocked,
		}
	}
	out := g.inner.Deliver(ctx, n)
	out.PrivacyDecisionID, out.PrivacyResult = pv.DecisionID, pv.Result
	return out
}
