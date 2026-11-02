package outbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/org-structure-svc/internal/outbox"
)

// fakeStore is an in-memory RelayStore: it lets the drain logic be tested
// without Postgres (the SQL is covered by the store integration tests).
type fakeStore struct {
	// mu guards the fields below: TestRelay_RunStopsWhenTheContextIsCancelled
	// reads them from the test goroutine while Run mutates them.
	mu        sync.Mutex
	pending   []outbox.Record
	published []string
	failed    map[string]int
	retryIn   map[string]time.Duration
}

func newFakeStore(ids ...string) *fakeStore {
	s := &fakeStore{failed: map[string]int{}, retryIn: map[string]time.Duration{}}
	for _, id := range ids {
		s.pending = append(s.pending, outbox.Record{EventID: id, EventType: "OrgUnitCreated", PartitionKey: "u-" + id, Payload: []byte(`{"n":"` + id + `"}`)})
	}
	return s
}

func (s *fakeStore) Claim(_ context.Context, _, batch int) ([]outbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) < batch {
		batch = len(s.pending)
	}
	return append([]outbox.Record(nil), s.pending[:batch]...), nil
}
func (s *fakeStore) MarkPublished(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published = append(s.published, id)
	for i, r := range s.pending {
		if r.EventID == id {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			break
		}
	}
	return nil
}
func (s *fakeStore) MarkFailed(_ context.Context, id string, _ int, retryIn time.Duration, _ error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[id]++
	s.retryIn[id] = retryIn
	return nil
}
func (s *fakeStore) PendingCount(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending), nil
}

func (s *fakeStore) publishedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.published)
}
func (s *fakeStore) DeadLetterCount(context.Context, int) (int, error) { return 0, nil }

type fakeWriter struct {
	calls [][]outbox.KafkaMessage
	// failWhen, when set, decides whether a call fails.
	failWhen func(msgs []outbox.KafkaMessage) bool
}

func (w *fakeWriter) WriteMessages(_ context.Context, msgs ...outbox.KafkaMessage) error {
	w.calls = append(w.calls, msgs)
	if w.failWhen != nil && w.failWhen(msgs) {
		return errors.New("broker down")
	}
	return nil
}

func relayWith(s outbox.RelayStore, w outbox.MessageWriter) *outbox.Relay {
	cfg := outbox.DefaultRelayConfig()
	cfg.BatchSize = 10
	return outbox.NewRelayWithStore(s, w, cfg, zap.NewNop())
}

// A healthy pass publishes the whole claimed batch in ONE write, in order, keyed
// by the partition key, and marks every event.
func TestRelay_PublishesABatchInOneWriteInOrder(t *testing.T) {
	s, w := newFakeStore("a", "b", "c"), &fakeWriter{}
	n, err := relayWith(s, w).DrainOnce(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("drain: n=%d err=%v", n, err)
	}
	if len(w.calls) != 1 || len(w.calls[0]) != 3 {
		t.Fatalf("one write for the whole batch, got %d calls", len(w.calls))
	}
	if string(w.calls[0][0].Key) != "u-a" || string(w.calls[0][2].Key) != "u-c" {
		t.Fatalf("messages must keep order and the unit as key: %s ... %s", w.calls[0][0].Key, w.calls[0][2].Key)
	}
	if len(s.published) != 3 || len(s.pending) != 0 {
		t.Fatalf("all three must be marked published: %v", s.published)
	}
	if n, _ := relayWith(s, w).DrainOnce(context.Background()); n != 0 {
		t.Fatal("published events must not be sent again")
	}
}

// A broker failure stops the pass at the first failing event, schedules a retry
// with backoff, and loses nothing.
func TestRelay_BrokerFailureKeepsTheEventAndBacksOff(t *testing.T) {
	s := newFakeStore("a", "b", "c")
	w := &fakeWriter{failWhen: func(msgs []outbox.KafkaMessage) bool { return true }}
	n, err := relayWith(s, w).DrainOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("nothing is published while the broker is down: n=%d err=%v", n, err)
	}
	if len(s.pending) != 3 || len(s.published) != 0 {
		t.Fatalf("no event may be lost or marked: pending=%d published=%v", len(s.pending), s.published)
	}
	if s.failed["a"] != 1 || s.failed["b"] != 0 {
		t.Fatalf("the pass must stop at the first failure, got %v", s.failed)
	}
	if s.retryIn["a"] < time.Second {
		t.Fatalf("a retry must be scheduled with backoff, got %v", s.retryIn["a"])
	}

	// The broker returns: the same events are delivered on the next pass.
	w.failWhen = nil
	if n, _ := relayWith(s, w).DrainOnce(context.Background()); n != 3 || len(s.pending) != 0 {
		t.Fatalf("recovery: n=%d pending=%d", n, len(s.pending))
	}
}

// If only one event of a batch is bad, the others still go out and the bad one
// stays queued.
func TestRelay_PartialFailureDeliversWhatItCan(t *testing.T) {
	s := newFakeStore("a", "bad", "c")
	w := &fakeWriter{failWhen: func(msgs []outbox.KafkaMessage) bool {
		for _, m := range msgs {
			if string(m.Key) == "u-bad" {
				return true
			}
		}
		return false
	}}
	n, _ := relayWith(s, w).DrainOnce(context.Background())
	if n != 1 || len(s.published) != 1 || s.published[0] != "a" {
		t.Fatalf("the event before the failure is delivered: n=%d published=%v", n, s.published)
	}
	if s.failed["bad"] != 1 {
		t.Fatalf("the bad event must be marked failed, got %v", s.failed)
	}
	if len(s.pending) != 2 {
		t.Fatalf("bad and c stay queued, got %d", len(s.pending))
	}
}

func TestRelay_RunStopsWhenTheContextIsCancelled(t *testing.T) {
	s, w := newFakeStore("a"), &fakeWriter{}
	cfg := outbox.DefaultRelayConfig()
	cfg.PollInterval = 10 * time.Millisecond
	r := outbox.NewRelayWithStore(s, w, cfg, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for s.publishedCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("Run must publish pending events")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run must return after cancel")
	}
	if published, failed := r.Stats(); published != 1 || failed != 0 {
		t.Fatalf("stats: %d published, %d failed", published, failed)
	}
}

func TestRelay_PendingCountReportsTheBacklog(t *testing.T) {
	r := relayWith(newFakeStore("a", "b"), &fakeWriter{})
	if n, err := r.PendingCount(context.Background()); err != nil || n != 2 {
		t.Fatalf("pending: %d %v", n, err)
	}
}
