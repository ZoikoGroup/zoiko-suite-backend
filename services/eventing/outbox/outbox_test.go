package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/eventing/envelope"
)

// openPool gives each test its own schema holding a fresh eventing_outbox, so
// tests never see each other's rows. Skips without TEST_DATABASE_URL, the
// convention every service in this repo uses; CI provides one.
func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		// Under CI a skip would report ok for a suite that never ran; every
		// guarantee this package makes is only proven against real Postgres.
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL must be set in CI")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "eventing_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, SchemaSQL); err != nil {
		t.Fatalf("apply SchemaSQL: %v", err)
	}
	return pool
}

func newEnvelope(t *testing.T, aggregateID string) *envelope.Envelope {
	t.Helper()
	e, err := envelope.New(envelope.Spec{
		Type:            "com.zoikosuite.accounting.journal.posted",
		Service:         "general-ledger-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now(),
		TenantID:        "tenant-1",
		AggregateType:   "journal",
		AggregateID:     aggregateID,
		ResidencyRegion: "uk",
		Classification:  envelope.Confidential,
		Data:            map[string]any{"journal_id": aggregateID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func enqueue(t *testing.T, pool *pgxpool.Pool, envs ...*envelope.Envelope) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, e := range envs {
		if err := Enqueue(ctx, tx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// fakeWriter records deliveries; fail decides per message whether to error.
type fakeWriter struct {
	mu        sync.Mutex
	delivered []Message
	calls     int
	fail      func(m Message) error
	probe     func() error
	probes    int
	delay     time.Duration
}

func (w *fakeWriter) Probe(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.probes++
	if w.probe != nil {
		return w.probe()
	}
	return nil
}

func (w *fakeWriter) WriteMessages(_ context.Context, msgs ...Message) error {
	if w.delay > 0 {
		time.Sleep(w.delay)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.fail != nil {
		for _, m := range msgs {
			if err := w.fail(m); err != nil {
				return err // all-or-nothing, like a failed batch
			}
		}
	}
	w.delivered = append(w.delivered, msgs...)
	return nil
}

func (w *fakeWriter) ids() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.delivered))
	for i, m := range w.delivered {
		out[i] = eventIDOf(m)
	}
	return out
}

func eventIDOf(m Message) string {
	for _, h := range m.Headers {
		if h.Key == EventIDHeader {
			return string(h.Value)
		}
	}
	return ""
}

func fastConfig() Config {
	return Config{
		BatchSize:    50,
		PollInterval: 10 * time.Millisecond,
		Lease:        time.Minute,
		MaxAttempts:  3,
		BaseBackoff:  time.Millisecond,
		MaxBackoff:   time.Millisecond,
	}
}

func newRelay(t *testing.T, pool *pgxpool.Pool, w Writer, cfg Config) *Relay {
	t.Helper()
	r, err := NewRelay(pool, w, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type row struct {
	state    string
	attempts int
	code     *string
}

func readRow(t *testing.T, pool *pgxpool.Pool, eventID string) row {
	t.Helper()
	var r row
	err := pool.QueryRow(context.Background(),
		`SELECT publish_state, attempt_count, last_error_code FROM eventing_outbox WHERE event_id = $1`,
		eventID).Scan(&r.state, &r.attempts, &r.code)
	if err != nil {
		t.Fatalf("read %s: %v", eventID, err)
	}
	return r
}

// drainUntilIdle runs passes (sleeping past the 1ms backoff) until a pass
// claims nothing or the budget runs out.
func drainUntilIdle(t *testing.T, r *Relay, passes int) {
	t.Helper()
	for i := 0; i < passes; i++ {
		time.Sleep(5 * time.Millisecond)
		res, err := r.DrainOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Claimed == 0 {
			return
		}
	}
}

// The core guarantee: an event exists only if the business transaction
// committed.
func TestEnqueue_RolledBackTransactionLeavesNoEvent(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(ctx, tx, newEnvelope(t, "j-1")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rolled-back transaction left %d outbox rows", n)
	}
}

func TestEnqueue_SameEnvelopeTwiceIsAnError(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	e := newEnvelope(t, "j-1")
	tx, _ := pool.Begin(ctx)
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := Enqueue(ctx, tx, e); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(ctx, tx, e); err == nil {
		t.Fatal("enqueueing one envelope twice must fail, not silently publish once")
	}
}

func TestRelay_DeliversTheCommittedEnvelope(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)

	w := &fakeWriter{}
	res, err := newRelay(t, pool, w, fastConfig()).DrainOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Published != 1 || len(w.delivered) != 1 {
		t.Fatalf("published %d, delivered %d; want 1, 1", res.Published, len(w.delivered))
	}
	m := w.delivered[0]
	if string(m.Key) != e.PartitionKey() {
		t.Error("message key must be the envelope's opaque partition key")
	}
	if eventIDOf(m) != e.ID {
		t.Error("X-Event-ID header must carry the event id")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(m.Value, &body); err != nil {
		t.Fatal(err)
	}
	var id, hash string
	_ = json.Unmarshal(body["id"], &id)
	_ = json.Unmarshal(body["payloadhash"], &hash)
	if id != e.ID {
		t.Errorf("body id %q, want %q", id, e.ID)
	}
	if envelope.PayloadHash(body["data"]) != hash {
		t.Error("payloadhash does not verify after a round trip through the outbox")
	}
	if _, ok := body["publishedat"]; !ok {
		t.Error("dispatcher must stamp publishedat")
	}
	if got := readRow(t, pool, e.ID); got.state != StatePublished {
		t.Errorf("state %q, want published", got.state)
	}
}

// Two (here: four) relays on one table must divide the backlog, never deliver
// a row twice. This is the bug in the hand-written relays that polled without
// FOR UPDATE SKIP LOCKED or released the lock before publishing.
func TestRelay_ConcurrentRelaysDeliverEachEventExactlyOnce(t *testing.T) {
	pool := openPool(t)
	const total = 300
	envs := make([]*envelope.Envelope, total)
	for i := range envs {
		envs[i] = newEnvelope(t, fmt.Sprintf("j-%d", i))
	}
	enqueue(t, pool, envs...)

	w := &fakeWriter{delay: 2 * time.Millisecond}
	cfg := fastConfig()
	cfg.BatchSize = 7
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		r := newRelay(t, pool, w, cfg)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				res, err := r.DrainOnce(context.Background())
				if err != nil {
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

	seen := map[string]int{}
	for _, id := range w.ids() {
		seen[id]++
	}
	for _, e := range envs {
		if seen[e.ID] != 1 {
			t.Fatalf("event %s delivered %d times, want exactly 1", e.ID, seen[e.ID])
		}
	}
	if len(seen) != total {
		t.Fatalf("delivered %d distinct events, want %d", len(seen), total)
	}
}

// A poison event must stop being retried and must not hold up the queue.
func TestRelay_PoisonEventIsQuarantinedAndOthersStillFlow(t *testing.T) {
	pool := openPool(t)
	poison := newEnvelope(t, "j-poison")
	healthy := []*envelope.Envelope{newEnvelope(t, "j-1"), newEnvelope(t, "j-2"), newEnvelope(t, "j-3")}
	enqueue(t, pool, append([]*envelope.Envelope{poison}, healthy...)...)

	w := &fakeWriter{fail: func(m Message) error {
		if eventIDOf(m) == poison.ID {
			return errors.New("record rejected")
		}
		return nil
	}}
	r := newRelay(t, pool, w, fastConfig())
	drainUntilIdle(t, r, 20)

	got := readRow(t, pool, poison.ID)
	if got.state != StateQuarantined || got.attempts != 3 {
		t.Fatalf("poison: state %q attempts %d; want quarantined after 3", got.state, got.attempts)
	}
	for _, e := range healthy {
		if s := readRow(t, pool, e.ID).state; s != StatePublished {
			t.Fatalf("healthy event %s is %q: the poison event blocked the queue", e.ID, s)
		}
	}
	stats, err := r.ReadStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quarantined != 1 || stats.Backlog != 0 {
		t.Fatalf("stats %+v; want 1 quarantined, empty backlog", stats)
	}
}

// A Kafka outage is not the events' fault: it must not spend their retry
// budget, or a long outage would quarantine the whole backlog.
func TestRelay_BrokerOutageDoesNotSpendAttempts(t *testing.T) {
	pool := openPool(t)
	envs := []*envelope.Envelope{newEnvelope(t, "j-1"), newEnvelope(t, "j-2"), newEnvelope(t, "j-3")}
	enqueue(t, pool, envs...)

	down := true
	var mu sync.Mutex
	w := &fakeWriter{fail: func(Message) error {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	}}
	w.probe = func() error {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return errors.New("metadata: connection refused")
		}
		return nil
	}
	r := newRelay(t, pool, w, fastConfig())

	// Far more passes than MaxAttempts.
	for i := 0; i < 10; i++ {
		time.Sleep(5 * time.Millisecond)
		if _, err := r.DrainOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range envs {
		got := readRow(t, pool, e.ID)
		if got.attempts != 0 || got.state != StatePending {
			t.Fatalf("during outage: state %q attempts %d; want pending, 0", got.state, got.attempts)
		}
		if got.code == nil || *got.code != CodeBrokerUnavailable {
			t.Fatalf("during outage: last_error_code %v; want %s", got.code, CodeBrokerUnavailable)
		}
	}

	mu.Lock()
	down = false
	mu.Unlock()
	// The relay-wide backoff grew during the outage, capped at MaxBackoff.
	drainUntilIdle(t, r, 20)
	for _, e := range envs {
		if s := readRow(t, pool, e.ID).state; s != StatePublished {
			t.Fatalf("after recovery %s is %q, want published", e.ID, s)
		}
	}
}

// A lone poison event, with nothing healthy beside it to compare against, is
// still recognised: the broker probe says the broker is up.
func TestRelay_LonePoisonEventIsQuarantinedWhenBrokerIsUp(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)
	w := &fakeWriter{fail: func(Message) error { return errors.New("record rejected") }}
	r := newRelay(t, pool, w, fastConfig())
	drainUntilIdle(t, r, 10)
	if got := readRow(t, pool, e.ID); got.attempts != 3 || got.state != StateQuarantined {
		t.Fatalf("state %q attempts %d; want quarantined after 3", got.state, got.attempts)
	}
}

// Two poison events at the head of the queue must not stall the events
// behind them. (A relay that inferred "outage" from consecutive failures
// would release the whole batch every pass and deliver nothing.)
func TestRelay_PoisonEventsAtHeadDoNotStallQueue(t *testing.T) {
	pool := openPool(t)
	p1, p2 := newEnvelope(t, "j-p1"), newEnvelope(t, "j-p2")
	healthy := newEnvelope(t, "j-ok")
	enqueue(t, pool, p1, p2, healthy)
	w := &fakeWriter{fail: func(m Message) error {
		if id := eventIDOf(m); id == p1.ID || id == p2.ID {
			return errors.New("record rejected")
		}
		return nil
	}}
	if _, err := newRelay(t, pool, w, fastConfig()).DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := readRow(t, pool, healthy.ID).state; s != StatePublished {
		t.Fatalf("healthy event behind two poison events is %q after one pass, want published", s)
	}
	for _, p := range []*envelope.Envelope{p1, p2} {
		if got := readRow(t, pool, p.ID); got.attempts != 1 {
			t.Fatalf("poison %s attempts %d, want 1", p.ID, got.attempts)
		}
	}
}

// The broker dies part-way through a pass: the events that failed after it
// died are not blamed.
func TestRelay_BrokerDyingMidPassIsNotBlamedOnEvents(t *testing.T) {
	pool := openPool(t)
	envs := []*envelope.Envelope{newEnvelope(t, "j-1"), newEnvelope(t, "j-2"), newEnvelope(t, "j-3")}
	enqueue(t, pool, envs...)
	w := &fakeWriter{fail: func(Message) error { return errors.New("broken pipe") }}
	probes := 0
	w.probe = func() error {
		probes++
		if probes == 1 {
			return nil // up when the batch failed...
		}
		return errors.New("connection refused") // ...gone by the re-probe
	}
	res, err := newRelay(t, pool, w, fastConfig()).DrainOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// j-1 failed while the broker still answered its probe: counted once.
	// j-2 failed, the re-probe failed: j-2 and j-3 released uncounted.
	if res.Retrying != 1 || res.Released != 2 {
		t.Fatalf("result %+v; want 1 retrying, 2 released", res)
	}
	if got := readRow(t, pool, envs[0].ID); got.attempts != 1 {
		t.Fatalf("first event attempts %d, want 1", got.attempts)
	}
	for _, e := range envs[1:] {
		if got := readRow(t, pool, e.ID); got.attempts != 0 || got.state != StatePending {
			t.Fatalf("%s: state %q attempts %d; want pending, 0", e.ID, got.state, got.attempts)
		}
	}
}

func TestRelay_PermanentErrorQuarantinesImmediately(t *testing.T) {
	pool := openPool(t)
	bad := newEnvelope(t, "j-bad")
	enqueue(t, pool, bad)
	w := &fakeWriter{fail: func(Message) error { return Permanent(errors.New("message too large")) }}
	if _, err := newRelay(t, pool, w, fastConfig()).DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := readRow(t, pool, bad.ID)
	if got.state != StateQuarantined || got.attempts != 1 || got.code == nil || *got.code != CodePermanent {
		t.Fatalf("state %q attempts %d code %v; want quarantined after 1 with %s",
			got.state, got.attempts, got.code, CodePermanent)
	}
}

// §6.1: "it must never generate a new event ID merely because a publish retry
// occurs."
func TestRelay_RetryKeepsTheSameEventID(t *testing.T) {
	pool := openPool(t)
	target := newEnvelope(t, "j-target")
	other := newEnvelope(t, "j-other")
	enqueue(t, pool, target, other)

	// Fails the batch write and the single retry of target in the first pass,
	// then accepts it.
	seen := 0
	w := &fakeWriter{fail: func(m Message) error {
		if eventIDOf(m) == target.ID {
			seen++
			if seen <= 2 {
				return errors.New("transient")
			}
		}
		return nil
	}}
	r := newRelay(t, pool, w, fastConfig())
	drainUntilIdle(t, r, 10)

	got := readRow(t, pool, target.ID)
	if got.state != StatePublished || got.attempts != 1 {
		t.Fatalf("state %q attempts %d; want published after one counted failure", got.state, got.attempts)
	}
	count := 0
	for _, id := range w.ids() {
		if id == target.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("target delivered %d times under its original id, want 1", count)
	}
}

// A relay that dies mid-publish leaves a claimed row. Once its lease expires
// another relay delivers it, keeping the original dispatch time because the
// dead relay's attempt may already have reached the broker.
func TestRelay_ExpiredLeaseIsReclaimedWithOriginalDispatchTime(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)
	original := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(context.Background(), `
		UPDATE eventing_outbox
		   SET publish_state = 'claimed', claimed_until = now() - interval '1 second',
		       dispatched_at = $2
		 WHERE event_id = $1`, e.ID, original); err != nil {
		t.Fatal(err)
	}

	w := &fakeWriter{}
	if _, err := newRelay(t, pool, w, fastConfig()).DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.delivered) != 1 {
		t.Fatalf("expired lease not reclaimed: %d deliveries", len(w.delivered))
	}
	var body struct {
		PublishedAt time.Time `json:"publishedat"`
	}
	if err := json.Unmarshal(w.delivered[0].Value, &body); err != nil {
		t.Fatal(err)
	}
	if !body.PublishedAt.Equal(original) {
		t.Fatalf("publishedat %v, want the original dispatch time %v", body.PublishedAt, original)
	}
}

// A live lease hides the row: this is what stops a second relay publishing an
// event the first is still sending.
func TestRelay_LiveLeaseIsNotReclaimed(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)
	if _, err := pool.Exec(context.Background(), `
		UPDATE eventing_outbox SET publish_state = 'claimed', claimed_until = now() + interval '1 minute'
		 WHERE event_id = $1`, e.ID); err != nil {
		t.Fatal(err)
	}
	w := &fakeWriter{}
	res, err := newRelay(t, pool, w, fastConfig()).DrainOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Claimed != 0 || len(w.delivered) != 0 {
		t.Fatal("a row under a live lease was published by a second relay")
	}
}

func TestRelay_CorruptStoredRowIsQuarantinedNotSent(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)
	if _, err := pool.Exec(context.Background(),
		`UPDATE eventing_outbox SET payload = '\x5b315d' WHERE event_id = $1`, e.ID); err != nil { // "[1]"
		t.Fatal(err)
	}
	w := &fakeWriter{}
	if _, err := newRelay(t, pool, w, fastConfig()).DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.delivered) != 0 {
		t.Fatal("a malformed stored envelope was sent to consumers")
	}
	if got := readRow(t, pool, e.ID); got.state != StateQuarantined || got.code == nil || *got.code != CodeInvalidStored {
		t.Fatalf("state %q code %v; want quarantined %s", got.state, got.code, CodeInvalidStored)
	}
}

func TestRequeue_ReturnsQuarantinedEventUnderItsOriginalID(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)
	broken := true
	w := &fakeWriter{fail: func(Message) error {
		if broken {
			return Permanent(errors.New("rejected"))
		}
		return nil
	}}
	r := newRelay(t, pool, w, fastConfig())
	if _, err := r.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	ok, err := r.Requeue(context.Background(), e.ID)
	if err != nil || !ok {
		t.Fatalf("requeue: %v %v", ok, err)
	}
	if ok, _ := r.Requeue(context.Background(), e.ID); ok {
		t.Fatal("requeue of a non-quarantined event must report false")
	}
	broken = false
	drainUntilIdle(t, r, 5)
	if ids := w.ids(); len(ids) != 1 || ids[0] != e.ID {
		t.Fatalf("delivered %v, want exactly the original id %s", ids, e.ID)
	}
}

func TestVerifySchema(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	if err := VerifySchema(ctx, pool); err != nil {
		t.Fatalf("schema applied but VerifySchema failed: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE eventing_outbox DROP COLUMN payload_hash`); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(ctx, pool); err == nil || !strings.Contains(err.Error(), "payload_hash") {
		t.Fatalf("missing column not reported: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE eventing_outbox`); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(ctx, pool); err == nil {
		t.Fatal("missing table not reported")
	}
}

func TestNewRelay_RejectsUnsafeConfig(t *testing.T) {
	pool := &pgxpool.Pool{}
	bad := []Config{
		{},
		func() Config { c := DefaultConfig(); c.Lease = 0; return c }(),
		func() Config { c := DefaultConfig(); c.MaxBackoff = time.Millisecond; return c }(),
	}
	for i, c := range bad {
		if _, err := NewRelay(pool, &fakeWriter{}, c, nil); err == nil {
			t.Errorf("config %d accepted", i)
		}
	}
}
