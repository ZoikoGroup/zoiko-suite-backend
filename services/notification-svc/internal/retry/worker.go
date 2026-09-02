package retry

import (
	"context"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// Store is the slice of the register the worker touches.
//
// FindDueRetries is the only cross-tenant call. Everything after it is
// tenant-scoped, which is why each method below takes a context the worker has
// already installed a tenant on — the same context a request handler would
// present, so these run under exactly the row-level security a user's read
// does.
type Store interface {
	FindDueRetries(ctx context.Context, now time.Time, limit int) ([]domain.DueRetry, error)
	ClaimRetry(ctx context.Context, id, tenantID string) (bool, error)
	GetNotification(ctx context.Context, id string) (*domain.Notification, error)
	CompleteDelivery(ctx context.Context, id, newStatus, failureReason, providerResponse string, sentAt *time.Time) error
	ScheduleRetry(ctx context.Context, id, tenantID, failureReason string, attemptedAt, nextAttemptAt time.Time) error
	SetRecipientAddress(ctx context.Context, id, tenantID, address, source string) error
}

type Deliverer interface {
	Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome
}

type Publisher interface {
	PublishSent(ctx context.Context, correlationID string, n domain.Notification)
	PublishFailed(ctx context.Context, correlationID string, n domain.Notification, reason string)
}

// RecipientResolver re-resolves an address the first attempt could not get.
type RecipientResolver interface {
	ResolveEmail(ctx context.Context, tenantID, callerPrincipalID, recipientPrincipalID string) (string, error)
}

// Settled reports whether a resolution error is a fact about the recipient
// rather than a failure to reach the authority holding it. Mirrors
// identity.IsSettled; declared here so this package does not depend on the
// client package for one predicate.
type Settled func(error) bool

// Worker polls for due retries and re-attempts them.
//
// Fixed-interval polling rather than LISTEN/NOTIFY, matching
// commercial-account-svc's outbox Relay: the requirement is that a transient
// failure is eventually retried, not that it is retried within a second of
// becoming due. The backoff is measured in tens of seconds, so a poll interval
// of a few seconds is already far below the resolution that matters.
type Worker struct {
	store     Store
	deliverer Deliverer
	publisher Publisher
	recipient RecipientResolver
	settled   Settled
	policy    Policy
	interval  time.Duration
	batchSize int
	log       *zap.Logger
}

type Options struct {
	Interval  time.Duration
	BatchSize int
	Policy    Policy
}

func NewWorker(store Store, deliverer Deliverer, publisher Publisher, recipient RecipientResolver, settled Settled, opts Options, log *zap.Logger) *Worker {
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 50
	}
	return &Worker{
		store:     store,
		deliverer: deliverer,
		publisher: publisher,
		recipient: recipient,
		settled:   settled,
		policy:    opts.Policy.Normalize(),
		interval:  opts.Interval,
		batchSize: opts.BatchSize,
		log:       log,
	}
}

// Start runs until ctx is cancelled. Intended for its own goroutine.
func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.log.Info("delivery retry worker started",
		zap.Duration("interval", w.interval),
		zap.Int("batch_size", w.batchSize),
		zap.Int("max_attempts", w.policy.MaxAttempts))
	for {
		select {
		case <-ctx.Done():
			w.log.Info("delivery retry worker stopped")
			return
		case <-ticker.C:
			w.RunOnce(ctx)
		}
	}
}

// RunOnce processes one batch and returns how many notifications it actually
// attempted. Exported so a test can drive the worker deterministically instead
// of waiting on a ticker.
func (w *Worker) RunOnce(ctx context.Context) int {
	due, err := w.store.FindDueRetries(ctx, time.Now().UTC(), w.batchSize)
	if err != nil {
		w.log.Error("retry worker: failed to poll for due deliveries", zap.Error(err))
		return 0
	}

	attempted := 0
	for _, d := range due {
		select {
		case <-ctx.Done():
			// Shutting down. Unclaimed rows keep their schedule and the next
			// process to start picks them up; a claimed one is PENDING with
			// nothing scheduled, which the sweep below is for.
			return attempted
		default:
		}
		if w.attempt(ctx, d) {
			attempted++
		}
	}
	return attempted
}

// attempt re-delivers one notification. Returns whether an attempt was made.
func (w *Worker) attempt(ctx context.Context, d domain.DueRetry) bool {
	// The tenant the notification belongs to, installed exactly as a request
	// would install the caller's. Every store call below is therefore governed
	// by the same policy a user's read is, and a bug here cannot reach another
	// tenant's rows.
	tctx := svcmiddleware.WithTenant(ctx, d.TenantID)

	claimed, err := w.store.ClaimRetry(tctx, d.NotificationID, d.TenantID)
	if err != nil {
		w.log.Error("retry worker: claim failed",
			zap.String("notification_id", d.NotificationID), zap.Error(err))
		return false
	}
	if !claimed {
		// Another replica took it, or it concluded between the poll and now.
		return false
	}

	n, err := w.store.GetNotification(tctx, d.NotificationID)
	if err != nil {
		// Claiming cleared next_attempt_at, so this notification is now PENDING
		// with nothing scheduled — stalled, and invisible to the next poll.
		// Put the schedule back rather than stranding it: the read failing is
		// a reason to try later, not a reason to abandon a notice nobody has
		// been told about.
		//
		// Scheduled off attempt 1 rather than the notification's real count —
		// the read failed, so that count is not available here (n is nil).
		// That makes this the shortest backoff in the policy, which is the
		// right bias: an unreadable row is more likely a transient database
		// fault than a permanent one, and ScheduleRetry still increments the
		// stored count, so the budget remains bounded.
		if next, ok := w.policy.NextAttempt(time.Now().UTC(), 1); ok {
			if reErr := w.store.ScheduleRetry(tctx, d.NotificationID, d.TenantID,
				"could not read the notification to re-attempt it: "+err.Error(),
				time.Now().UTC(), next); reErr != nil {
				w.log.Error("retry worker: claimed a notification it can neither read nor reschedule — it is now stalled PENDING with no schedule",
					zap.String("notification_id", d.NotificationID),
					zap.NamedError("read_error", err), zap.NamedError("reschedule_error", reErr))
				return false
			}
		}
		w.log.Error("retry worker: could not read a claimed notification; rescheduled",
			zap.String("notification_id", d.NotificationID), zap.Error(err))
		return false
	}

	// A first attempt that failed because identity-context-svc was unreachable
	// left no address on the record. Re-attempting the transport with an empty
	// To would fail forever, so the resolution is retried first — it is the
	// step that actually failed.
	if domain.ChannelNeedsAddress(n.Channel) && n.RecipientAddress == "" {
		if !w.reresolve(tctx, n) {
			return true
		}
	}

	outcome := w.deliverer.Deliver(tctx, *n)
	w.conclude(tctx, n, outcome)
	return true
}

// reresolve fills in a recipient address the first attempt could not obtain.
// Returns whether delivery should proceed.
func (w *Worker) reresolve(ctx context.Context, n *domain.Notification) bool {
	if w.recipient == nil {
		w.conclude(ctx, n, domain.DeliveryOutcome{
			Reason: "no recipient address on record and no resolver configured",
		})
		return false
	}

	// CreatedByPrincipalID, not the recipient: identity-context-svc attributes
	// the read to whoever asked for the send, and naming the recipient as the
	// caller would let any send become a read of that principal's own record.
	addr, err := w.recipient.ResolveEmail(ctx, n.TenantID, n.CreatedByPrincipalID, n.RecipientPrincipalID)
	if err != nil {
		w.conclude(ctx, n, domain.DeliveryOutcome{
			Reason:    "recipient resolution failed: " + err.Error(),
			Retryable: w.settled != nil && !w.settled(err),
		})
		return false
	}

	if err := w.store.SetRecipientAddress(ctx, n.NotificationID, n.TenantID, addr, domain.AddressSourceIdentityContext); err != nil {
		w.conclude(ctx, n, domain.DeliveryOutcome{
			Reason:    "resolved a recipient address but could not record it: " + err.Error(),
			Retryable: true,
		})
		return false
	}
	n.RecipientAddress = addr
	n.RecipientAddressSource = domain.AddressSourceIdentityContext
	return true
}

// conclude records the outcome of an attempt: delivered, scheduled for another
// try, or terminally failed.
func (w *Worker) conclude(ctx context.Context, n *domain.Notification, outcome domain.DeliveryOutcome) {
	now := time.Now().UTC()

	if outcome.Delivered {
		if err := w.store.CompleteDelivery(ctx, n.NotificationID, "SENT", "", outcome.ProviderResponse, &now); err != nil {
			w.log.Error("retry worker: delivered but could not record it",
				zap.String("notification_id", n.NotificationID), zap.Error(err))
			return
		}
		n.Status, n.SentAt, n.ProviderResponse = "SENT", &now, outcome.ProviderResponse
		n.FailureReason = ""
		w.log.Info("retry worker: delivery succeeded on re-attempt",
			zap.String("notification_id", n.NotificationID),
			zap.Int("attempt", n.DeliveryAttempts+1))
		w.publisher.PublishSent(ctx, n.CorrelationID, *n)
		return
	}

	// attemptsMade counts the attempt just concluded: the column has not been
	// incremented yet — ScheduleRetry and CompleteDelivery both do that — so
	// the value the policy needs is the stored count plus this one.
	attemptsMade := n.DeliveryAttempts + 1

	if outcome.Retryable {
		if next, ok := w.policy.NextAttempt(now, attemptsMade); ok {
			if err := w.store.ScheduleRetry(ctx, n.NotificationID, n.TenantID, outcome.Reason, now, next); err != nil {
				w.log.Error("retry worker: could not reschedule",
					zap.String("notification_id", n.NotificationID), zap.Error(err))
			}
			w.log.Warn("retry worker: delivery failed, rescheduled",
				zap.String("notification_id", n.NotificationID),
				zap.Int("attempt", attemptsMade),
				zap.Time("next_attempt_at", next),
				zap.String("reason", outcome.Reason))
			// No notification.failed event. The delivery has not concluded,
			// and publishing a failure that a later attempt reverses would
			// have consumers reacting to an outcome that did not happen.
			return
		}
		// Exhausted. The reason records that, so the register does not read as
		// though a mailbox was rejected when in fact the platform gave up.
		outcome.Reason = outcome.Reason + " (no further attempts: exhausted after " +
			itoa(attemptsMade) + " of " + itoa(w.policy.MaxAttempts) + ")"
	}

	if err := w.store.CompleteDelivery(ctx, n.NotificationID, "FAILED", outcome.Reason, "", &now); err != nil {
		w.log.Error("retry worker: could not record terminal failure",
			zap.String("notification_id", n.NotificationID), zap.Error(err))
		return
	}
	n.Status, n.SentAt, n.FailureReason = "FAILED", &now, outcome.Reason
	w.log.Warn("retry worker: delivery failed terminally",
		zap.String("notification_id", n.NotificationID),
		zap.Int("attempts", attemptsMade),
		zap.String("reason", outcome.Reason))
	w.publisher.PublishFailed(ctx, n.CorrelationID, *n, outcome.Reason)
}

// itoa avoids pulling strconv in for two call sites in a log-adjacent string.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
