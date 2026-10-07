package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/retry"
	"zoiko.io/notification-svc/internal/store"
)

// queuedNotification creates a communication scheduled for notBefore, expiring at expiresAt
// (either may be nil), exactly as the handler would for a queued send.
func queuedNotification(t *testing.T, s *store.PgStore, tenant, corr string, notBefore, expiresAt *time.Time) *domain.Notification {
	t.Helper()
	n := newNotification(tenant, "entity-1", "recipient-1", corr)
	n.NotBefore, n.ExpiresAt = notBefore, expiresAt
	if notBefore != nil {
		n.NextAttemptAt = notBefore
	}
	created, err := s.CreateNotification(tenantCtx(tenant), n)
	require.NoError(t, err)
	require.True(t, created)
	return n
}

func tp(d time.Duration) *time.Time { v := time.Now().UTC().Add(d); return &v }

func TestSchedule_AQueuedSendWaitsAndCarriesItsJob(t *testing.T) {
	s := store.New(openTestPool(t))
	n := queuedNotification(t, s, "tenant-sched", "corr-sched-1", tp(2*time.Hour), tp(48*time.Hour))

	got, err := s.GetNotification(tenantCtx("tenant-sched"), n.NotificationID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPending, got.Status)
	assert.NotEmpty(t, got.JobID, "every communication has a stable job identity")
	require.NotNil(t, got.NotBefore)
	require.NotNil(t, got.ExpiresAt)
	require.NotNil(t, got.NextAttemptAt)
	assert.WithinDuration(t, *got.NotBefore, *got.NextAttemptAt, time.Second, "it is due exactly at not_before")
	assert.Zero(t, got.DeliveryAttempts, "nothing was attempted")

	due, err := s.FindDueRetries(context.Background(), time.Now().UTC(), 50)
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, n.NotificationID, d.NotificationID, "not due before not_before")
	}
	due, err = s.FindDueRetries(context.Background(), got.NotBefore.Add(time.Second), 50)
	require.NoError(t, err)
	var found bool
	for _, d := range due {
		found = found || d.NotificationID == n.NotificationID
	}
	assert.True(t, found, "due once not_before has passed")

	// A communication that asked for no timing still has a job.
	plain := seedNotification(t, s, "tenant-sched", "corr-sched-plain")
	pg, err := s.GetNotification(tenantCtx("tenant-sched"), plain.NotificationID)
	require.NoError(t, err)
	assert.NotEmpty(t, pg.JobID)
	assert.NotEqual(t, got.JobID, pg.JobID)
}

func TestSchedule_EveryAttemptAndItsEventCarryTheJob(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	n := seedNotification(t, s, "tenant-sched-job", "corr-sched-job")
	require.NoError(t, s.ScheduleRetry(tenantCtx("tenant-sched-job"), n.NotificationID, "tenant-sched-job", "421 later",
		time.Now().UTC(), time.Now().Add(time.Minute).UTC(), domain.AttemptMeta{Origin: domain.AttemptOriginRequest}))
	done := time.Now().UTC()
	require.NoError(t, s.CompleteDelivery(tenantCtx("tenant-sched-job"), n.NotificationID, "SENT", "", "250 ok", &done, "c", domain.AttemptMeta{Origin: domain.AttemptOriginRetry}))

	got, err := s.GetNotification(tenantCtx("tenant-sched-job"), n.NotificationID)
	require.NoError(t, err)
	attempts, err := s.ListAttempts(tenantCtx("tenant-sched-job"), n.NotificationID)
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	for _, a := range attempts {
		assert.Equal(t, got.JobID, a.JobID, "every attempt is under the communication's job")
	}
	assert.Equal(t, got.JobID, outboxFor(t, pool, n.NotificationID)["delivery.attempt.created"]["job_id"], "and the canonical event says so")
}

func TestSchedule_OnlyAQueuedCommunicationCanBeCancelled(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-cancel"
	ctx := tenantCtx(tenant)
	n := queuedNotification(t, s, tenant, "corr-cancel-1", tp(time.Hour), nil)

	_, err := s.CancelNotification(ctx, n.NotificationID, tenant, "ops-1", "", time.Now())
	assert.ErrorIs(t, err, domain.ErrCancelReasonRequired)
	_, err = s.CancelNotification(ctx, uuid.NewString(), tenant, "ops-1", "no longer needed", time.Now())
	assert.ErrorIs(t, err, domain.ErrNotificationNotFound)

	got, err := s.CancelNotification(ctx, n.NotificationID, tenant, "ops-1", "the workflow was cancelled", time.Now())
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCancelled, got.Status)
	assert.Equal(t, "ops-1", got.CancelledBy)
	assert.Equal(t, "the workflow was cancelled", got.CancelReason)
	require.NotNil(t, got.CancelledAt)
	assert.Nil(t, got.NextAttemptAt, "a cancelled communication is no longer scheduled")

	evs := outboxFor(t, pool, n.NotificationID)
	require.Contains(t, evs, "communication.cancelled")
	assert.Equal(t, "ops-1", evs["communication.cancelled"]["cancelled_by"])
	assert.NotContains(t, evs, "notification.failed", "a withdrawal is not a failure")

	// Gone for good: not cancellable twice, not resendable, never due.
	_, err = s.CancelNotification(ctx, n.NotificationID, tenant, "ops-2", "again", time.Now())
	assert.ErrorIs(t, err, domain.ErrCancelNotAllowed)
	_, err = s.BeginResend(ctx, n.NotificationID, tenant, "ops-2", "resend it", time.Now())
	assert.Error(t, err, "a cancelled communication cannot be resent")
	due, err := s.FindDueRetries(context.Background(), time.Now().Add(24*time.Hour), 50)
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, n.NotificationID, d.NotificationID)
	}
}

func TestSchedule_ACancellationNeverDescribesAMessageThatWasSent(t *testing.T) {
	s := store.New(openTestPool(t))
	tenant := "tenant-cancel-race"
	ctx := tenantCtx(tenant)

	claimed := queuedNotification(t, s, tenant, "corr-cancel-claimed", tp(-time.Minute), nil)
	ok, err := s.ClaimRetry(ctx, claimed.NotificationID, tenant)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = s.CancelNotification(ctx, claimed.NotificationID, tenant, "ops-1", "too late", time.Now())
	assert.ErrorIs(t, err, domain.ErrCancelNotAllowed, "a worker already owns it")

	submitting := queuedNotification(t, s, tenant, "corr-cancel-submitting", tp(-time.Minute), nil)
	ok, err = s.ClaimRetry(ctx, submitting.NotificationID, tenant)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, s.BeginSubmission(ctx, submitting.NotificationID, tenant, time.Now()))
	_, err = s.CancelNotification(ctx, submitting.NotificationID, tenant, "ops-1", "too late", time.Now())
	assert.ErrorIs(t, err, domain.ErrCancelNotAllowed, "the provider may already have it")

	sent := seedNotification(t, s, tenant, "corr-cancel-sent")
	done := time.Now().UTC()
	require.NoError(t, s.CompleteDelivery(ctx, sent.NotificationID, "SENT", "", "250 ok", &done, "c", domain.AttemptMeta{Origin: domain.AttemptOriginRequest}))
	_, err = s.CancelNotification(ctx, sent.NotificationID, tenant, "ops-1", "too late", time.Now())
	assert.ErrorIs(t, err, domain.ErrCancelNotAllowed, "it concluded")

	other, err := s.CancelNotification(tenantCtx("someone-else"), claimed.NotificationID, "someone-else", "ops", "x", time.Now())
	assert.Nil(t, other)
	assert.ErrorIs(t, err, domain.ErrNotificationNotFound, "another tenant cannot see it")
}

func TestSchedule_ACommunicationPastItsExpiryIsNeverSent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-expire"
	ctx := tenantCtx(tenant)
	// A queued send whose expiry falls before the scheduled time is refused by the CHECK in the
	// service; here the retry schedule outlived the expiry (a retry backed off past it).
	n := queuedNotification(t, s, tenant, "corr-expire-1", nil, tp(time.Hour))
	require.NoError(t, s.ScheduleRetry(ctx, n.NotificationID, tenant, "421 later", time.Now().UTC(), time.Now().Add(3*time.Hour).UTC(),
		domain.AttemptMeta{Origin: domain.AttemptOriginRequest}))

	ok, err := s.ExpireNotification(ctx, n.NotificationID, tenant, time.Now())
	require.NoError(t, err)
	assert.False(t, ok, "not yet due to expire")

	later := time.Now().Add(2 * time.Hour)
	listed, err := s.FindExpiredQueued(context.Background(), later, 50)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, n.NotificationID, listed[0].NotificationID)

	ok, err = s.ExpireNotification(ctx, n.NotificationID, tenant, later)
	require.NoError(t, err)
	assert.True(t, ok)
	got, err := s.GetNotification(ctx, n.NotificationID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusExpired, got.Status)
	assert.Contains(t, got.FailureReason, "NCD-015 DELIVERY_EXPIRED")
	assert.Nil(t, got.NextAttemptAt)

	evs := outboxFor(t, pool, n.NotificationID)
	require.Contains(t, evs, "notification.failed", "an escalation waiting on a failure is told")
	require.Contains(t, evs, "communication.blocked")
	assert.Equal(t, "NCD-015", evs["communication.blocked"]["reason_code"])

	// Idempotent, never resent, never due again.
	ok, err = s.ExpireNotification(ctx, n.NotificationID, tenant, later.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, ok)
	_, err = s.BeginResend(ctx, n.NotificationID, tenant, "ops", "send it anyway", time.Now())
	assert.Error(t, err, "an expired communication cannot be resent")
	listed, err = s.FindExpiredQueued(context.Background(), later.Add(time.Hour), 50)
	require.NoError(t, err)
	assert.Empty(t, listed)
}

func TestSchedule_ExpiryLeavesWhatIsInFlightOrConcludedAlone(t *testing.T) {
	s := store.New(openTestPool(t))
	tenant := "tenant-expire-guard"
	ctx := tenantCtx(tenant)

	inFlight := queuedNotification(t, s, tenant, "corr-ex-flight", nil, tp(time.Hour))
	require.NoError(t, s.BeginSubmission(ctx, inFlight.NotificationID, tenant, time.Now()))
	ok, err := s.ExpireNotification(ctx, inFlight.NotificationID, tenant, time.Now().Add(2*time.Hour))
	require.NoError(t, err)
	assert.False(t, ok, "the provider may already have it: that is UNKNOWN territory, not expiry")

	noExpiry := seedNotification(t, s, tenant, "corr-ex-none")
	ok, err = s.ExpireNotification(ctx, noExpiry.NotificationID, tenant, time.Now().Add(24*time.Hour))
	require.NoError(t, err)
	assert.False(t, ok, "no expires_at, no expiry")
}

func TestSchedule_TheDatabaseHoldsTheTimingRules(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-sched-db"
	n := queuedNotification(t, s, tenant, "corr-sched-db", tp(time.Hour), tp(5*time.Hour))
	exec := func(q string, args ...any) error {
		conn, err := pool.Acquire(context.Background())
		require.NoError(t, err)
		defer conn.Release()
		_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, false)`, tenant)
		require.NoError(t, err)
		_, err = conn.Exec(context.Background(), q, args...)
		return err
	}
	err := exec(`UPDATE notifications SET expires_at = not_before WHERE notification_id = $1`, n.NotificationID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notifications_timing_order")
	err = exec(`UPDATE notifications SET status = 'CANCELLED', next_attempt_at = NULL, sent_at = now() WHERE notification_id = $1`, n.NotificationID)
	require.Error(t, err, "a cancellation must say who and why")
	assert.Contains(t, err.Error(), "notifications_cancel_has_evidence")
	err = exec(`UPDATE notifications SET status = 'EXPIRED', next_attempt_at = NULL, sent_at = now() WHERE notification_id = $1`, n.NotificationID)
	require.Error(t, err, "an expiry must say why")
	assert.Contains(t, err.Error(), "notifications_expiry_has_reason")
}

// A real worker, a real database: a scheduled send is attempted when due, labelled scheduled;
// an expired one is never handed to the provider.
type schedProvider struct{ sent []string }

func (p *schedProvider) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	p.sent = append(p.sent, n.NotificationID)
	return domain.DeliveryOutcome{Delivered: true, ProviderName: "smtp", ProviderResponse: "smtp mx:25 accepted; message-id=<sched@example.com>"}
}

type schedResolver struct{}

func (schedResolver) ResolveEmail(context.Context, string, string, string) (string, error) {
	return "recipient@example.com", nil
}

func TestSchedule_AWorkerSendsAScheduledMessageWhenDueAndNeverAnExpiredOne(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-sched-worker"
	ctx := tenantCtx(tenant)

	due := queuedNotification(t, s, tenant, "corr-w-due", tp(-time.Minute), tp(24*time.Hour))
	notYet := queuedNotification(t, s, tenant, "corr-w-notyet", tp(time.Hour), tp(24*time.Hour))
	expired := queuedNotification(t, s, tenant, "corr-w-expired", nil, tp(time.Hour))
	require.NoError(t, s.ScheduleRetry(ctx, expired.NotificationID, tenant, "421 later", time.Now().UTC(), time.Now().Add(-time.Minute).UTC(),
		domain.AttemptMeta{Origin: domain.AttemptOriginRequest}))
	// Make the expiry already past (bypassing the service's own validation, as time passing would).
	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, false)`, tenant)
	require.NoError(t, err)
	_, err = conn.Exec(context.Background(), `UPDATE notifications SET expires_at = now() - interval '1 minute' WHERE notification_id = $1`, expired.NotificationID)
	require.NoError(t, err)
	conn.Release()

	prov := &schedProvider{}
	w := retry.NewWorker(s, prov, nil, schedResolver{}, func(error) bool { return false }, retry.Options{StrandedAfter: 0}, zap.NewNop())
	w.RunOnce(context.Background())

	assert.Equal(t, []string{due.NotificationID}, prov.sent, "only the due, unexpired send went to the provider")

	sent, err := s.GetNotification(ctx, due.NotificationID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSent, sent.Status)
	assert.Equal(t, "recipient@example.com", sent.RecipientAddress, "resolved when sent, not when queued")
	attempts, err := s.ListAttempts(ctx, due.NotificationID)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, domain.AttemptOriginScheduled, attempts[0].Origin, "its first attempt is labelled scheduled")
	assert.Equal(t, sent.JobID, attempts[0].JobID)

	waiting, err := s.GetNotification(ctx, notYet.NotificationID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusPending, waiting.Status)
	assert.Zero(t, waiting.DeliveryAttempts)

	gone, err := s.GetNotification(ctx, expired.NotificationID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusExpired, gone.Status)
}

func TestMigration000028_DownThenUp(t *testing.T) {
	pool := openTestPool(t)
	for _, f := range []string{"000028_scheduling_cancel_expiry.down.sql", "000028_scheduling_cancel_expiry.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(b))
		require.NoError(t, err, f)
	}
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'notifications' AND column_name IN ('job_id','not_before','expires_at','cancelled_by','cancelled_at','cancel_reason')`).Scan(&n))
	assert.Equal(t, 6, n)
}
