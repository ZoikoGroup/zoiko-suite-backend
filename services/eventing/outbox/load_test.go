package outbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/inbox"
)

func loadN(def int) int {
	if v, err := strconv.Atoi(os.Getenv("INT_LOAD_N")); err == nil && v > 0 {
		return v
	}
	return def
}

func enqueueMany(t *testing.T, ctxPool interface {
	Begin(context.Context) (pgx.Tx, error)
}, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	ctx := context.Background()
	for start := 0; start < n; start += 500 {
		tx, err := ctxPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i := start; i < start+500 && i < n; i++ {
			e := newEnvelope(t, fmt.Sprintf("j-%d", i))
			if err := Enqueue(ctx, tx, e); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, e.ID)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

// Load: several relays drain a large backlog concurrently. Every event is published exactly once, the backlog and its
// age reach zero within a bounded time, and nothing is quarantined.
func TestLoad_ConcurrentRelaysDrainABacklogExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	n := loadN(4000)
	pool := openPool(t)
	ids := enqueueMany(t, pool, n)

	w := &fakeWriter{}
	cfg := fastConfig()
	cfg.BatchSize = 200
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 4; i++ {
		r := newRelay(t, pool, w, cfg)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				res, err := r.DrainOnce(ctx)
				if err != nil && ctx.Err() == nil {
					t.Error(err)
					return
				}
				if res.Claimed == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	seen := map[string]int{}
	for _, id := range w.ids() {
		seen[id]++
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Fatalf("event %s was published %d times", id, seen[id])
		}
	}
	if len(seen) != n {
		t.Fatalf("published %d distinct events, want %d", len(seen), n)
	}
	st, err := newRelay(t, pool, w, cfg).ReadStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Backlog != 0 || st.Quarantined != 0 {
		t.Fatalf("after the drain: %+v", st)
	}
	// the budget is generous (a laptop with a shared Postgres), the point is that it is bounded and measured
	if budget := 60 * time.Second; elapsed > budget {
		t.Fatalf("draining %d events took %v, over the %v budget", n, elapsed, budget)
	}
	t.Logf("drained %d events with 4 relays in %v (%.0f events/s)", n, elapsed, float64(n)/elapsed.Seconds())
}

// Backpressure: the broker goes away in the middle of a large backlog. Nothing is lost, no event is quarantined for the
// broker's fault, the backlog (and its age) is visible while it is stuck, and everything publishes after recovery.
func TestLoad_BrokerOutageMidBacklogLosesNothingAndBlamesNoEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	n := loadN(1500)
	pool := openPool(t)
	ids := enqueueMany(t, pool, n)

	w := &fakeWriter{}
	var down atomic.Bool
	w.fail = func(Message) error {
		if down.Load() {
			return errors.New("broker unreachable")
		}
		return nil
	}
	w.probe = func() error {
		if down.Load() {
			return errors.New("broker unreachable")
		}
		return nil
	}
	cfg := fastConfig()
	cfg.BatchSize = 100
	cfg.MaxAttempts = 3 // a tiny budget: if the outage were blamed on events they would quarantine at once
	r := newRelay(t, pool, w, cfg)
	ctx := context.Background()

	for i := 0; i < 3; i++ { // a little flows
		if _, err := r.DrainOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	down.Store(true)
	for i := 0; i < 25; i++ { // a long outage: far more passes than any retry budget
		if _, err := r.DrainOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st, err := r.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Quarantined != 0 {
		t.Fatalf("the broker's outage quarantined %d events", st.Quarantined)
	}
	if st.Backlog == 0 || st.OldestBacklogAge <= 0 {
		t.Fatalf("a stuck backlog must be visible, with its age: %+v", st)
	}
	if alerts := Evaluate(st, Thresholds{MaxBacklog: 10}); len(alerts) == 0 {
		t.Fatal("the stuck backlog raises an alert")
	}

	down.Store(false)
	drainUntilIdle(t, r, 200)
	seen := map[string]int{}
	for _, id := range w.ids() {
		seen[id]++
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Fatalf("event %s published %d times after recovery", id, seen[id])
		}
	}
	if st, _ = r.ReadStats(ctx); st.Backlog != 0 || st.Quarantined != 0 {
		t.Fatalf("after recovery: %+v", st)
	}
}

// Load: the same events are delivered to a consumer concurrently, three times each. The business effect happens once per
// event and every other delivery is recognised as a duplicate.
func TestLoad_ConcurrentRedeliveryProducesOneEffectPerEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	n := loadN(600)
	pool := openPool(t)
	ctx := context.Background()
	for _, ddl := range []string{inbox.SchemaSQL, `CREATE TABLE effects (id BIGSERIAL PRIMARY KEY, event_id TEXT NOT NULL)`} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	in, err := inbox.New(inbox.PoolRunner{Pool: pool}, "load-consumer", inbox.Config{MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	effect := func(ctx context.Context, tx pgx.Tx, r inbox.Receipt) (string, error) {
		_, err := tx.Exec(ctx, `INSERT INTO effects (event_id) VALUES ($1)`, r.EventID)
		return "ok", err
	}
	var processed, duplicates, other atomic.Int64
	var wg sync.WaitGroup
	sem := make(chan struct{}, 24)
	start := time.Now()
	for i := 0; i < n; i++ {
		for copy := 0; copy < 3; copy++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				res, err := in.Process(ctx, inbox.Message{EventID: fmt.Sprintf("evt-%d", i), EventType: "com.zoikosuite.test.thing.changed",
					TenantID: "tenant-a", PayloadHash: "sha256:aaa"}, effect)
				switch {
				case err != nil:
					other.Add(1)
				case res.Outcome == inbox.Processed:
					processed.Add(1)
				case res.Outcome == inbox.Duplicate:
					duplicates.Add(1)
				default:
					other.Add(1)
				}
			}(i)
		}
	}
	wg.Wait()
	var effects int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM effects`).Scan(&effects); err != nil {
		t.Fatal(err)
	}
	if effects != n || processed.Load() != int64(n) || duplicates.Load() != int64(2*n) || other.Load() != 0 {
		t.Fatalf("effects=%d processed=%d duplicates=%d other=%d, want %d / %d / %d / 0", effects, processed.Load(), duplicates.Load(), other.Load(), n, n, 2*n)
	}
	t.Logf("%d deliveries in %v", 3*n, time.Since(start))
}
