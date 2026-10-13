package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/events"
	"zoiko.io/governance-decision-log-svc/internal/outbox"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

// insertDecision records d with no idempotency key and no event — the shape
// the pre-outbox tests were written against.
func insertDecision(ctx context.Context, s *store.PgStore, d domain.GovernanceDecision) (bool, error) {
	_, created, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d})
	return created, err
}

func countScoped(t *testing.T, ctx context.Context, tx pgx.Tx, q string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// A decision, its idempotency key and its outbox event commit together.
func TestPgStore_RecordDecision_WritesKeyAndEventInOneTransaction(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	d := sampleDecision("dec-atomic")
	ev, err := events.DecisionRecorded(d)
	if err != nil {
		t.Fatal(err)
	}
	stored, created, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d, IdempotencyKey: "k-atomic", BodyHash: []byte{1}, Event: ev})
	if err != nil || !created {
		t.Fatalf("expected created, got created=%v err=%v", created, err)
	}
	if stored.RecordedAt.IsZero() {
		t.Error("expected the server's stored_at to be returned as recorded_at")
	}
	scoped(t, pool, "tenant-1", func(tx pgx.Tx) {
		if n := countScoped(t, ctx, tx, `SELECT COUNT(*) FROM idempotency_keys WHERE idempotency_key = 'k-atomic'`); n != 1 {
			t.Errorf("expected 1 idempotency key, got %d", n)
		}
		if n := countScoped(t, ctx, tx, `SELECT COUNT(*) FROM outbox WHERE aggregate_id = 'dec-atomic' AND published_at IS NULL`); n != 1 {
			t.Errorf("expected 1 pending outbox event keyed by the decision, got %d", n)
		}
	})
}

// A key already committed by another request rolls the whole write back: no
// decision, no event.
func TestPgStore_RecordDecision_KeyInUseWritesNothing(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: sampleDecision("dec-k1"), IdempotencyKey: "shared", BodyHash: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	ev, _ := events.DecisionRecorded(sampleDecision("dec-k2"))
	_, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: sampleDecision("dec-k2"), IdempotencyKey: "shared", BodyHash: []byte{2}, Event: ev})
	if !errors.Is(err, domain.ErrIdempotencyKeyInUse) {
		t.Fatalf("expected ErrIdempotencyKeyInUse, got %v", err)
	}
	scoped(t, pool, "tenant-1", func(tx pgx.Tx) {
		if n := countScoped(t, ctx, tx, `SELECT COUNT(*) FROM governance_decisions WHERE decision_id = 'dec-k2'`); n != 0 {
			t.Errorf("the losing write left its decision behind (%d rows)", n)
		}
		if n := countScoped(t, ctx, tx, `SELECT COUNT(*) FROM outbox WHERE aggregate_id = 'dec-k2'`); n != 0 {
			t.Errorf("the losing write left its event behind (%d rows)", n)
		}
	})
}

// A repeat decision_id returns the STORED row, so the handler can refuse a
// changed body instead of echoing it.
func TestPgStore_RecordDecision_RepeatReturnsStoredRow(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	d := sampleDecision("dec-repeat")
	if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d}); err != nil {
		t.Fatal(err)
	}
	changed := d
	changed.Outcome = "GRANTED"
	stored, created, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: changed, Event: []byte(`{}`)})
	if err != nil || created {
		t.Fatalf("expected created=false, got %v / %v", created, err)
	}
	if stored.Outcome != "DENIED" {
		t.Fatalf("expected the stored outcome DENIED, got %q", stored.Outcome)
	}
	scoped(t, pool, "tenant-1", func(tx pgx.Tx) {
		if n := countScoped(t, ctx, tx, `SELECT COUNT(*) FROM outbox WHERE aggregate_id = 'dec-repeat'`); n != 0 {
			t.Errorf("a repeat must queue no event, got %d", n)
		}
	})
}

// Audit D12: decision_id was a global key, so a second tenant using the same id
// got 409 — learning that the id exists elsewhere. Each tenant now has its own
// namespace.
func TestPgStore_DecisionIDIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	a := sampleDecision("dec-shared-id")
	b := a
	b.TenantID = "tenant-2"
	b.Outcome = "GRANTED"
	if c, err := insertDecision(ctx, s, a); err != nil || !c {
		t.Fatalf("tenant-1: %v %v", c, err)
	}
	if c, err := insertDecision(ctx, s, b); err != nil || !c {
		t.Fatalf("tenant-2 must be able to use the same id: created=%v err=%v", c, err)
	}
	got, err := s.FindByID(ctx, "tenant-1", "dec-shared-id")
	if err != nil || got.Outcome != "DENIED" {
		t.Fatalf("tenant-1 must still see its own row: %+v %v", got, err)
	}
}

// Audit A3: replay manifests had no tenant_id and no RLS. Under the runtime
// role, tenant-2 sees none of tenant-1's manifests even by naming the decision.
func TestPgStore_ReplayManifestsAreTenantIsolated(t *testing.T) {
	ctx := context.Background()
	pool := appRolePool(t, openTestPool(t))
	s := store.New(pool, zap.NewNop())

	if _, err := insertDecision(ctx, s, sampleDecision("dec-rm")); err != nil {
		t.Fatal(err)
	}
	m := &domain.ReplayManifest{
		ReplayManifestID: "11111111-1111-1111-1111-111111111111", DecisionID: "dec-rm", PolicyVersionID: "pv",
		ReplayedOutcome: "DENIED", OriginalOutcome: "DENIED", OutcomesMatch: true,
		ReplayedByPrincipalID: "auditor", TenantID: "tenant-1", IdempotencyKey: "rk", RequestHash: []byte{9},
	}
	if _, created, err := s.CreateReplayManifest(ctx, m, nil); err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}

	mine, err := s.ListReplayManifestsByDecision(ctx, "tenant-1", "dec-rm")
	if err != nil || len(mine) != 1 {
		t.Fatalf("tenant-1 should see its manifest: %d %v", len(mine), err)
	}
	theirs, err := s.ListReplayManifestsByDecision(ctx, "tenant-2", "dec-rm")
	if err != nil || len(theirs) != 0 {
		t.Fatalf("tenant-2 must see nothing, got %d (%v)", len(theirs), err)
	}
	// Even a raw, unfiltered SELECT under tenant-2's scope sees nothing: RLS,
	// not only the WHERE clause.
	scoped(t, pool, "tenant-2", func(tx pgx.Tx) {
		if n := countScoped(t, ctx, tx, `SELECT COUNT(*) FROM replay_manifests`); n != 0 {
			t.Errorf("RLS let tenant-2 see %d manifests", n)
		}
	})

	// A manifest filed under another tenant's decision is refused by the FK
	// (tenant_id, decision_id) — tenant-2 has no decision dec-rm.
	foreign := *m
	foreign.ReplayManifestID = "22222222-2222-2222-2222-222222222222"
	foreign.TenantID = "tenant-2"
	foreign.IdempotencyKey = "rk2"
	if _, _, err := s.CreateReplayManifest(ctx, &foreign, nil); err == nil {
		t.Fatal("expected a manifest against another tenant's decision to be refused")
	}
}

// Audit I6: a retried replay with the same key must not write a second manifest.
func TestPgStore_ReplayManifestIdempotentOnKey(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	if _, err := insertDecision(ctx, s, sampleDecision("dec-rk")); err != nil {
		t.Fatal(err)
	}
	mk := func(id string) *domain.ReplayManifest {
		return &domain.ReplayManifest{
			ReplayManifestID: id, DecisionID: "dec-rk", PolicyVersionID: "pv",
			ReplayedOutcome: "DENIED", OriginalOutcome: "DENIED", OutcomesMatch: true,
			ReplayedByPrincipalID: "auditor", TenantID: "tenant-1", IdempotencyKey: "same", RequestHash: []byte{1},
		}
	}
	first, created, err := s.CreateReplayManifest(ctx, mk("33333333-3333-3333-3333-333333333333"), &store.ReplayEvent{Envelope: []byte(`{}`), LegalEntityID: "entity-1", CorrelationID: "c"})
	if err != nil || !created {
		t.Fatalf("first: %v %v", created, err)
	}
	second, created, err := s.CreateReplayManifest(ctx, mk("44444444-4444-4444-4444-444444444444"), &store.ReplayEvent{Envelope: []byte(`{}`), LegalEntityID: "entity-1", CorrelationID: "c"})
	if err != nil || created {
		t.Fatalf("retry must not create: created=%v err=%v", created, err)
	}
	if second.ReplayManifestID != first.ReplayManifestID {
		t.Fatalf("retry returned %s, want the first manifest %s", second.ReplayManifestID, first.ReplayManifestID)
	}
}

// Audit D9: TRUNCATE bypasses row triggers; 000011 blocks it by statement trigger.
func TestPgStore_TruncateIsRefused(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	for _, table := range []string{"governance_decisions", "replay_manifests"} {
		if _, err := pool.Exec(ctx, "TRUNCATE "+table+" CASCADE"); err == nil {
			t.Errorf("TRUNCATE %s succeeded on an append-only table", table)
		}
	}
}

type capturingWriter struct {
	mu   sync.Mutex
	msgs []kafka.Message
}

func (c *capturingWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range msgs {
		if m.Topic != "" {
			return errors.New("kafka-go refuses a message topic when the writer has one")
		}
	}
	c.msgs = append(c.msgs, msgs...)
	return nil
}

type failingWriter struct{}

func (failingWriter) WriteMessages(context.Context, ...kafka.Message) error {
	return errors.New("broker down")
}

// Audit V2/V3: the relay read the outbox with no tenant, which under the
// runtime role's FORCE RLS matched nothing — no event was ever published. It
// also set a topic on every message, which kafka-go refuses.
func TestOutboxRelay_DrainsEveryTenantAsRuntimeRole(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	for _, tenant := range []string{"tenant-1", "tenant-2"} {
		d := sampleDecision("dec-relay")
		d.TenantID = tenant
		ev, _ := events.DecisionRecorded(d)
		if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d, IdempotencyKey: "k-" + tenant, BodyHash: []byte{1}, Event: ev}); err != nil {
			t.Fatal(err)
		}
	}

	// A broker outage leaves every event pending, with the attempt recorded.
	down := outbox.NewWorker(pool, failingWriter{}, zap.NewNop())
	if n, err := down.RelayOnce(ctx); err != nil || n != 0 {
		t.Fatalf("relay against a dead broker: published=%d err=%v", n, err)
	}

	w := &capturingWriter{}
	relay := outbox.NewWorker(pool, w, zap.NewNop())
	n, err := relay.RelayOnce(ctx)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	if n != 2 || len(w.msgs) != 2 {
		t.Fatalf("expected both tenants' events published, got %d (%d messages)", n, len(w.msgs))
	}
	for _, m := range w.msgs {
		if string(m.Key) != "dec-relay" {
			t.Errorf("message keyed %q, want the aggregate id dec-relay", m.Key)
		}
		// A consumer recomputing payload_hash over the published payload must
		// get the value the envelope carries — the bytes must not have been
		// re-serialised on the way through the outbox.
		var env struct {
			PayloadHash string          `json:"payload_hash"`
			Payload     json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(m.Value, &env); err != nil {
			t.Fatalf("published value is not the envelope: %v", err)
		}
		sum := sha256.Sum256(env.Payload)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != env.PayloadHash {
			t.Errorf("payload_hash %s does not match the published payload (%s)", env.PayloadHash, got)
		}
	}
	if again, _ := relay.RelayOnce(ctx); again != 0 {
		t.Fatalf("published events were relayed again (%d)", again)
	}
	scoped(t, pool, "tenant-1", func(tx pgx.Tx) {
		var attempts int
		if err := tx.QueryRow(ctx, `SELECT attempts FROM outbox WHERE tenant_id = 'tenant-1'`).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != 1 {
			t.Errorf("expected the failed attempt recorded once, got %d", attempts)
		}
	})
}

// The decision's jurisdiction context is stored and read back (000015).
func TestPgStore_JurisdictionContextRoundTrips(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	d := sampleDecision("dec-jur")
	jur := "JUR-GB"
	d.JurisdictionContext = &jur
	if _, err := insertDecision(ctx, s, d); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindByID(ctx, "tenant-1", "dec-jur")
	if err != nil || got.JurisdictionContext == nil || *got.JurisdictionContext != "JUR-GB" {
		t.Fatalf("expected JUR-GB back, got %+v / %v", got, err)
	}
}

// A replay queues exactly one governance.decision.replayed event, in the
// manifest's transaction; a retry with the same key queues none.
func TestPgStore_ReplayQueuesOneEvent(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	if _, err := insertDecision(ctx, s, sampleDecision("dec-rev")); err != nil {
		t.Fatal(err)
	}
	mk := func(id string) *domain.ReplayManifest {
		return &domain.ReplayManifest{
			ReplayManifestID: id, DecisionID: "dec-rev", PolicyVersionID: "pv",
			ReplayedOutcome: "DENIED", OriginalOutcome: "DENIED", OutcomesMatch: true,
			ReplayedByPrincipalID: "auditor", TenantID: "tenant-1", IdempotencyKey: "rk", RequestHash: []byte{1},
		}
	}
	ev := &store.ReplayEvent{Envelope: []byte(`{"event_type":"governance.decision.replayed"}`), LegalEntityID: "entity-1", CorrelationID: "c"}
	for _, id := range []string{"55555555-5555-5555-5555-555555555555", "66666666-6666-6666-6666-666666666666"} {
		if _, _, err := s.CreateReplayManifest(ctx, mk(id), ev); err != nil {
			t.Fatal(err)
		}
	}
	scoped(t, pool, "tenant-1", func(tx pgx.Tx) {
		if n := countScoped(t, ctx, tx, `SELECT COUNT(*) FROM outbox WHERE event_type = 'governance.decision.replayed' AND aggregate_id = 'dec-rev'`); n != 1 {
			t.Errorf("expected exactly one replay event, got %d", n)
		}
	})
}

// Re-audit R-8: the relay reports its backlog, and retention removes old
// PUBLISHED rows only — never one still waiting to be published.
func TestOutbox_BacklogAndRetention(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	for _, id := range []string{"dec-old-published", "dec-new-published", "dec-pending"} {
		d := sampleDecision(id)
		ev, _ := events.DecisionRecorded(d)
		if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d, IdempotencyKey: "k-" + id, BodyHash: []byte{1}, Event: ev}); err != nil {
			t.Fatal(err)
		}
	}
	w := outbox.NewWorker(pool, &capturingWriter{}, zap.NewNop())

	pending, oldest, err := w.Backlog(ctx)
	if err != nil || pending != 3 || oldest < 0 {
		t.Fatalf("backlog before relay: pending=%d oldest=%s err=%v", pending, oldest, err)
	}

	// Mark two published, one of them long ago, as the relay would.
	scopedRelay := func(q string) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT set_config('app.outbox_relay', 'on', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	scopedRelay(`UPDATE outbox SET published_at = now() - interval '30 days' WHERE aggregate_id = 'dec-old-published'`)
	scopedRelay(`UPDATE outbox SET published_at = now() WHERE aggregate_id = 'dec-new-published'`)
	scopedRelay(`UPDATE outbox SET created_at = now() - interval '2 hours' WHERE aggregate_id = 'dec-pending'`)

	pending, oldest, err = w.Backlog(ctx)
	if err != nil || pending != 1 || oldest < time.Hour {
		t.Fatalf("backlog: pending=%d oldest=%s err=%v, want 1 pending at least 1h old", pending, oldest, err)
	}

	n, err := w.Prune(ctx, 7*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("prune removed %d (err %v), want only the 30-day-old published row", n, err)
	}
	scoped(t, pool, "tenant-1", func(tx pgx.Tx) {
		if c := countScoped(t, ctx, tx, `SELECT COUNT(*) FROM outbox`); c != 2 {
			t.Errorf("expected the recent published row and the pending row kept, got %d rows", c)
		}
		if c := countScoped(t, ctx, tx, `SELECT COUNT(*) FROM outbox WHERE aggregate_id = 'dec-pending'`); c != 1 {
			t.Error("retention removed an unpublished event")
		}
	})
}

type blockingWriter struct{}

func (blockingWriter) WriteMessages(ctx context.Context, _ ...kafka.Message) error {
	<-ctx.Done()
	return ctx.Err()
}

// Re-audit R-9: Stop used to wait out a relay pass blocked on a dead broker.
func TestOutbox_StopCancelsABlockedRelay(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	d := sampleDecision("dec-stop")
	ev, _ := events.DecisionRecorded(d)
	if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d, IdempotencyKey: "k-stop", BodyHash: []byte{1}, Event: ev}); err != nil {
		t.Fatal(err)
	}

	w := outbox.NewWorker(pool, blockingWriter{}, zap.NewNop())
	w.Start(ctx)
	time.Sleep(1200 * time.Millisecond) // let a pass start and block in WriteMessages

	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return while the relay was blocked on the broker")
	}
	scoped(t, pool, "tenant-1", func(tx pgx.Tx) {
		if c := countScoped(t, ctx, tx, `SELECT COUNT(*) FROM outbox WHERE published_at IS NULL`); c != 1 {
			t.Errorf("the interrupted event must stay pending for the next relay, got %d pending", c)
		}
	})
}
