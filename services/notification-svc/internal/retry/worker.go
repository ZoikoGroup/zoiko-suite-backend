package retry

import (
	"context"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/ncd"
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
	// The expiry pair (ZS-SVC-Y-001 6.1, NCD-015): a communication past expires_at is never
	// submitted. FindExpiredQueued lists queued rows past it; ExpireNotification concludes one.
	FindExpiredQueued(ctx context.Context, now time.Time, limit int) ([]domain.DueRetry, error)
	ExpireNotification(ctx context.Context, id, tenantID string, now time.Time) (bool, error)
	// The stranded-delivery pair. A notification left in flight — PENDING
	// with nothing scheduled — is invisible to FindDueRetries and nothing
	// else in the service would ever touch it again.
	FindStrandedDeliveries(ctx context.Context, staleBefore time.Time, limit int) ([]domain.DueRetry, error)
	ReviveStranded(ctx context.Context, id, tenantID string, staleBefore, nextAttemptAt time.Time) (bool, error)
	// MarkStrandedUnknown handles a stranded row that WAS handed to a provider
	// (migration 000013): PENDING_UNKNOWN, never a blind re-send (§6.2).
	MarkStrandedUnknown(ctx context.Context, id, tenantID string, staleBefore, at time.Time) (bool, error)
	// BeginSubmission marks a notification as being handed to a provider,
	// committed before the call, so a lost outcome is recognisable.
	BeginSubmission(ctx context.Context, id, tenantID string, at time.Time) error
	GetNotification(ctx context.Context, id string) (*domain.Notification, error)
	// CompleteDelivery and MarkOutcomeUnknown enqueue their event in the same
	// transaction as the transition (migration 000010); the worker never
	// publishes.
	CompleteDelivery(ctx context.Context, id, newStatus, failureReason, providerResponse string, sentAt *time.Time, correlationID string, meta domain.AttemptMeta) error
	ScheduleRetry(ctx context.Context, id, tenantID, failureReason string, attemptedAt, nextAttemptAt time.Time, meta domain.AttemptMeta) error
	SetRecipientAddress(ctx context.Context, id, tenantID, address, source string) error
	// MarkOutcomeUnknown backs an ambiguous re-attempt — see conclude's own
	// doc comment.
	MarkOutcomeUnknown(ctx context.Context, id, tenantID, reason string, attemptedAt time.Time, correlationID string, meta domain.AttemptMeta) error
}

type Deliverer interface {
	Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome
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
	metrics   *telemetry.Domain
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
}

// metrics may be nil (tests); every observation is nil-safe.
func NewWorker(store Store, deliverer Deliverer, metrics *telemetry.Domain, recipient RecipientResolver, settled Settled, opts Options, log *zap.Logger) *Worker {
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
	w.ExpireOverdue(ctx)

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
// It never sends. A reclaimed row goes through the ordinary due path, so there
// is one place that sends and one place that decides what an outcome means.
// Reclaimed rows keep their attempt count, so a notification that had already
// burned attempts does not get a fresh budget by being stranded — the policy's
// ceiling still applies and the sweep cannot become an unbounded resend loop.
//
// The duplicate-send hazard, and how it is now closed. This used to say that a
// stranded row "may or may not have reached the provider before its attempt
// died, and nothing on the row can distinguish those", so the sweep
// rescheduled every one and accepted the occasional second copy. Migration
// 000013's submitting_since marker distinguishes them: it is committed before
// the provider is called and cleared when the outcome is recorded. So
//
//   - a stranded row WITHOUT the marker never reached a provider, and is
//     rescheduled — that can duplicate nothing;
//   - a stranded row WITH the marker was handed to a provider and lost its
//     outcome, and becomes PENDING_UNKNOWN (MarkStrandedUnknown) for a person
//     to resolve — ZS-SVC-Y-001 §6.2: "UNKNOWN never authorizes an immediate
//     blind second material send."
//
// The staleness threshold must still exceed the longest possible attempt, so a
// row genuinely mid-SMTP on another replica is never taken. Rows that recorded
// a conclusion are never touched.
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

		// A row that was handed to a provider (submitting_since set,
		// migration 000013) may already have been delivered. Re-sending it
		// is the blind second material send §6.2 forbids, so it becomes
		// PENDING_UNKNOWN for a person to resolve against the provider's
		// records instead.
		if d.Submitted {
			marked, err := w.store.MarkStrandedUnknown(tctx, d.NotificationID, d.TenantID, staleBefore, now)
			if err != nil {
				w.log.Error("retry worker: could not mark a lost submission unknown",
					zap.String("notification_id", d.NotificationID), zap.Error(err))
				continue
			}
			if marked {
				w.metrics.ObserveConclusion("stranded", domain.StatusPendingUnknown)
				w.log.Warn("retry worker: a stranded delivery had already been submitted; marked PENDING_UNKNOWN, not re-sent",
					zap.String("notification_id", d.NotificationID))
			}
			continue
		}

		// Never submitted, so reviving it cannot duplicate anything. Due
		// immediately: it has already waited longer than any backoff this
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
		w.metrics.ObserveStrandedReclaimed()

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
				time.Now().UTC(), next, domain.AttemptMeta{Origin: domain.AttemptOriginRetry}); reErr != nil {
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

	// The submit gate for expiry: whatever the schedule says, a communication past its
	// expires_at is never handed to a provider. A stale reminder is worse than none.
	if n.ExpiresAt != nil && !time.Now().UTC().Before(*n.ExpiresAt) {
		if _, err := w.store.ExpireNotification(tctx, n.NotificationID, n.TenantID, time.Now().UTC()); err != nil {
			w.log.Error("retry worker: could not expire an overdue communication",
				zap.String("notification_id", n.NotificationID), zap.Error(err))
		}
		return true
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

	// Marked, and committed, before the provider is called (migration 000013),
	// so that if this process dies mid-attempt the sweep knows the provider may
	// have the message and does not blindly send it again. If the mark cannot
	// be written the provider is NOT called: the row is put back on the
	// schedule, which never duplicates anything.
	if n.Channel != domain.ChannelInApp {
		if err := w.store.BeginSubmission(tctx, n.NotificationID, n.TenantID, time.Now().UTC()); err != nil {
			w.log.Error("retry worker: could not mark the submission; not calling the provider",
				zap.String("notification_id", n.NotificationID), zap.Error(err))
			if next, ok := w.policy.NextAttempt(time.Now().UTC(), n.DeliveryAttempts+1); ok {
				if reErr := w.store.ScheduleRetry(tctx, n.NotificationID, n.TenantID,
					"could not record the submission before attempting it: "+err.Error(),
					time.Now().UTC(), next, domain.AttemptMeta{Origin: domain.AttemptOriginRetry}); reErr != nil {
					w.log.Error("retry worker: could not reschedule after a failed submission mark",
						zap.String("notification_id", n.NotificationID), zap.Error(reErr))
				}
			}
			return true
		}
	}

	started := time.Now()
	outcome := w.deliverer.Deliver(tctx, *n)
	w.metrics.ObserveAttempt(n.Channel, attemptOutcome(outcome), telemetry.OriginRetry, time.Since(started).Seconds())
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
			Reason:    ncd.Format(ncd.RecipientUnresolved) + ": recipient resolution failed: " + err.Error(),
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
		if err := w.store.CompleteDelivery(ctx, n.NotificationID, "SENT", "", outcome.ProviderResponse, &now, n.CorrelationID, w.meta(n, outcome)); err != nil {
			w.log.Error("retry worker: delivered but could not record it",
				zap.String("notification_id", n.NotificationID), zap.Error(err))
			return
		}
		n.Status, n.SentAt, n.ProviderResponse = "SENT", &now, outcome.ProviderResponse
		n.FailureReason = ""
		w.log.Info("retry worker: delivery succeeded on re-attempt",
			zap.String("notification_id", n.NotificationID),
			zap.Int("attempt", n.DeliveryAttempts+1))
		w.metrics.ObserveConclusion(n.Channel, domain.StatusSent)
		return
	}

	// attemptsMade counts the attempt just concluded: the column has not been
	// incremented yet — ScheduleRetry and CompleteDelivery both do that — so
	// the value the policy needs is the stored count plus this one.
	attemptsMade := n.DeliveryAttempts + 1

	// An ambiguous outcome on a retried attempt gets the same treatment as
	// one on the first attempt (handler.SendNotification): it is checked
	// before Retryable so it is never silently retried, which would risk
	// a duplicate if the message did go out. See
	// domain.DeliveryOutcome.Unknown's own doc comment.
	if outcome.Unknown {
		if err := w.store.MarkOutcomeUnknown(ctx, n.NotificationID, n.TenantID, outcome.Reason, now, n.CorrelationID, w.meta(n, outcome)); err != nil {
			w.log.Error("retry worker: could not record ambiguous delivery outcome",
				zap.String("notification_id", n.NotificationID), zap.Error(err))
			return
		}
		n.Status, n.SentAt, n.UnknownAt, n.FailureReason = "PENDING_UNKNOWN", &now, &now, outcome.Reason
		w.log.Warn("retry worker: delivery outcome ambiguous on re-attempt",
			zap.String("notification_id", n.NotificationID),
			zap.Int("attempt", attemptsMade),
			zap.String("reason", outcome.Reason))
		w.metrics.ObserveConclusion(n.Channel, domain.StatusPendingUnknown)
		return
	}

	if outcome.Retryable {
		if next, ok := w.policy.NextAttemptFor(now, attemptsMade, outcome.DeferUntil); ok {
			if err := w.store.ScheduleRetry(ctx, n.NotificationID, n.TenantID, outcome.Reason, now, next, w.meta(n, outcome)); err != nil {
				w.log.Error("retry worker: could not reschedule",
					zap.String("notification_id", n.NotificationID), zap.Error(err))
			}
			w.metrics.ObserveRetryScheduled(n.Channel)
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
		w.metrics.ObserveRetryExhausted()
		// Exhausted. The reason records that, so the register does not read as
		// though a mailbox was rejected when in fact the platform gave up.
		outcome.Reason = outcome.Reason + " (no further attempts: exhausted after " +
			itoa(attemptsMade) + " of " + itoa(w.policy.MaxAttempts) + ")"
	}

	if err := w.store.CompleteDelivery(ctx, n.NotificationID, "FAILED", outcome.Reason, "", &now, n.CorrelationID, w.meta(n, outcome)); err != nil {
		w.log.Error("retry worker: could not record terminal failure",
			zap.String("notification_id", n.NotificationID), zap.Error(err))
		return
	}
	n.Status, n.SentAt, n.FailureReason = "FAILED", &now, outcome.Reason
	w.log.Warn("retry worker: delivery failed terminally",
		zap.String("notification_id", n.NotificationID),
		zap.Int("attempts", attemptsMade),
		zap.String("reason", outcome.Reason))
	w.metrics.ObserveConclusion(n.Channel, domain.StatusFailed)
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

// attemptOutcome maps one delivery outcome onto the attempt metric's outcome
// label. Shared with the handler's first attempt, so the two origins sum.
func attemptOutcome(o domain.DeliveryOutcome) string {
	switch {
	case o.Delivered:
		return telemetry.OutcomeDelivered
	case o.Unknown:
		return telemetry.OutcomeUnknown
	case o.Retryable:
		return telemetry.OutcomeRetrying
	default:
		return telemetry.OutcomeFailed
	}
}

// AttemptOutcome is attemptOutcome for the handler's first attempt.
func AttemptOutcome(o domain.DeliveryOutcome) string { return attemptOutcome(o) }

// retryMeta describes a worker attempt for its durable attempt row.
func retryMeta(o domain.DeliveryOutcome) domain.AttemptMeta {
	return domain.AttemptMeta{Origin: domain.AttemptOriginRetry, ProviderName: o.ProviderName, Retryable: o.Retryable,
		PrivacyDecisionID: o.PrivacyDecisionID, PrivacyResult: o.PrivacyResult, BlockCode: o.BlockCode}
}

// ExpireOverdue concludes queued communications that passed their expires_at without being
// submitted, and returns how many it expired.
func (w *Worker) ExpireOverdue(ctx context.Context) int {
	overdue, err := w.store.FindExpiredQueued(ctx, time.Now().UTC(), w.batchSize)
	if err != nil {
		w.log.Error("retry worker: failed to poll for expired communications", zap.Error(err))
		return 0
	}
	expired := 0
	for _, d := range overdue {
		tctx := svcmiddleware.WithTenant(ctx, d.TenantID)
		ok, err := w.store.ExpireNotification(tctx, d.NotificationID, d.TenantID, time.Now().UTC())
		if err != nil {
			w.log.Error("retry worker: could not expire a communication",
				zap.String("notification_id", d.NotificationID), zap.Error(err))
			continue
		}
		if ok {
			expired++
			w.metrics.ObserveConclusion("EMAIL", domain.StatusExpired)
			w.log.Warn("retry worker: communication expired before it was submitted",
				zap.String("notification_id", d.NotificationID))
		}
	}
	return expired
}

// meta describes a worker attempt for its durable attempt row. The first attempt of a send
// that waited for not_before is labelled scheduled, not retry: nothing had been tried before.
func (w *Worker) meta(n *domain.Notification, o domain.DeliveryOutcome) domain.AttemptMeta {
	m := retryMeta(o)
	if n.DeliveryAttempts == 0 {
		m.Origin = domain.AttemptOriginScheduled
	}
	return m
}
