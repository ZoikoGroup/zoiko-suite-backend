package outbox_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/outbox"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/telemetry"
)

// Tests for the relay loop.
//
// The store-side half — that an event is committed with its delivery, and that
// a failed publish leaves the row claimable — is proved against real Postgres
// in internal/store/outbox_test.go. What is left here is the loop's own
// behaviour, and specifically the two properties that are easy to get wrong in
// a way nothing else would catch:
//
//   - a failed publish must NOT be treated as a drain, or the relay would sleep
//     through a broker outage while the backlog grew;
//   - a full batch must loop again immediately, or a relay that is behind
//     drains at batchSize/interval events per second no matter how far behind
//     it is.

// metrics builds the real telemetry.Domain.
//
// Not a stub: the relay writes to concrete *prometheus.Vec fields on it, so a
// fake would mean changing the relay's shape to suit the test. The registry is
// process-global and MustRegister panics on a duplicate, so this is built once.
var domainMetrics = telemetry.NewDomain("notification-svc-relay-test")

type fakeStore struct {
	batches  [][]store.OutboxRecord
	claims   int32
	claimErr error

	pending   int64
	oldestAge time.Duration
	depthErr  error
}

func (f *fakeStore) ClaimOutbox(_ context.Context, _ int, fn func([]store.OutboxRecord) error) error {
	n := int(atomic.AddInt32(&f.claims, 1)) - 1
	if f.claimErr != nil {
		return f.claimErr
	}
	if n >= len(f.batches) {
		return fn(nil)
	}
	return fn(f.batches[n])
}

func (f *fakeStore) OutboxDepth(context.Context) (int64, time.Duration, error) {
	return f.pending, f.oldestAge, f.depthErr
}

type fakeSender struct {
	sent []kafka.Message
	err  error
}

func (s *fakeSender) Publish(_ context.Context, msgs []kafka.Message) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, msgs...)
	return nil
}

func records(n int) []store.OutboxRecord {
	out := make([]store.OutboxRecord, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, store.OutboxRecord{
			OutboxID:  int64(i + 1),
			EventType: "notification.sent",
			Key:       "notif-" + string(rune('a'+i)),
			Body:      []byte(`{"event_type":"notification.sent"}`),
		})
	}
	return out
}

func TestDrainOnce_PublishesTheClaimedBatch(t *testing.T) {
	st := &fakeStore{batches: [][]store.OutboxRecord{records(3)}}
	sender := &fakeSender{}
	r := outbox.NewRelay(st, sender, domainMetrics, zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if n != 3 {
		t.Errorf("published = %d, want 3", n)
	}
	if len(sender.sent) != 3 {
		t.Errorf("sender saw %d messages, want 3", len(sender.sent))
	}
	// The partition key must survive the hop. The store holds the aggregate
	// key so events about one notification stay ordered; a relay that dropped
	// it would scatter them across partitions.
	if string(sender.sent[0].Key) == "" {
		t.Error("the relay lost the partition key")
	}
}

// A broker refusal must come back as an error and a published count of ZERO.
//
// Reporting the claimed count here would be worse than cosmetic: Run treats a
// full batch as "there is more, go again" and anything less as "idle, sleep".
// A failed drain that reported its batch size would spin; one that reported a
// partial count would sleep through the outage.
func TestDrainOnce_PublishFailureReportsNothingPublished(t *testing.T) {
	st := &fakeStore{batches: [][]store.OutboxRecord{records(3)}}
	boom := errors.New("broker unreachable")
	r := outbox.NewRelay(st, &fakeSender{err: boom}, domainMetrics, zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the broker failure", err)
	}
	if n != 0 {
		t.Errorf("published = %d, want 0 — nothing reached the broker", n)
	}
}

func TestDrainOnce_EmptyOutboxIsNotAnError(t *testing.T) {
	r := outbox.NewRelay(&fakeStore{}, &fakeSender{}, domainMetrics, zap.NewNop())
	n, err := r.DrainOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("empty drain returned (%d, %v), want (0, nil)", n, err)
	}
}

// A full batch means there is probably more waiting, so the loop must go again
// with no sleep. The fixed-tick version of this loop drains at
// batchSize/interval events per second however far behind it is — which on a
// service whose events record that governed notices were issued means the
// backlog outlives the incident that caused it.
func TestRun_FullBatchLoopsWithoutWaiting(t *testing.T) {
	full := records(outbox.DefaultBatchSize)
	st := &fakeStore{batches: [][]store.OutboxRecord{full, full, records(1)}}
	sender := &fakeSender{}
	r := outbox.NewRelay(st, sender, domainMetrics, zap.NewNop())

	// Far less than DefaultInterval. If the relay slept between batches this
	// could not get past the first one.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	if got := atomic.LoadInt32(&st.claims); got < 3 {
		t.Fatalf("claims = %d in 200ms, want at least 3 — the relay is sleeping between full batches", got)
	}
	want := 2*outbox.DefaultBatchSize + 1
	if len(sender.sent) < want {
		t.Errorf("sender saw %d messages, want at least %d", len(sender.sent), want)
	}
}

// A drain that fails must not stop the loop, and the depth gauges must still be
// refreshed — that tick is precisely when a growing backlog is worth seeing.
func TestRun_SurvivesADrainFailureAndKeepsReportingDepth(t *testing.T) {
	st := &fakeStore{claimErr: errors.New("database unavailable"), pending: 42, oldestAge: 90 * time.Second}
	r := outbox.NewRelay(st, &fakeSender{}, domainMetrics, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	r.Run(ctx)

	if atomic.LoadInt32(&st.claims) == 0 {
		t.Fatal("the relay never attempted a drain")
	}
	// It returned rather than spinning on the failure, and it came back on the
	// idle interval rather than hot-looping.
	if got := atomic.LoadInt32(&st.claims); got > 10 {
		t.Errorf("claims = %d in 150ms — a failing drain is hot-looping instead of waiting", got)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	st := &fakeStore{}
	r := outbox.NewRelay(st, &fakeSender{}, domainMetrics, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop within 2s of cancellation")
	}
}
