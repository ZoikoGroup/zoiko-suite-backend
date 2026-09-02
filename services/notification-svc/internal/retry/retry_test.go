package retry_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"
)

// ── policy ──────────────────────────────────────────────────────────────────

func TestPolicyStopsAtMaxAttempts(t *testing.T) {
	p := retry.Policy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: time.Minute}
	now := time.Now()

	for attempts := 1; attempts < 3; attempts++ {
		if _, ok := p.NextAttempt(now, attempts); !ok {
			t.Fatalf("attempt %d: expected another attempt to be scheduled", attempts)
		}
	}
	if _, ok := p.NextAttempt(now, 3); ok {
		t.Fatal("attempts made == MaxAttempts must not schedule a fourth")
	}
	if _, ok := p.NextAttempt(now, 9); ok {
		t.Fatal("attempts made beyond MaxAttempts must not schedule")
	}
}

func TestPolicyBacksOffAndIsCapped(t *testing.T) {
	p := retry.Policy{MaxAttempts: 10, BaseDelay: time.Minute, MaxDelay: 4 * time.Minute}
	now := time.Now()

	// Growth is asserted on the floor of each wait, not on the sampled value.
	// Once the backoff reaches MaxDelay every attempt draws from the same
	// [cap, cap+20%] band, so consecutive samples are genuinely unordered —
	// comparing them directly is a flaky test, not a real invariant.
	for attempts := 1; attempts <= 6; attempts++ {
		at, ok := p.NextAttempt(now, attempts)
		if !ok {
			t.Fatalf("attempt %d: expected a schedule", attempts)
		}
		delay := at.Sub(now)

		// Uncapped floor for this attempt: base * 2^(n-1), capped.
		floor := time.Duration(float64(p.BaseDelay) * math.Pow(2, float64(attempts-1)))
		if floor > p.MaxDelay {
			floor = p.MaxDelay
		}
		if delay < floor {
			t.Fatalf("attempt %d: delay %s is below the %s floor", attempts, delay, floor)
		}
		if ceil := p.MaxDelay + p.MaxDelay/5; delay > ceil {
			t.Fatalf("attempt %d: delay %s exceeded cap+jitter %s", attempts, delay, ceil)
		}
	}

	// And the growth itself, on the floors alone: attempt 1 must wait less
	// than attempt 3, whatever the jitter does within each band.
	first, _ := p.NextAttempt(now, 1)
	third, _ := p.NextAttempt(now, 3)
	if first.Sub(now) >= third.Sub(now) {
		t.Fatalf("no backoff growth: attempt 1 waited %s, attempt 3 waited %s",
			first.Sub(now), third.Sub(now))
	}
}

// A MaxAttempts of 1 is how "retry disabled" is expressed. It must schedule
// nothing at all rather than falling back to the default, which is what a zero
// value does — the distinction main.go depends on when NOTIFICATION_RETRY_
// ENABLED is false.
func TestPolicyMaxAttemptsOneDisablesRetry(t *testing.T) {
	p := retry.Policy{MaxAttempts: 1, BaseDelay: time.Second, MaxDelay: time.Minute}
	if _, ok := p.NextAttempt(time.Now(), 1); ok {
		t.Fatal("MaxAttempts 1 must not schedule a second attempt")
	}
}

func TestPolicyNormalizeRejectsNonsense(t *testing.T) {
	p := retry.Policy{MaxAttempts: -3, BaseDelay: -time.Second, MaxDelay: -time.Hour}.Normalize()
	if p.MaxAttempts != retry.DefaultPolicy.MaxAttempts {
		t.Fatalf("MaxAttempts = %d, want the default %d", p.MaxAttempts, retry.DefaultPolicy.MaxAttempts)
	}
	if p.BaseDelay <= 0 {
		t.Fatalf("BaseDelay = %s, want a positive default", p.BaseDelay)
	}
	if p.MaxDelay < p.BaseDelay {
		t.Fatalf("MaxDelay %s is below BaseDelay %s", p.MaxDelay, p.BaseDelay)
	}
}

// ── worker stubs ────────────────────────────────────────────────────────────

type stubStore struct {
	due       []domain.DueRetry
	byID      map[string]*domain.Notification
	claimed   map[string]bool
	completed []string // "id:STATUS"
	scheduled []string // "id:reason"
	addresses map[string]string

	claimFails  bool
	tenantsSeen []string
}

func newStubStore() *stubStore {
	return &stubStore{
		byID:      map[string]*domain.Notification{},
		claimed:   map[string]bool{},
		addresses: map[string]string{},
	}
}

func (s *stubStore) FindDueRetries(_ context.Context, _ time.Time, _ int) ([]domain.DueRetry, error) {
	return s.due, nil
}

func (s *stubStore) ClaimRetry(ctx context.Context, id, tenantID string) (bool, error) {
	// Every call after the cross-tenant poll must arrive with the
	// notification's own tenant installed on the context — that is the whole
	// basis for the claim that the worker's writes stay tenant-scoped.
	s.tenantsSeen = append(s.tenantsSeen, svcmiddleware.TenantFromContext(ctx))
	if s.claimFails {
		return false, errors.New("claim exploded")
	}
	if s.claimed[id] {
		return false, nil
	}
	s.claimed[id] = true
	return true, nil
}

func (s *stubStore) GetNotification(_ context.Context, id string) (*domain.Notification, error) {
	n, ok := s.byID[id]
	if !ok {
		return nil, domain.ErrNotificationNotFound
	}
	return n, nil
}

func (s *stubStore) CompleteDelivery(_ context.Context, id, newStatus, _, _ string, _ *time.Time) error {
	s.completed = append(s.completed, id+":"+newStatus)
	if n, ok := s.byID[id]; ok {
		n.Status = newStatus
	}
	return nil
}

func (s *stubStore) ScheduleRetry(_ context.Context, id, _, failureReason string, _, next time.Time) error {
	s.scheduled = append(s.scheduled, id+":"+failureReason)
	if n, ok := s.byID[id]; ok {
		n.DeliveryAttempts++
		n.NextAttemptAt = &next
	}
	return nil
}

func (s *stubStore) SetRecipientAddress(_ context.Context, id, _, address, _ string) error {
	s.addresses[id] = address
	if n, ok := s.byID[id]; ok {
		n.RecipientAddress = address
	}
	return nil
}

type stubDeliverer struct {
	outcome domain.DeliveryOutcome
	calls   int
	sawAddr []string
}

func (d *stubDeliverer) Deliver(_ context.Context, n domain.Notification) domain.DeliveryOutcome {
	d.calls++
	d.sawAddr = append(d.sawAddr, n.RecipientAddress)
	return d.outcome
}

type stubPublisher struct{ sent, failed int }

func (p *stubPublisher) PublishSent(context.Context, string, domain.Notification) { p.sent++ }
func (p *stubPublisher) PublishFailed(context.Context, string, domain.Notification, string) {
	p.failed++
}

type stubResolver struct {
	email string
	err   error
}

func (r *stubResolver) ResolveEmail(context.Context, string, string, string) (string, error) {
	return r.email, r.err
}

func newWorker(s *stubStore, d *stubDeliverer, p *stubPublisher, res retry.RecipientResolver, pol retry.Policy) *retry.Worker {
	settled := func(err error) bool {
		return errors.Is(err, domain.ErrPrincipalNotFound) || errors.Is(err, domain.ErrPrincipalHasNoAddress)
	}
	return retry.NewWorker(s, d, p, res, settled, retry.Options{Policy: pol}, zap.NewNop())
}

func seed(s *stubStore, id, tenant string, attempts int) {
	s.due = append(s.due, domain.DueRetry{NotificationID: id, TenantID: tenant})
	s.byID[id] = &domain.Notification{
		NotificationID:   id,
		TenantID:         tenant,
		Channel:          domain.ChannelEmail,
		RecipientAddress: "someone@example.com",
		Status:           "PENDING",
		DeliveryAttempts: attempts,
		CorrelationID:    "corr-" + id,
	}
}

// ── worker ──────────────────────────────────────────────────────────────────

func TestWorkerConcludesSentOnSuccess(t *testing.T) {
	s := newStubStore()
	seed(s, "n1", "tenant-a", 1)
	d := &stubDeliverer{outcome: domain.DeliveryOutcome{Delivered: true, ProviderResponse: "smtp; queued"}}
	p := &stubPublisher{}

	newWorker(s, d, p, nil, retry.DefaultPolicy).RunOnce(context.Background())

	if len(s.completed) != 1 || s.completed[0] != "n1:SENT" {
		t.Fatalf("completed = %v, want [n1:SENT]", s.completed)
	}
	if p.sent != 1 || p.failed != 0 {
		t.Fatalf("published sent=%d failed=%d, want 1/0", p.sent, p.failed)
	}
}

// The point of the whole package: a transient failure with attempts remaining
// must reschedule, NOT conclude, and must publish nothing — a
// notification.failed followed two minutes later by a notification.sent would
// have consumers act on an outcome that never happened.
func TestWorkerReschedulesTransientFailureWithoutPublishing(t *testing.T) {
	s := newStubStore()
	seed(s, "n1", "tenant-a", 1)
	d := &stubDeliverer{outcome: domain.DeliveryOutcome{Reason: "connection refused", Retryable: true}}
	p := &stubPublisher{}

	newWorker(s, d, p, nil, retry.Policy{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: time.Minute}).
		RunOnce(context.Background())

	if len(s.scheduled) != 1 {
		t.Fatalf("scheduled = %v, want one entry", s.scheduled)
	}
	if len(s.completed) != 0 {
		t.Fatalf("completed = %v, want nothing concluded", s.completed)
	}
	if p.sent != 0 || p.failed != 0 {
		t.Fatalf("published sent=%d failed=%d, want nothing published for a pending retry", p.sent, p.failed)
	}
}

func TestWorkerConcludesFailedWhenAttemptsExhausted(t *testing.T) {
	s := newStubStore()
	// 4 attempts already made against a MaxAttempts of 5: this attempt is the
	// fifth and last.
	seed(s, "n1", "tenant-a", 4)
	d := &stubDeliverer{outcome: domain.DeliveryOutcome{Reason: "connection refused", Retryable: true}}
	p := &stubPublisher{}

	newWorker(s, d, p, nil, retry.Policy{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: time.Minute}).
		RunOnce(context.Background())

	if len(s.scheduled) != 0 {
		t.Fatalf("scheduled = %v, want nothing scheduled past the limit", s.scheduled)
	}
	if len(s.completed) != 1 || s.completed[0] != "n1:FAILED" {
		t.Fatalf("completed = %v, want [n1:FAILED]", s.completed)
	}
	if p.failed != 1 {
		t.Fatalf("published failed=%d, want 1 once it terminally failed", p.failed)
	}
}

// A settled failure must not consume the retry budget — it concludes at once.
func TestWorkerConcludesSettledFailureImmediately(t *testing.T) {
	s := newStubStore()
	seed(s, "n1", "tenant-a", 1)
	d := &stubDeliverer{outcome: domain.DeliveryOutcome{Reason: "550 no such mailbox", Retryable: false}}
	p := &stubPublisher{}

	newWorker(s, d, p, nil, retry.DefaultPolicy).RunOnce(context.Background())

	if len(s.scheduled) != 0 {
		t.Fatalf("scheduled = %v, want a permanent refusal not to be retried", s.scheduled)
	}
	if len(s.completed) != 1 || s.completed[0] != "n1:FAILED" {
		t.Fatalf("completed = %v, want [n1:FAILED]", s.completed)
	}
}

// The regression that makes rescheduling a resolution failure worthwhile: the
// first attempt failed because identity-context-svc was down, so the row has
// no address. Re-attempting the transport alone would fail forever on an empty
// To — the resolution has to be retried first.
func TestWorkerReresolvesAMissingAddressBeforeDelivering(t *testing.T) {
	s := newStubStore()
	seed(s, "n1", "tenant-a", 1)
	s.byID["n1"].RecipientAddress = ""
	d := &stubDeliverer{outcome: domain.DeliveryOutcome{Delivered: true, ProviderResponse: "smtp; queued"}}
	p := &stubPublisher{}

	newWorker(s, d, p, &stubResolver{email: "resolved@example.com"}, retry.DefaultPolicy).
		RunOnce(context.Background())

	if got := s.addresses["n1"]; got != "resolved@example.com" {
		t.Fatalf("recorded address = %q, want the re-resolved one", got)
	}
	if len(d.sawAddr) != 1 || d.sawAddr[0] != "resolved@example.com" {
		t.Fatalf("deliverer saw %v, want the re-resolved address", d.sawAddr)
	}
	if len(s.completed) != 1 || s.completed[0] != "n1:SENT" {
		t.Fatalf("completed = %v, want [n1:SENT]", s.completed)
	}
}

// A principal that genuinely has no address is settled: concluded, not retried
// until the budget runs out.
func TestWorkerConcludesWhenRecipientHasNoAddress(t *testing.T) {
	s := newStubStore()
	seed(s, "n1", "tenant-a", 1)
	s.byID["n1"].RecipientAddress = ""
	d := &stubDeliverer{}
	p := &stubPublisher{}

	newWorker(s, d, p, &stubResolver{err: domain.ErrPrincipalHasNoAddress}, retry.DefaultPolicy).
		RunOnce(context.Background())

	if d.calls != 0 {
		t.Fatalf("deliverer called %d times, want 0 — there is nowhere to send", d.calls)
	}
	if len(s.scheduled) != 0 {
		t.Fatalf("scheduled = %v, want no retry for a recipient with no address", s.scheduled)
	}
	if len(s.completed) != 1 || s.completed[0] != "n1:FAILED" {
		t.Fatalf("completed = %v, want [n1:FAILED]", s.completed)
	}
}

// Every store call after the cross-tenant poll must carry the notification's
// own tenant. This is what keeps the platform-scope hatch a read-only
// discovery mechanism rather than a way for the worker to write anywhere.
func TestWorkerInstallsTheNotificationsTenantOnEveryWrite(t *testing.T) {
	s := newStubStore()
	seed(s, "n1", "tenant-a", 1)
	seed(s, "n2", "tenant-b", 1)
	d := &stubDeliverer{outcome: domain.DeliveryOutcome{Delivered: true}}

	newWorker(s, d, &stubPublisher{}, nil, retry.DefaultPolicy).RunOnce(context.Background())

	if len(s.tenantsSeen) != 2 {
		t.Fatalf("tenants seen = %v, want one per notification", s.tenantsSeen)
	}
	if s.tenantsSeen[0] != "tenant-a" || s.tenantsSeen[1] != "tenant-b" {
		t.Fatalf("tenants seen = %v, want [tenant-a tenant-b]", s.tenantsSeen)
	}
}

// A notification another replica already took must not be delivered twice.
func TestWorkerSkipsWhatItCannotClaim(t *testing.T) {
	s := newStubStore()
	seed(s, "n1", "tenant-a", 1)
	s.claimed["n1"] = true // already taken
	d := &stubDeliverer{outcome: domain.DeliveryOutcome{Delivered: true}}

	newWorker(s, d, &stubPublisher{}, nil, retry.DefaultPolicy).RunOnce(context.Background())

	if d.calls != 0 {
		t.Fatalf("deliverer called %d times for an unclaimed notification, want 0", d.calls)
	}
	if len(s.completed) != 0 {
		t.Fatalf("completed = %v, want nothing", s.completed)
	}
}
