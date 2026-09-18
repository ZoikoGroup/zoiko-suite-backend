package outbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

// DoD gate 5: "Outbox/inbox and idempotency tests pass."
//
// These cover the relay's DRAIN LOGIC — how many it takes, when it stops a
// pass, how long it waits before retrying, whether a poison event can block
// the queue forever. Those are the parts that go wrong; the SQL underneath is
// covered by the Postgres integration tests alongside the other store code.

// ── Fakes ────────────────────────────────────────────────────────────────────

type fakeRelayStore struct {
	mu sync.Mutex

	pending    []outbox.Record
	claimErr   error
	published  []string
	failures   []failure
	markPubErr error
}

type failure struct {
	eventID  string
	attempts int
	retryIn  time.Duration
	cause    string
}

func (f *fakeRelayStore) Claim(_ context.Context, maxAttempts, batchSize int) ([]outbox.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	var out []outbox.Record
	for _, r := range f.pending {
		if len(out) >= batchSize {
			break
		}
		if r.Attempts >= maxAttempts {
			// A dead-lettered event is never claimed again. This is what stops
			// a poison payload sitting at the head of the queue forever.
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeRelayStore) MarkPublished(_ context.Context, eventID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markPubErr != nil {
		return f.markPubErr
	}
	f.published = append(f.published, eventID)
	remaining := f.pending[:0]
	for _, r := range f.pending {
		if r.EventID != eventID {
			remaining = append(remaining, r)
		}
	}
	f.pending = remaining
	return nil
}

func (f *fakeRelayStore) MarkFailed(_ context.Context, eventID string, attempts int, retryIn time.Duration, cause error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, failure{eventID, attempts, retryIn, cause.Error()})
	for i := range f.pending {
		if f.pending[i].EventID == eventID {
			f.pending[i].Attempts++
		}
	}
	return nil
}

func (f *fakeRelayStore) PendingCount(_ context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending), nil
}

func (f *fakeRelayStore) DeadLetterCount(_ context.Context, maxAttempts int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.pending {
		if r.Attempts >= maxAttempts {
			n++
		}
	}
	return n, nil
}

type fakeWriter struct {
	mu      sync.Mutex
	written []outbox.KafkaMessage

	failWith error
	// failAfter makes the PER-RECORD writer succeed n times then fail, so a
	// test can assert the relay stops the pass at the first failure rather
	// than hammering the broker with the rest of the batch.
	failAfter int
	// failBatched fails only the batched fast path, which is how a test
	// reaches the per-record slow path underneath it.
	failBatched bool

	calls       int // every call, batched or not
	batchCalls  int // calls carrying more than one message
	recordCalls int // calls carrying exactly one
}

// WriteMessages distinguishes the relay's two paths.
//
// The relay sends a whole claimed batch in ONE call and only falls back to one
// call per record when that fails. Counting calls without telling the two
// apart is what let the original relay ship: it issued one call per record and
// paid kafka-go's one-second BatchTimeout on each, and every test passed
// because a stub writer has no batch timer.
func (w *fakeWriter) WriteMessages(_ context.Context, msgs ...outbox.KafkaMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++

	if len(msgs) > 1 {
		w.batchCalls++
		if w.failBatched {
			if w.failWith != nil {
				return w.failWith
			}
			return errors.New("batched write refused")
		}
		if w.failWith != nil && w.failAfter == 0 {
			return w.failWith
		}
		w.written = append(w.written, msgs...)
		return nil
	}

	w.recordCalls++
	if w.failWith != nil && (w.failAfter == 0 || w.recordCalls > w.failAfter) {
		return w.failWith
	}
	w.written = append(w.written, msgs...)
	return nil
}

func rec(id string, attempts int) outbox.Record {
	return outbox.Record{
		EventID:      id,
		EventType:    "identity.context.resolved",
		TenantID:     "tenant-1",
		PartitionKey: "session-" + id,
		Payload:      []byte(`{"event_id":"` + id + `"}`),
		Attempts:     attempts,
	}
}

func testCfg() outbox.RelayConfig {
	return outbox.RelayConfig{
		BatchSize:    10,
		PollInterval: time.Millisecond,
		MaxAttempts:  3,
		BaseBackoff:  2 * time.Second,
		MaxBackoff:   time.Minute,
	}
}

// ── Delivery ─────────────────────────────────────────────────────────────────

func TestRelayDeliversPendingEvents(t *testing.T) {
	store := &fakeRelayStore{pending: []outbox.Record{rec("e1", 0), rec("e2", 0)}}
	w := &fakeWriter{}
	r := outbox.NewRelayWithStore(store, w, testCfg(), zap.NewNop())

	n, err := r.DrainOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, []string{"e1", "e2"}, store.published)
	require.Len(t, w.written, 2)

	// The partition key is the session the event is about, not the event id:
	// ordering within a partition is per key, so two events about one session
	// must land on the same partition.
	assert.Equal(t, "session-e1", string(w.written[0].Key))
}

// TestRelayPublishesTheBatchInOneCall is the regression guard for the defect
// that made the outbox useless in practice.
//
// The relay used to call WriteMessages once per record. kafka-go's Writer is a
// batching writer whose synchronous write returns when the batch flushes, and
// a batch holding one message waits out BatchTimeout — one second by default.
// The relay therefore drained at 1.03 events per second against a
// 14,800-event backlog, with zero errors and zero retries, so every health
// signal said it was working. A minute of load left four hours of drain.
//
// Nothing about that is visible to a stub writer, which has no batch timer.
// What IS visible, and what this pins, is the shape: one call, all records.
func TestRelayPublishesTheBatchInOneCall(t *testing.T) {
	store := &fakeRelayStore{pending: []outbox.Record{rec("e1", 0), rec("e2", 0), rec("e3", 0)}}
	w := &fakeWriter{}
	r := outbox.NewRelayWithStore(store, w, testCfg(), zap.NewNop())

	n, err := r.DrainOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, 1, w.batchCalls, "the whole claimed batch goes in one write")
	assert.Zero(t, w.recordCalls, "the per-record path is for failures only")
	assert.Equal(t, []string{"e1", "e2", "e3"}, store.published)
}

func TestRelayReportsNothingWhenQueueIsEmpty(t *testing.T) {
	r := outbox.NewRelayWithStore(&fakeRelayStore{}, &fakeWriter{}, testCfg(), zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
}

// ── Failure handling ─────────────────────────────────────────────────────────

// TestRelayStopsPassAtFirstFailure pins the "do not hammer a dead broker" rule.
//
// If the first write fails, the broker is almost certainly down for the rest
// of the batch too. Continuing would turn one backoff into a hundred, and the
// hundredth event would carry an attempt count it did not earn.
func TestRelayStopsPassAtFirstFailure(t *testing.T) {
	store := &fakeRelayStore{pending: []outbox.Record{rec("e1", 0), rec("e2", 0), rec("e3", 0)}}
	w := &fakeWriter{failWith: errors.New("broker unreachable")}
	r := outbox.NewRelayWithStore(store, w, testCfg(), zap.NewNop())

	n, err := r.DrainOnce(context.Background())

	require.NoError(t, err, "a broker outage is not a relay error — it is retried")
	assert.Zero(t, n)
	assert.Equal(t, 1, w.batchCalls, "the batch is attempted once")
	assert.Equal(t, 1, w.recordCalls, "the relay must not attempt the rest of the batch one by one")
	require.Len(t, store.failures, 1)
	assert.Equal(t, "e1", store.failures[0].eventID)
}

func TestRelayDeliversWhatItCanBeforeFailing(t *testing.T) {
	store := &fakeRelayStore{pending: []outbox.Record{rec("e1", 0), rec("e2", 0), rec("e3", 0)}}
	// failBatched forces the fast path to fail so the per-record path runs;
	// failAfter then lets exactly one record through before the broker goes.
	w := &fakeWriter{failWith: errors.New("broker went away"), failBatched: true, failAfter: 1}
	r := outbox.NewRelayWithStore(store, w, testCfg(), zap.NewNop())

	n, err := r.DrainOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"e1"}, store.published)
	require.Len(t, store.failures, 1)
	assert.Equal(t, "e2", store.failures[0].eventID)
}

// TestRelayBackoffGrowsAndIsCapped pins both halves.
//
// The CAP matters more than the growth: without it, the twelfth attempt on a
// two-second base would be scheduled two and a quarter hours out, and an
// operator who had just fixed the broker would watch a healthy service deliver
// nothing for the rest of the afternoon.
func TestRelayBackoffGrowsAndIsCapped(t *testing.T) {
	cfg := testCfg()
	cfg.MaxAttempts = 100 // do not dead-letter during this test
	cfg.MaxBackoff = 30 * time.Second

	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		{4, 30 * time.Second}, // 32s would exceed the cap
		{9, 30 * time.Second},
		// Far past the int64 nanosecond ceiling. Not theoretical: at a 2s base
		// the shift overflows around attempt 62, and a NEGATIVE duration would
		// schedule the retry in the past, turning a poison event into a hot loop.
		{80, 30 * time.Second},
	} {
		store := &fakeRelayStore{pending: []outbox.Record{rec("e1", tc.attempts)}}
		w := &fakeWriter{failWith: errors.New("down")}
		r := outbox.NewRelayWithStore(store, w, cfg, zap.NewNop())

		_, err := r.DrainOnce(context.Background())
		require.NoError(t, err)

		require.Len(t, store.failures, 1)
		assert.Equal(t, tc.want, store.failures[0].retryIn,
			"attempt %d should back off %s", tc.attempts, tc.want)
		assert.Positive(t, store.failures[0].retryIn, "a retry must never be scheduled in the past")
	}
}

// TestRelayDeadLettersAfterMaxAttempts pins that a poison event stops being
// retried — and is NOT deleted.
//
// It stays in the table with its last_error, which is how an operator finds
// it. An event retried forever would keep a broken payload at the head of the
// queue indefinitely; one that was deleted would vanish without anybody
// learning it had failed.
func TestRelayDeadLettersAfterMaxAttempts(t *testing.T) {
	cfg := testCfg()
	store := &fakeRelayStore{pending: []outbox.Record{rec("poison", cfg.MaxAttempts)}}
	w := &fakeWriter{}
	r := outbox.NewRelayWithStore(store, w, cfg, zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n, "an exhausted event must not be claimed again")
	assert.Zero(t, w.calls)

	dead, err := r.DeadLetterCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, dead, "it stays visible rather than being deleted")

	pending, err := r.PendingCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, pending)
}

func TestRelayClaimFailureIsReported(t *testing.T) {
	store := &fakeRelayStore{claimErr: errors.New("postgres is gone")}
	r := outbox.NewRelayWithStore(store, &fakeWriter{}, testCfg(), zap.NewNop())

	_, err := r.DrainOnce(context.Background())
	require.Error(t, err)
}

// TestRelayRedeliversWhenBookkeepingFails is the at-least-once guarantee made
// concrete.
//
// If the Kafka write succeeds but marking it published does not, the event IS
// on the topic and will be delivered AGAIN on the next pass. That is why
// delivery is at-least-once and why every consumer in the estate dedupes on
// event_id — including this service's own, whose dedupe this relies on.
func TestRelayRedeliversWhenBookkeepingFails(t *testing.T) {
	store := &fakeRelayStore{
		pending:    []outbox.Record{rec("e1", 0)},
		markPubErr: errors.New("connection reset"),
	}
	w := &fakeWriter{}
	r := outbox.NewRelayWithStore(store, w, testCfg(), zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the write succeeded, so it counts as published")

	// Still pending, so the next pass sends it again.
	_, err = r.DrainOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, w.calls, "an unmarked event is redelivered, never lost")
}

// ── Batching ─────────────────────────────────────────────────────────────────

func TestRelayRespectsBatchSize(t *testing.T) {
	cfg := testCfg()
	cfg.BatchSize = 2

	store := &fakeRelayStore{pending: []outbox.Record{rec("e1", 0), rec("e2", 0), rec("e3", 0)}}
	r := outbox.NewRelayWithStore(store, &fakeWriter{}, cfg, zap.NewNop())

	n, err := r.DrainOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

// TestRelayRunDrainsBacklogWithoutSleepingPerBatch pins the continue-on-full-
// batch behaviour: a full batch means there is probably more waiting, so the
// loop goes straight back rather than sleeping a poll interval per batch
// through a backlog.
func TestRelayRunDrainsBacklogWithoutSleepingPerBatch(t *testing.T) {
	cfg := testCfg()
	cfg.BatchSize = 2
	cfg.PollInterval = time.Hour // any sleep would hang the test

	pending := make([]outbox.Record, 0, 6)
	for i := 0; i < 6; i++ {
		pending = append(pending, rec(string(rune('a'+i)), 0))
	}
	store := &fakeRelayStore{pending: pending}
	w := &fakeWriter{}
	r := outbox.NewRelayWithStore(store, w, cfg, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	require.Eventually(t, func() bool {
		n, _ := r.PendingCount(context.Background())
		return n == 0
	}, 2*time.Second, 5*time.Millisecond, "a backlog must drain without a poll interval between batches")

	cancel()
	<-done

	published, failed := r.Stats()
	assert.EqualValues(t, 6, published)
	assert.Zero(t, failed)
}

func TestRelayRunStopsOnContextCancel(t *testing.T) {
	r := outbox.NewRelayWithStore(&fakeRelayStore{}, &fakeWriter{}, testCfg(), zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not stop on context cancellation")
	}
}
