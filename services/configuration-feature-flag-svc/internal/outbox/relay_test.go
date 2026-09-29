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

	"zoiko.io/configuration-feature-flag-svc/internal/outbox"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
	"zoiko.io/configuration-feature-flag-svc/internal/telemetry"
)

// fakeClaimer plays the store side of the contract: it hands a batch to fn and
// records whether fn reported success, which is what decides whether the real
// store marks the rows published.
type fakeClaimer struct {
	batches   [][]store.OutboxRecord
	published [][]store.OutboxRecord
	failed    [][]store.OutboxRecord
	pending   int64
	oldest    time.Duration
	depthErr  error
}

func (f *fakeClaimer) ClaimOutbox(_ context.Context, _ int, fn func([]store.OutboxRecord) error) error {
	if len(f.batches) == 0 {
		return fn(nil)
	}
	batch := f.batches[0]
	f.batches = f.batches[1:]
	if err := fn(batch); err != nil {
		f.failed = append(f.failed, batch)
		return err
	}
	f.published = append(f.published, batch)
	return nil
}

func (f *fakeClaimer) OutboxDepth(context.Context) (int64, time.Duration, error) {
	return f.pending, f.oldest, f.depthErr
}

type fakeSender struct {
	sent []kafka.Message
	err  error
	// calls counts Publish invocations, to prove the batch goes in one call.
	calls int
}

func (f *fakeSender) Publish(_ context.Context, msgs []kafka.Message) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, msgs...)
	return nil
}

func newMetrics() *telemetry.Domain {
	return telemetry.NewDomainWith(telemetry.NewRegistry(), "configuration-feature-flag-svc")
}

func rec(id int64, t, key string) store.OutboxRecord {
	return store.OutboxRecord{OutboxID: id, EventType: t, Key: key, Body: []byte(`{"event_type":"` + t + `"}`)}
}

func TestDrainOnce_PublishesTheWholeBatchInOneCall(t *testing.T) {
	claimer := &fakeClaimer{batches: [][]store.OutboxRecord{{
		rec(1, "config.updated", "cfg-1"),
		rec(2, "feature_flag.updated", "flag-1"),
	}}}
	sender := &fakeSender{}
	r := outbox.NewRelay(claimer, sender, newMetrics(), zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Len(t, sender.sent, 2)
	// One Publish, not one per record: kafka-go waits out its BatchTimeout on
	// each partial batch, so a per-record loop makes the relay slower the
	// further behind it gets.
	assert.Equal(t, 1, sender.calls)
	assert.Len(t, claimer.published, 1)
}

// The partition key must survive into the Kafka message, or two changes to the
// same config entry can land on different partitions and be applied out of
// order by a consumer.
func TestDrainOnce_CarriesThePartitionKey(t *testing.T) {
	claimer := &fakeClaimer{batches: [][]store.OutboxRecord{{rec(1, "config.updated", "cfg-42")}}}
	sender := &fakeSender{}
	r := outbox.NewRelay(claimer, sender, newMetrics(), zap.NewNop())

	_, err := r.DrainOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, sender.sent, 1)
	assert.Equal(t, "cfg-42", string(sender.sent[0].Key))
}

// A publish failure must propagate, so the store leaves published_at NULL and
// the batch is claimed again. Reporting success here is the exact bug the
// outbox exists to remove: it would mark events delivered that never left.
func TestDrainOnce_PublishFailureIsReportedNotSwallowed(t *testing.T) {
	claimer := &fakeClaimer{batches: [][]store.OutboxRecord{{rec(1, "config.updated", "cfg-1")}}}
	sender := &fakeSender{err: errors.New("broker unreachable")}
	r := outbox.NewRelay(claimer, sender, newMetrics(), zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	require.Error(t, err)
	assert.Equal(t, 0, n)
	assert.Len(t, claimer.failed, 1)
	assert.Empty(t, claimer.published)
}

func TestDrainOnce_EmptyOutboxIsNotAnError(t *testing.T) {
	r := outbox.NewRelay(&fakeClaimer{}, &fakeSender{}, newMetrics(), zap.NewNop())
	n, err := r.DrainOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// Run must return promptly on cancellation rather than sleeping out its idle
// interval, or shutdown stalls behind a relay that has nothing to do.
func TestRun_StopsOnContextCancel(t *testing.T) {
	r := outbox.NewRelay(&fakeClaimer{}, &fakeSender{}, newMetrics(), zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not stop on context cancel")
	}
}
