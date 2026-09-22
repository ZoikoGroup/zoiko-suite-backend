package retry

import (
	"context"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/telemetry"
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
	// The stranded-delivery pair. A notification left in flight — PENDING
	// with nothing scheduled — is invisible to FindDueRetries and nothing
	// else in the service would ever touch it again.
	FindStrandedDeliveries(ctx context.Context, staleBefore time.Time, limit int) ([]domain.DueRetry, error)
	ReviveStranded(ctx context.Context, id, tenantID string, staleBefore, nextAttemptAt time.Time) (bool, error)
	GetNotification(ctx context.Context, id string) (*domain.Notification, error)
	// CompleteDelivery concludes a delivery AND enqueues the event describing
	// that conclusion, in one transaction. The worker used to publish to Kafka
	// after this returned, with the error logged and discarded — so a broker
	// outage during a successful re-attempt delivered the notice and told
	// nobody. See migration 000005.
	CompleteDelivery(ctx context.Context, id, newStatus, failureReason, providerResponse string, sentAt *time.Time, ev events.Outbound) error
	ScheduleRetry(ctx context.Context, id, tenantID, failureReason string, attemptedAt, nextAttemptAt time.Time) error
	SetRecipientAddress(ctx context.Context, id, tenantID, address, source string) error
}

type Deliverer interface {
	Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome
}

// Metrics is the domain-metric surface the worker records against.
//
// It matters more here than on the request path, because nothing the worker
// does produces an HTTP response at all: a notification delivered on its fourth
// attempt, one that exhausted its budget, and one reclaimed from being stranded
// are all invisible to every request metric this service has. A nil Metrics is
// safe and means "do not record".
type Metrics interface {
	ObserveAttempt(channel, outcome, origin string, seconds float64)
	ObserveConclusion(channel, status string)
	ObserveRetryScheduled(channel string)
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
	metrics   Metrics
	stranded  Counter
	exhausted Counter
	recipient RecipientResolver
	settled   Settled
	policy    Policy
	interval  time.Duration
	batchSize int
	// strandedAfter is how long a notification may sit in flight before the
	// sweep reclaims it. Zero disables the sweep.
	strandedAfter time.Duration
	log           *zap.Logger
}

type Options struct {
	Interval  time.Duration
	BatchSize int
	Policy    Policy

	// StrandedAfter defaults to 15 minutes when unset. Set it explicitly to
	// zero to disable the sweep — see Worker.SweepStranded for why the
	// default is far larger than the longest possible attempt.
	StrandedAfter time.Duration

	// StrandedReclaimed and RetriesExhausted are optional counters. Nil means
	// "do not record", so a test need not build a metrics registry to drive
	// the worker.
	StrandedReclaimed Counter
	RetriesExhausted  Counter
}

// Counter is one unlabelled counter. Satisfied by prometheus.Counter.
//
// Kept out of Metrics because these two have no labels, and because they are
// the numbers that should normally be ZERO rather than merely low: every
// stranded reclaim is a notice the platform accepted and then lost track of,
// and every exhaustion is a notice it gave up on after using its whole budget.
// A dashboard treats "should be zero" differently from "watch the ratio".
type Counter interface{ Inc() }

func NewWorker(store Store, deliverer Deliverer, metrics Metrics, recipient RecipientResolver, settled Settled, opts Options, log *zap.Logger) *Worker {
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 50
	}
	// Negative is read as "off", never as "sweep everything" — a duration
	// that went negative through arithmetic must not turn into a sweep that
	// reclaims live in-flight notifications.
	if opts.StrandedAfter < 0 {
		opts.StrandedAfter = 0
	}
	return &Worker{
		store:         store,
		deliverer:     deliverer,
		metrics:       metrics,
		stranded:      opts.StrandedReclaimed,
		exhausted:     opts.RetriesExhausted,
		recipient:     recipient,
		settled:       settled,
		policy:        opts.Policy.Normalize(),
		interval:      opts.Interval,
		batchSize:     opts.BatchSize,
		strandedAfter: opts.StrandedAfter,
		log:           log,
	}
}

// Start runs until ctx is cancelled. Intended for its own goroutine.
func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.log.Info("delivery retry worker started",
		zap.Duration("interval", w.interval),
		zap.Int("batch_size", w.batchSize),
		zap.Int("max_attempts", w.policy.MaxAttempts),
		zap.Duration("stranded_after", w.strandedAfter))
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
	// The sweep runs FIRST, so anything it reclaims is picked up by the due
	// pass in this same tick rather than waiting a further interval. It only
	// sets a schedule; the delivery itself always goes through the ordinary
	// path below, so there is one code path that actually sends.
	w.SweepStranded(ctx)

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
			// nothing scheduled, which SweepStranded reclaims — a sweep this
			// comment asserted before one existed, which is how five
			// notifications sat undelivered for six days.
			return attempted
		default:
		}
		if w.attempt(ctx, d) {
			attempted++
		}
	}
	return attempted
}

// SweepStranded puts abandoned in-flight notifications back on the retry
// schedule, and returns how many it reclaimed.
//
// THE GAP THIS CLOSES. PENDING with next_attempt_at NULL means "in flight
// right now". Nothing moves such a row on its own — FindDueRetries requires a
// schedule to be set — so a notification whose attempt never reported an
// outcome stays there permanently: never delivered, never failed, never
// retried, and displayed as PENDING, which reads as progress rather than as a
// notice that silently never went out. Measured on the dev database
// 2026-09-08: five of them from 2026-09-02, delivery_attempts = 0.
//
// RunOnce's own shutdown comment claimed "the sweep below" handled exactly
// this. It did not exist. That is the whole of the defect: the failure mode
// was understood and written down, and the remedy was never built.
//
// It only ever SCHEDULES. The delivery goes through the ordinary due path, so
// there is one place that sends and one place that decides what an outcome
// means. Reclaimed rows keep their attempt count, so a notification that had
// already burned attempts does not get a fresh budget by being stranded — the
// policy's ceiling still applies and the sweep cannot become an unbounded
// resend loop.
//
// The duplicate-send hazard, stated plainly: a stranded row may or may not
// have reached the provider before its attempt died, and nothing on the row
// can distinguish those. Rescheduling therefore risks a second copy of a
// notice; not rescheduling guarantees some governed notices are never sent at
// all. The second is the worse failure for this service, so the sweep
// reschedules — and the staleness threshold, which must exceed the longest
// possible attempt, is what keeps the risk to the genuine-crash case. Rows
// that recorded SENT are never touched.
//
// A zero threshold disables the sweep entirely rather than sweeping
// everything, because "reclaim every in-flight notification immediately" is
// the one setting that would reliably cause the duplicates above.
func (w *Worker) SweepStranded(ctx context.Context) int {
	if w.strandedAfter <= 0 {
		return 0
	}

	now := time.Now().UTC()
	staleBefore := now.Add(-w.strandedAfter)

	stranded, err := w.store.FindStrandedDeliveries(ctx, staleBefore, w.batchSize)
	if err != nil {
		w.log.Error("retry worker: failed to sweep for stranded deliveries", zap.Error(err))
		return 0
	}
	if len(stranded) == 0 {
		return 0
	}

	revived := 0
	for _, d := range stranded {
		select {
		case <-ctx.Done():
			return revived
		default:
		}

		// The notification's own tenant installed on the context, exactly as
		// attempt does and exactly as a request would. The store also takes
		// the tenant explicitly, so this is belt and braces — but the poll
		// above is the one cross-tenant read in the service, and every write
		// that follows it running under the tenant's own policy is the
		// property that makes that hatch safe to have.
		tctx := svcmiddleware.WithTenant(ctx, d.TenantID)

		// Due immediately: it has already waited longer than any backoff this
		// policy would impose, and the point is to stop it waiting.
		ok, err := w.store.ReviveStranded(tctx, d.NotificationID, d.TenantID, staleBefore, now)
		if err != nil {
			// Logged per row and the loop continues: one tenant's failure
			// must not strand the rest of the batch, which is the same
			// property that made this bug survive in the first place.
			w.log.Error("retry worker: could not revive stranded delivery",
				zap.String("notification_id", d.NotificationID), zap.Error(err))
			continue
		}
		if !ok {
			// Another replica got it, or it concluded between the find and
			// the write. Not an error — the row itself is the claim.
			continue
		}
		revived++
		if w.stranded != nil {
			w.stranded.Inc()
		}

		// WARN, not Info. Every row here is a notification the platform
		// accepted and then lost track of, so each one is a delivery that
		// would never have happened; that is worth an operator's attention
		// even though the sweep repairs it.
		w.log.Warn("retry worker: reclaimed a stranded delivery",
			zap.String("notification_id", d.NotificationID),
			zap.Duration("stranded_after", w.strandedAfter))
	}

	if revived > 0 {
		w.log.Warn("retry worker: stranded deliveries reclaimed",
			zap.Int("count", revived),
			zap.Int("found", len(stranded)))
	}
	return revived
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

	attemptStarted := time.Now()
	outcome := w.deliverer.Deliver(tctx, *n)
	w.observeAttempt(n.Channel, outcome.Delivered, attemptStarted)
	w.conclude(tctx, n, outcome)
	return true
}

// observeAttempt records one re-attempt against the provider.
//
// Called only where a provider was actually reached. conclude's other entry
// points — a missing resolver, a resolution failure, an address that could not
// be recorded — never touch a provider, and counting them here would inflate
// the attempt histogram with durations that measure identity-context-svc or
// the local database instead of delivery.
func (w *Worker) observeAttempt(channel string, delivered bool, started time.Time) {
	if w.metrics == nil {
		return
	}
	outcome := telemetry.OutcomeFailed
	if delivered {
		outcome = telemetry.OutcomeDelivered
	}
	w.metrics.ObserveAttempt(channel, outcome, telemetry.OriginRetry, time.Since(started).Seconds())
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
		// The record is brought up to date BEFORE the event is sealed, because
		// the event is built from it. Sealing first would announce a
		// notification.sent carrying status PENDING and no sent_at — describing
		// the row as it was before the transition the event exists to report.
		//
		// DeliveryAttempts is incremented here too, and only here in memory:
		// the column is incremented by CompleteDelivery itself, so the struct
		// would otherwise carry a count one behind the row it describes and the
		// event would understate how much work the delivery took.
		n.Status, n.SentAt, n.ProviderResponse = "SENT", &now, outcome.ProviderResponse
		n.FailureReason = ""
		n.DeliveryAttempts++

		ev, err := events.Sent(n.CorrelationID, *n)
		if err != nil {
			w.log.Error("retry worker: delivered but could not seal the event",
				zap.String("notification_id", n.NotificationID), zap.Error(err))
			return
		}
		// One transaction. The worker used to call CompleteDelivery and then
		// PublishSent, so a broker outage during a successful re-attempt sent
		// the notice, recorded it, and told no consumer — permanently, with
		// nothing but a log line to show for it.
		if err := w.store.CompleteDelivery(ctx, n.NotificationID, "SENT", "", outcome.ProviderResponse, &now, ev); err != nil {
			w.log.Error("retry worker: delivered but could not record it",
				zap.String("notification_id", n.NotificationID), zap.Error(err))
			return
		}
		w.log.Info("retry worker: delivery succeeded on re-attempt",
			zap.String("notification_id", n.NotificationID),
			zap.Int("attempt", n.DeliveryAttempts))
		if w.metrics != nil {
			w.metrics.ObserveConclusion(n.Channel, "SENT")
		}
		return
	}

	// attemptsMade counts the attempt just concluded: the column has not been
	// incremented yet — ScheduleRetry and CompleteDelivery both do that — so
	// the value the policy needs is the stored count plus this one.
	attemptsMade := n.DeliveryAttempts + 1

	// Recorded before the reason string is appended to, because the appended
	// text is how the register explains the difference and the metric should
	// not be parsed out of prose.
	exhausted := false

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
			if w.metrics != nil {
				w.metrics.ObserveRetryScheduled(n.Channel)
			}
			// No notification.failed event, and nothing enqueued. The delivery
			// has not concluded, and emitting a failure that a later attempt
			// reverses would have consumers reacting to an outcome that did not
			// happen. This is why ScheduleRetry takes no event while
			// CompleteDelivery requires one.
			return
		}
		// Exhausted. The reason records that, so the register does not read as
		// though a mailbox was rejected when in fact the platform gave up.
		exhausted = true
		outcome.Reason = outcome.Reason + " (no further attempts: exhausted after " +
			itoa(attemptsMade) + " of " + itoa(w.policy.MaxAttempts) + ")"
	}

	n.Status, n.SentAt, n.FailureReason = "FAILED", &now, outcome.Reason
	n.DeliveryAttempts = attemptsMade

	ev, err := events.Failed(n.CorrelationID, *n, outcome.Reason)
	if err != nil {
		w.log.Error("retry worker: could not seal the terminal-failure event",
			zap.String("notification_id", n.NotificationID), zap.Error(err))
		return
	}
	if err := w.store.CompleteDelivery(ctx, n.NotificationID, "FAILED", outcome.Reason, "", &now, ev); err != nil {
		w.log.Error("retry worker: could not record terminal failure",
			zap.String("notification_id", n.NotificationID), zap.Error(err))
		return
	}
	w.log.Warn("retry worker: delivery failed terminally",
		zap.String("notification_id", n.NotificationID),
		zap.Int("attempts", attemptsMade),
		zap.String("reason", outcome.Reason))
	if w.metrics != nil {
		w.metrics.ObserveConclusion(n.Channel, "FAILED")
	}
	if exhausted && w.exhausted != nil {
		w.exhausted.Inc()
	}
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
