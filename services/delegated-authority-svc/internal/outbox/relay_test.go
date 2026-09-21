package outbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/delegated-authority-svc/internal/outbox"
	"zoiko.io/delegated-authority-svc/internal/store"
	"zoiko.io/delegated-authority-svc/internal/telemetry"
)

// fakeClaimer models the store's contract precisely on the point that matters:
// rows stay unpublished when the publish callback returns an error, and are
// removed from the backlog only when it returns nil.
type fakeClaimer struct {
	queue     []store.OutboxRecord
	published []store.OutboxRecord
	claims    int
}

func (f *fakeClaimer) ClaimOutbox(_ context.Context, limit int, fn func([]store.OutboxRecord) error) error {
	f.claims++
	n := limit
	if len(f.queue) < n {
		n = len(f.queue)
	}
	if n == 0 {
		return nil
	}
	batch := f.queue[:n]
	if err := fn(batch); err != nil {
		return err
	}
	f.published = append(f.published, batch...)
	f.queue = f.queue[n:]
	return nil
}

func (f *fakeClaimer) OutboxDepth(context.Context) (int64, time.Duration, error) {
	return int64(len(f.queue)), 0, nil
}

type fakeSender struct {
	calls  int
	sent   int
	err    error
	failed int
}

func (s *fakeSender) Publish(_ context.Context, msgs []kafka.Message) error {
	s.calls++
	if s.err != nil {
		s.failed++
		return s.err
	}
	s.sent += len(msgs)
	return nil
}

// A fresh registry per test: telemetry.NewDomain calls prometheus.MustRegister,
// which panics on a duplicate, and every test here needs its own counters.
func newMetrics(t *testing.T) *telemetry.Domain {
	t.Helper()
	return telemetry.NewDomainWith(telemetry.NewRegistry(), "delegated-authority-svc")
}

func records(n int) []store.OutboxRecord {
	out := make([]store.OutboxRecord, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, store.OutboxRecord{
			OutboxID:  int64(i + 1),
			EventType: "authority.revoked",
			Key:       "del-1",
			Body:      []byte(`{"event_type":"authority.revoked"}`),
		})
	}
	return out
}

func TestDrainOnce_PublishesTheWholeBatchInOneSend(t *testing.T) {
	c := &fakeClaimer{queue: records(120)}
	s := &fakeSender{}
	r := outbox.NewRelay(c, s, newMetrics(t), zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 120 {
		t.Errorf("published %d, want 120", n)
	}
	// One Publish call, not 120. A per-record loop waits out kafka-go's
	// BatchTimeout once per event and drains at about one event a second.
	if s.calls != 1 {
		t.Errorf("Publish called %d times for one batch, want 1", s.calls)
	}
	if s.sent != 120 {
		t.Errorf("sent %d messages, want 120", s.sent)
	}
}

// The property the outbox exists for: a broker failure must leave the events
// in the table, not mark them delivered. Before the outbox, this same failure
// discarded the event and told the operator the revocation had succeeded.
func TestDrainOnce_FailedPublishLeavesEventsUnpublished(t *testing.T) {
	c := &fakeClaimer{queue: records(3)}
	s := &fakeSender{err: errors.New("broker unreachable")}
	r := outbox.NewRelay(c, s, newMetrics(t), zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	if err == nil {
		t.Fatal("drain reported success on a failed publish")
	}
	if n != 0 {
		t.Errorf("reported %d published on a failed drain", n)
	}
	if len(c.published) != 0 {
		t.Errorf("%d event(s) marked published despite the write failing", len(c.published))
	}
	if len(c.queue) != 3 {
		t.Errorf("backlog is %d, want the 3 events still queued for retry", len(c.queue))
	}
}

// Retried on the next tick and delivered, with nothing lost in between.
func TestDrainOnce_RetryAfterAnOutageDeliversTheBacklog(t *testing.T) {
	c := &fakeClaimer{queue: records(5)}
	s := &fakeSender{err: errors.New("broker unreachable")}
	r := outbox.NewRelay(c, s, newMetrics(t), zap.NewNop())

	if _, err := r.DrainOnce(context.Background()); err == nil {
		t.Fatal("expected the first drain to fail")
	}
	s.err = nil
	n, err := r.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if n != 5 {
		t.Errorf("recovered drain published %d, want all 5", n)
	}
	if len(c.queue) != 0 {
		t.Errorf("%d event(s) still queued after a successful drain", len(c.queue))
	}
}

func TestDrainOnce_EmptyBacklogIsNotAnError(t *testing.T) {
	c := &fakeClaimer{}
	s := &fakeSender{}
	r := outbox.NewRelay(c, s, newMetrics(t), zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain on an empty outbox: %v", err)
	}
	if n != 0 || s.calls != 0 {
		t.Errorf("empty drain published %d via %d call(s)", n, s.calls)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	c := &fakeClaimer{}
	r := outbox.NewRelay(c, &fakeSender{}, newMetrics(t), zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not stop within 5s of cancellation; shutdown would hang")
	}
}

// A backlog drains without waiting out the idle interval between batches.
// The fixed-tick version of this loop moves batchSize events per interval no
// matter how far behind it is, which is precisely wrong when it is behind.
func TestRun_DrainsAFullBacklogWithoutSleepingBetweenBatches(t *testing.T) {
	c := &fakeClaimer{queue: records(outbox.DefaultBatchSize * 3)}
	s := &fakeSender{}
	r := outbox.NewRelay(c, s, newMetrics(t), zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for {
		if len(c.queue) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("backlog of %d still had %d queued after 2s", outbox.DefaultBatchSize*3, len(c.queue))
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if s.sent != outbox.DefaultBatchSize*3 {
		t.Errorf("sent %d, want %d", s.sent, outbox.DefaultBatchSize*3)
	}
}
