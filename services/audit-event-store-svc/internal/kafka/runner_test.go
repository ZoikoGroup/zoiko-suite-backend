package kafka

// Offset / retry / dead-letter behaviour of the consume loop, driven by fakes
// for the reader, the DLQ writer and the handler (no broker needed).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"zoiko.io/audit-event-store-svc/internal/consumer"
	"zoiko.io/audit-event-store-svc/internal/telemetry"
)

type fakeReader struct {
	mu        sync.Mutex
	msgs      []kafka.Message
	committed []int64
	commitErr error
	drained   chan struct{}
	once      sync.Once
}

func newFakeReader(msgs ...kafka.Message) *fakeReader {
	return &fakeReader{msgs: msgs, drained: make(chan struct{})}
}

func (f *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	f.mu.Lock()
	if len(f.msgs) > 0 {
		m := f.msgs[0]
		f.msgs = f.msgs[1:]
		f.mu.Unlock()
		return m, nil
	}
	f.mu.Unlock()
	f.once.Do(func() { close(f.drained) })
	<-ctx.Done()
	return kafka.Message{}, ctx.Err()
}

func (f *fakeReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		f.committed = append(f.committed, m.Offset)
	}
	return f.commitErr
}
func (f *fakeReader) Close() error { return nil }

func (f *fakeReader) committedOffsets() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.committed...)
}

type fakeDLQ struct {
	mu   sync.Mutex
	err  error
	sent []kafka.Message
}

func (d *fakeDLQ) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	d.sent = append(d.sent, msgs...)
	return nil
}
func (d *fakeDLQ) Close() error { return nil }

type fakeHandler struct {
	mu    sync.Mutex
	calls map[string]int
	fn    func(eventID string, call int) error
}

func (h *fakeHandler) Handle(_ context.Context, eventID string, _ []byte) error {
	h.mu.Lock()
	h.calls[eventID]++
	n := h.calls[eventID]
	h.mu.Unlock()
	return h.fn(eventID, n)
}

func (h *fakeHandler) count(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[id]
}

func msg(offset int64, eventID string) kafka.Message {
	return kafka.Message{
		Topic: "zoiko.test.events", Partition: 0, Offset: offset, Value: []byte(`{"x":1}`),
		Headers: []kafka.Header{{Key: "X-Event-ID", Value: []byte(eventID)}},
	}
}

// run drives the loop until the reader has been drained and the loop has
// had time to finish the last message, then stops it.
func run(t *testing.T, rd *fakeReader, dlq *fakeDLQ, h *fakeHandler) *telemetry.Metrics {
	t.Helper()
	m := &telemetry.Metrics{MessagesConsumedTotal: prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "messages_consumed_total"}, []string{"topic", "outcome"})}
	r := &Runner{reader: rd, dlqWriter: dlq, handler: h, topic: "zoiko.test.events",
		log: zaptest.NewLogger(t), metrics: m, retryBackoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-rd.drained:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not drain the reader")
	}
	time.Sleep(50 * time.Millisecond) // let the last message finish
	cancel()
	<-done
	return m
}

func outcome(m *telemetry.Metrics, label string) float64 {
	var out dto.Metric
	if err := m.MessagesConsumedTotal.WithLabelValues("zoiko.test.events", label).Write(&out); err != nil {
		return -1
	}
	return out.GetCounter().GetValue()
}

func TestRunner_ValidMessage_CommittedNoDLQ(t *testing.T) {
	rd, dlq := newFakeReader(msg(1, "evt-ok")), &fakeDLQ{}
	h := &fakeHandler{calls: map[string]int{}, fn: func(string, int) error { return nil }}
	m := run(t, rd, dlq, h)

	assert.Equal(t, 1, h.count("evt-ok"))
	assert.Equal(t, []int64{1}, rd.committedOffsets())
	assert.Empty(t, dlq.sent)
	assert.Equal(t, float64(1), outcome(m, "ok"))
}

func TestRunner_MalformedMessage_NotRetried_DeadLetteredThenCommitted(t *testing.T) {
	rd, dlq := newFakeReader(msg(7, "evt-bad")), &fakeDLQ{}
	h := &fakeHandler{calls: map[string]int{}, fn: func(string, int) error {
		return fmt.Errorf("%w: source_service is missing", consumer.ErrMalformedEvent)
	}}
	m := run(t, rd, dlq, h)

	assert.Equal(t, 1, h.count("evt-bad"), "a permanently invalid message is not retried")
	require.Len(t, dlq.sent, 1, "it is dead-lettered, not dropped")
	assert.Equal(t, []byte(`{"x":1}`), dlq.sent[0].Value, "original bytes preserved")
	assert.Contains(t, headerValue(dlq.sent[0], "X-DLQ-Reason"), "malformed event")
	assert.Equal(t, []int64{7}, rd.committedOffsets(), "committed only after the DLQ publish")
	assert.Equal(t, float64(1), outcome(m, "malformed_dead_lettered"))
}

func TestRunner_MalformedMessage_DLQFailure_OffsetNotCommitted(t *testing.T) {
	rd, dlq := newFakeReader(msg(8, "evt-bad")), &fakeDLQ{err: errors.New("broker down")}
	h := &fakeHandler{calls: map[string]int{}, fn: func(string, int) error {
		return fmt.Errorf("%w: bad", consumer.ErrMalformedEvent)
	}}
	m := run(t, rd, dlq, h)

	assert.Empty(t, rd.committedOffsets(), "with nowhere durable to put it, the offset must not advance")
	assert.Equal(t, float64(1), outcome(m, "malformed_dlq_failed"))
}

func TestRunner_StoreError_RetriedThenDeadLettered(t *testing.T) {
	rd, dlq := newFakeReader(msg(3, "evt-db")), &fakeDLQ{}
	h := &fakeHandler{calls: map[string]int{}, fn: func(string, int) error { return errors.New("db down") }}
	m := run(t, rd, dlq, h)

	assert.Equal(t, dlqRetryAttempts, h.count("evt-db"), "store failures are retried the bounded number of times")
	assert.Len(t, dlq.sent, 1)
	assert.Equal(t, []int64{3}, rd.committedOffsets())
	assert.Equal(t, float64(1), outcome(m, "dead_lettered"))
}

func TestRunner_StoreError_RecoversWithinRetries_NoDLQ(t *testing.T) {
	rd, dlq := newFakeReader(msg(4, "evt-flaky")), &fakeDLQ{}
	h := &fakeHandler{calls: map[string]int{}, fn: func(_ string, call int) error {
		if call < 2 {
			return errors.New("transient")
		}
		return nil
	}}
	m := run(t, rd, dlq, h)

	assert.Equal(t, 2, h.count("evt-flaky"))
	assert.Empty(t, dlq.sent)
	assert.Equal(t, []int64{4}, rd.committedOffsets())
	assert.Equal(t, float64(1), outcome(m, "ok"))
}

func TestRunner_StoreError_DLQFailure_OffsetNotCommitted(t *testing.T) {
	rd, dlq := newFakeReader(msg(5, "evt-db")), &fakeDLQ{err: errors.New("broker down")}
	h := &fakeHandler{calls: map[string]int{}, fn: func(string, int) error { return errors.New("db down") }}
	m := run(t, rd, dlq, h)

	assert.Empty(t, rd.committedOffsets())
	assert.Equal(t, float64(1), outcome(m, "store_error"))
}

func TestRunner_EventIDFallsBackToMessageCoordinates(t *testing.T) {
	assert.Equal(t, "zoiko.test.events:2:9", extractEventID(kafka.Message{Topic: "zoiko.test.events", Partition: 2, Offset: 9}))
	assert.Equal(t, "evt-1", extractEventID(msg(1, "evt-1")))
}

func headerValue(m kafka.Message, key string) string {
	for _, h := range m.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}
