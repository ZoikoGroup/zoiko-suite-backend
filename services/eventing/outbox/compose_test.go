package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/inbox"
)

// INT-NP-010 end to end: a replay after a consumer already acted reaches that consumer again under the ORIGINAL event
// id, and the consumer's inbox keeps the business effect single. The outbox and inbox are each proven alone elsewhere;
// this composes them through the real replay path.
func TestReplayAfterTheConsumerActedNeverDoublesTheEffect_INT_NP_010(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	for _, ddl := range []string{inbox.SchemaSQL, `CREATE TABLE effects (id BIGSERIAL PRIMARY KEY, event_id TEXT NOT NULL)`} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	consumer, err := inbox.New(inbox.PoolRunner{Pool: pool}, "ledger-projection", inbox.Config{MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	act := func(ctx context.Context, tx pgx.Tx, r inbox.Receipt) (string, error) {
		_, err := tx.Exec(ctx, `INSERT INTO effects (event_id) VALUES ($1)`, r.EventID)
		return "applied-" + r.EventID, err
	}
	// deliver every message the broker holds that the consumer has not yet been shown, in order
	shown := 0
	deliver := func(w *fakeWriter) (processed, duplicates int) {
		w.mu.Lock()
		msgs := append([]Message(nil), w.delivered[shown:]...)
		shown = len(w.delivered)
		w.mu.Unlock()
		for _, m := range msgs {
			parsed, err := inbox.Parse(m.Value)
			if err != nil {
				t.Fatalf("a published event must parse for its consumer: %v", err)
			}
			res, err := consumer.Process(ctx, parsed, act)
			if err != nil {
				t.Fatal(err)
			}
			switch res.Outcome {
			case inbox.Processed:
				processed++
			case inbox.Duplicate:
				duplicates++
			default:
				t.Fatalf("unexpected outcome %s", res.Outcome)
			}
		}
		return
	}
	effects := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM effects`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	v := int64(1)
	e, err := envelope.New(envelope.Spec{Type: "com.zoikosuite.accounting.journal.posted", Service: "general-ledger-svc", SchemaVersion: "1.0.0",
		OccurredAt: time.Now(), TenantID: "tenant-1", AggregateType: "journal", AggregateID: "j-1", AggregateVersion: &v,
		ResidencyRegion: "uk", Classification: envelope.Confidential, Data: map[string]any{"journal_id": "j-1"}})
	if err != nil {
		t.Fatal(err)
	}
	enqueue(t, pool, e)
	w := &fakeWriter{}
	r := newRelay(t, pool, w, fastConfig())
	drainUntilIdle(t, r, 5)
	if p, d := deliver(w); p != 1 || d != 0 || effects() != 1 {
		t.Fatalf("first delivery: processed=%d duplicates=%d effects=%d", p, d, effects())
	}

	// operators replay it, twice, after the consumer has long since acted
	for i := 0; i < 2; i++ {
		if n, err := r.Replay(ctx, []string{e.ID}, testAuth); err != nil || n != 1 {
			t.Fatalf("replay %d: %d %v", i, n, err)
		}
		drainUntilIdle(t, r, 5)
		p, d := deliver(w)
		if p != 0 || d != 1 {
			t.Fatalf("replay %d: processed=%d duplicates=%d: the consumer must recognise the original event id", i, p, d)
		}
	}
	if effects() != 1 {
		t.Fatalf("a replay produced a second business effect: %d", effects())
	}
	counts := map[string]int{}
	for _, id := range w.ids() {
		counts[id]++
	}
	if counts[e.ID] != 3 {
		t.Fatalf("the broker saw the event %d times, want 3 (original + two replays) under one id", counts[e.ID])
	}
}
