package whout

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/events"
)

// Event types this domain emits through the service's transactional outbox. They are
// never fanned back out to webhook subscriptions (see IsOwnEvent): a subscriber to
// webhook.* would otherwise be sent an event about each delivery of that event.
const (
	EventSubscriptionActivated = "webhook.subscription.activated"
	EventSubscriptionDisabled  = "webhook.subscription.disabled"
	EventDeliveryFailed        = "webhook.delivery.failed"
	EventDeadLettered          = "webhook.delivery.dead-lettered"
	EventReplayRequested       = "webhook.replay.requested"
	EventControlFailed         = "webhook.control.failed"
)

// IsOwnEvent reports whether an event type belongs to this domain.
func IsOwnEvent(eventType string) bool { return len(eventType) >= 8 && eventType[:8] == "webhook." }

// Change is one atomic subscription mutation: the new state, the optimistic-lock
// version it was derived from, the log entry that explains it, and the event it
// emits, all committed together or not at all.
type Change struct {
	TenantID        string
	Sub             *Subscription // the complete new state
	ExpectedVersion int
	Log             LogEntry
	Event           *events.Outbound
	// DeadLetterPending moves every undelivered delivery of the subscription to the
	// dead letters (used by revoke).
	DeadLetterPending bool
	DeadLetterReason  string
	// ResetHealth clears the failure counters and the breaker (a fresh approval or a
	// resume starts the destination with a clean record).
	ResetHealth bool
}

// Claim names one delivery a dispatcher took the lease on.
type Claim struct {
	TenantID   string
	DeliveryID string
}

// FinishInput records how one attempt ended and what happens next, atomically.
type FinishInput struct {
	TenantID   string
	DeliveryID string
	AttemptNo  int
	Now        time.Time

	Outcome            AttemptOutcome
	ResponseCode       *int
	ResponseBodyHash   string
	ResponseBodyBytes  *int
	ErrorClass         string
	SignatureTimestamp *int64
	SignatureKeyPaths  []string

	NewStatus        DeliveryStatus
	NextAttemptAt    time.Time
	DeadLetterReason string

	// Health bookkeeping. Success resets the failure counters; Failure advances them
	// and may open the breaker or disable the subscription.
	Success                 bool
	Failure                 bool
	BreakerThreshold        int
	BreakerCooldown         time.Duration
	DisableAfterDeadLetters int
	ControlFailure          bool // a minimization block: also raises webhook.control.failed
}

// Store is the persistence the domain needs. The PostgreSQL implementation lives in
// internal/store; every method runs in a tenant-scoped transaction except ClaimDue,
// which names itself as the dispatcher.
type Store interface {
	CreateSubscription(ctx context.Context, s *Subscription, e LogEntry) (*Subscription, error)
	GetSubscription(ctx context.Context, tenantID, id string) (*Subscription, error)
	ListSubscriptions(ctx context.Context, tenantID string, status Status, limit int) ([]Subscription, error)
	ApplyChange(ctx context.Context, ch Change) (*Subscription, error)
	ListLog(ctx context.Context, tenantID, subscriptionID string, limit int) ([]LogEntry, error)
	// HasMatchingActive reports whether the tenant has an ACTIVE subscription that selects
	// the event type, so events nobody subscribed to create no inbox receipt at all.
	HasMatchingActive(ctx context.Context, tenantID, eventType string) (bool, error)

	// FanOut turns one source event into deliveries for every ACTIVE subscription of
	// the tenant that selects it. It runs inside the caller's transaction (the inbox
	// transaction) so the receipt and the deliveries commit together.
	FanOut(ctx context.Context, tx pgx.Tx, ev SourceEvent) (FanOutResult, error)

	ClaimDue(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]Claim, error)
	LoadForSend(ctx context.Context, tenantID, deliveryID string) (*Delivery, *Subscription, error)
	BeginAttempt(ctx context.Context, tenantID string, d *Delivery, host string, now time.Time, lease time.Duration) (int, error)
	FinishAttempt(ctx context.Context, in FinishInput) error
	Defer(ctx context.Context, tenantID, deliveryID string, until time.Time) error

	GetDelivery(ctx context.Context, tenantID, deliveryID string) (*Delivery, error)
	ListDeliveries(ctx context.Context, tenantID, subscriptionID string, status DeliveryStatus, limit int) ([]Delivery, error)
	ListDeliveryAttempts(ctx context.Context, tenantID, deliveryID string) ([]Attempt, error)
	Health(ctx context.Context, tenantID, subscriptionID string, now time.Time) (*Health, error)
	Replay(ctx context.Context, tenantID, deliveryID string, e LogEntry, ev *events.Outbound, now time.Time) (*Delivery, error)
}

// FanOutResult says what a fan-out did.
type FanOutResult struct {
	Matched          int // active subscriptions that selected the event
	Created          int // new deliveries
	Duplicates       int // deliveries that already existed
	Blocked          int // subscriptions skipped because the event could not be minimized safely
	BeforeActivation int // subscriptions skipped because the event predates their activation
}

// ── event builders ───────────────────────────────────────────────────────────

func subscriptionEvent(eventType string, s *Subscription, actor, correlation, reason string) (events.Outbound, error) {
	return events.Build(eventType, correlation, s.TenantID, s.LegalEntityID, actor, s.SubscriptionID, map[string]any{
		"subscription_id": s.SubscriptionID, "version": s.Version, "status": string(s.Status),
		"config_fingerprint": s.ConfigFingerprint, "destination_host": hostOnly(s.DestinationURL),
		"reason": reason,
	})
}

// ActivatedEvent is webhook.subscription.activated.
func ActivatedEvent(s *Subscription, actor, correlation string) (events.Outbound, error) {
	return subscriptionEvent(EventSubscriptionActivated, s, actor, correlation, "")
}

// DisabledEvent is webhook.subscription.disabled.
func DisabledEvent(s *Subscription, actor, correlation, reason string) (events.Outbound, error) {
	return subscriptionEvent(EventSubscriptionDisabled, s, actor, correlation, reason)
}

// ReplayEvent is webhook.replay.requested.
func ReplayEvent(s *Subscription, d *Delivery, operator, approver, correlation, reason string) (events.Outbound, error) {
	return events.Build(EventReplayRequested, correlation, s.TenantID, s.LegalEntityID, operator, d.DeliveryID, map[string]any{
		"subscription_id": s.SubscriptionID, "delivery_id": d.DeliveryID, "event_id": d.EventID,
		"approver": approver, "reason": reason, "replay_count": d.ReplayCount + 1,
	})
}

func deliveryEvent(eventType string, tenantID, legalEntityID string, d *Delivery, attemptNo int, outcome AttemptOutcome, class, reason string) (events.Outbound, error) {
	payload := map[string]any{
		"delivery_id": d.DeliveryID, "subscription_id": d.SubscriptionID, "event_id": d.EventID,
		"attempt": attemptNo, "outcome": string(outcome), "error_class": class, "reason": reason,
		"payload_hash": d.BodyHash,
	}
	if eventType == EventDeadLettered {
		// a dead letter is never ownerless: it names the queue that must decide it (INT-CTRL-034)
		payload["owner"] = DeadLetterOwner
	}
	return events.Build(eventType, d.CorrelationID, tenantID, legalEntityID, "system:webhook-dispatcher", d.DeliveryID, payload)
}

// DeadLetterOwner is the queue that owns a dead-lettered webhook delivery. Replay needs an approver and a reason.
const DeadLetterOwner = "integration-operations"

// FailedEvent is webhook.delivery.failed (one failed attempt).
func FailedEvent(tenantID, legalEntityID string, d *Delivery, attemptNo int, outcome AttemptOutcome, class string) (events.Outbound, error) {
	return deliveryEvent(EventDeliveryFailed, tenantID, legalEntityID, d, attemptNo, outcome, class, "")
}

// DeadLetteredEvent is webhook.delivery.dead-lettered.
func DeadLetteredEvent(tenantID, legalEntityID string, d *Delivery, attemptNo int, outcome AttemptOutcome, class, reason string) (events.Outbound, error) {
	return deliveryEvent(EventDeadLettered, tenantID, legalEntityID, d, attemptNo, outcome, class, reason)
}

// ControlFailedEvent is webhook.control.failed: a body was stopped before it was sent
// because it contained something outside the approved scope (INT-NP-017).
func ControlFailedEvent(tenantID, legalEntityID string, d *Delivery, reason string) (events.Outbound, error) {
	return deliveryEvent(EventControlFailed, tenantID, legalEntityID, d, d.AttemptCount, OutcomeBlocked, "scope_violation", reason)
}
