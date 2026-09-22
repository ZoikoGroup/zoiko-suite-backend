package outbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/outbox"
	"zoiko.io/access-control-svc/internal/store"
	"zoiko.io/access-control-svc/internal/telemetry"
)

// fakeClaimer models the store's contract precisely on the point that matters:
// rows stay unpublished when the publish callback returns an error, and leave
// the backlog only when it returns nil.
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
	return int64(len(f.queue)), 42 * time.Second, nil
}

type fakeSender struct {
	calls int
	sent  int
	err   error
}

func (s *fakeSender) Publish(_ context.Context, msgs []kafka.Message) error {
	s.calls++
	if s.err != nil {
		return s.err
	}
	s.sent += len(msgs)
	return nil
}

// A fresh registry per test: telemetry.NewDomain calls MustRegister, which
// panics on a duplicate, and every test here needs its own counters.
func newMetrics(t *testing.T) *telemetry.Domain {
	t.Helper()
	return telemetry.NewDomainWith(telemetry.NewRegistry(), "access-control-svc")
}

func records(n int) []store.OutboxRecord {
	out := make([]store.OutboxRecord, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, store.OutboxRecord{
			OutboxID: int64(i + 1), EventType: "role.updated",
			Key: "role-1", Body: []byte(`{"event_type":"role.updated"}`),
		})
	}
	return out
}

func TestDrainOnce_PublishesTheWholeBatchInOneCall(t *testing.T) {
	claimer := &fakeClaimer{queue: records(5)}
	sender := &fakeSender{}
	relay := outbox.NewRelay(claimer, sender, newMetrics(t), zap.NewNop())

	n, err := relay.DrainOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, 5, sender.sent)
	// One call, not five. kafka-go applies its BatchTimeout per WriteMessages
	// call, so a per-record loop turns a backlog into one tick per event.
	assert.Equal(t, 1, sender.calls)
	assert.Empty(t, claimer.queue)
}

// TestDrainOnce_FailedPublishLeavesTheBacklog is the property that makes the
// outbox worth having. The publish this replaced logged its error and moved on;
// here the error propagates, the rows stay claimed-but-unpublished, and the
// next tick retries them.
func TestDrainOnce_FailedPublishLeavesTheBacklog(t *testing.T) {
	claimer := &fakeClaimer{queue: records(3)}
	sender := &fakeSender{err: errors.New("broker unavailable")}
	metrics := newMetrics(t)
	relay := outbox.NewRelay(claimer, sender, metrics, zap.NewNop())

	n, err := relay.DrainOnce(context.Background())
	require.Error(t, err)
	assert.Zero(t, n)
	assert.Len(t, claimer.queue, 3, "a failed publish emptied the backlog — those events are lost")
	assert.Empty(t, claimer.published)

	// And a later drain, once the broker is back, delivers them.
	sender.err = nil
	n, err = relay.DrainOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Empty(t, claimer.queue)
}

func TestDrainOnce_EmptyBacklogIsNotAnError(t *testing.T) {
	claimer := &fakeClaimer{}
	sender := &fakeSender{}
	relay := outbox.NewRelay(claimer, sender, newMetrics(t), zap.NewNop())

	n, err := relay.DrainOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Zero(t, sender.calls, "an empty drain still called the broker")
}

// TestRun_StopsOnContextCancel — the relay must not outlive the process it
// belongs to, and main drains once more after cancelling it.
func TestRun_StopsOnContextCancel(t *testing.T) {
	claimer := &fakeClaimer{queue: records(2)}
	sender := &fakeSender{}
	relay := outbox.NewRelay(claimer, sender, newMetrics(t), zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		relay.Run(ctx)
		close(done)
	}()

	// Give it one drain, then stop.
	require.Eventually(t, func() bool { return len(claimer.published) == 2 }, 3*time.Second, 10*time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not stop within 3s of cancellation")
	}
}
