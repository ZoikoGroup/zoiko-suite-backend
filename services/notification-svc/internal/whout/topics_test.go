package whout

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type topicWorld struct {
	mu     sync.Mutex
	topics map[string]bool
	err    error
}

func (w *topicWorld) list(context.Context) (map[string]bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, w.err
	}
	out := map[string]bool{}
	for k, v := range w.topics {
		out[k] = v
	}
	return out, nil
}

func (w *topicWorld) create(t string) { w.mu.Lock(); w.topics[t] = true; w.mu.Unlock() }

type runs struct {
	mu   sync.Mutex
	seen [][]string
}

func (r *runs) run(ctx context.Context, topics []string) {
	r.mu.Lock()
	r.seen = append(r.seen, append([]string(nil), topics...))
	r.mu.Unlock()
	<-ctx.Done()
}

func (r *runs) all() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.seen...)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSuperviseTopicsWaitsForTopicsAndRejoinsWhenMoreAppear(t *testing.T) {
	w := &topicWorld{topics: map[string]bool{"unrelated": true}}
	r := &runs{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		SuperviseTopics(ctx, nil, []string{"a.events", "b.events"}, w.list, r.run, 20*time.Millisecond)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if len(r.all()) != 0 {
		t.Fatal("a reader joined before any source topic existed; its group would sit on an empty assignment forever")
	}
	w.create("a.events")
	waitUntil(t, "the first reader", func() bool { return len(r.all()) == 1 })
	if got := r.all()[0]; len(got) != 1 || got[0] != "a.events" {
		t.Fatalf("first reader topics: %v", got)
	}
	w.create("b.events")
	waitUntil(t, "a rejoin for the second topic", func() bool { return len(r.all()) == 2 })
	if got := r.all()[1]; len(got) != 2 {
		t.Fatalf("the rejoined reader must cover both topics: %v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if len(r.all()) != 2 {
		t.Fatalf("a stable topic set must not restart the reader: %d starts", len(r.all()))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the supervisor did not stop with its context")
	}
}

func TestSuperviseTopicsSurvivesBrokerErrorsAndRestartsAReaderThatEnds(t *testing.T) {
	w := &topicWorld{topics: map[string]bool{"a.events": true}, err: errors.New("broker down")}
	var mu sync.Mutex
	starts := 0
	run := func(ctx context.Context, _ []string) {
		mu.Lock()
		starts++
		n := starts
		mu.Unlock()
		if n == 1 {
			return // the reader dies by itself
		}
		<-ctx.Done()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go SuperviseTopics(ctx, nil, []string{"a.events"}, w.list, run, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if starts != 0 {
		t.Fatal("a reader started while the broker could not be asked")
	}
	mu.Unlock()
	w.mu.Lock()
	w.err = nil
	w.mu.Unlock()
	waitUntil(t, "a reader that ends to be started again", func() bool { mu.Lock(); defer mu.Unlock(); return starts >= 2 })
}
