package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	svcenvelope "zoiko.io/key-management-svc/internal/envelope"
	"zoiko.io/key-management-svc/internal/outbox"
	"zoiko.io/key-management-svc/internal/store"
	"zoiko.io/key-management-svc/internal/testdb"
)

type fakePub struct {
	mu   sync.Mutex
	sent map[string][]byte
	fail bool
}

func (f *fakePub) PublishOutbox(_ context.Context, id, _ string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return context.DeadlineExceeded
	}
	if f.sent == nil {
		f.sent = map[string][]byte{}
	}
	f.sent[id] = payload
	return nil
}

func actorCtx(tenant, actor string) context.Context {
	return svcenvelope.WithEnvelope(context.Background(),
		svcenvelope.Envelope{TenantID: tenant, ActorSubjectID: actor, CorrelationID: "corr-" + actor})
}

type outboxRow struct {
	eventType, actor string
	payload          []byte
}

func eventsFor(t *testing.T, pools *testdb.Pools, keyID string) []outboxRow {
	t.Helper()
	rows, err := pools.Admin.Query(context.Background(), `
		SELECT event_type, COALESCE(actor_id, ''), payload FROM outbox_events
		 WHERE aggregate_id = $1 ORDER BY created_at, outbox_event_id`, keyID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.eventType, &r.actor, &r.payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// Register, rotate and disable each write exactly one lifecycle event in the
// same transaction, attributed to the request's principal; disabling an
// already disabled key is a no-op and writes nothing.
func TestOutbox_KeyLifecycleEvents(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	tenant := newTenant()
	s := store.NewPgStore(app)
	ctx := actorCtx(tenant, "alice")

	key := newScopedKey("le-1")
	if err := s.CreateKey(ctx, tenant, key); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.RotateKey(ctx, tenant, key.ID); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := s.DisableKey(ctx, tenant, key.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := s.DisableKey(ctx, tenant, key.ID); err != nil {
		t.Fatalf("disable again: %v", err)
	}
	evs := eventsFor(t, pools, key.ID)
	want := []string{store.EventKeyRegistered, store.EventKeyRotated, store.EventKeyDisabled}
	if len(evs) != len(want) {
		t.Fatalf("events = %d, want %d (%v)", len(evs), len(want), evs)
	}
	// created_at can tie inside one test; order by expected type instead.
	byType := map[string]outboxRow{}
	for _, e := range evs {
		byType[e.eventType] = e
	}
	for _, typ := range want {
		e, ok := byType[typ]
		if !ok || e.actor != "alice" {
			t.Fatalf("missing or misattributed %s event: %+v", typ, e)
		}
		var env struct {
			SchemaVersion string `json:"schema_version"`
			Payload       struct {
				ID         string `json:"id"`
				KeyVersion int    `json:"key_version"`
				State      string `json:"state"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(e.payload, &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if env.SchemaVersion != "1.0" || env.Payload.ID != key.ID {
			t.Fatalf("%s envelope = %+v", typ, env)
		}
		if typ == store.EventKeyRotated && env.Payload.KeyVersion != 2 {
			t.Fatalf("rotated event key_version = %d, want 2", env.Payload.KeyVersion)
		}
		if typ == store.EventKeyDisabled && env.Payload.State != "DISABLED" {
			t.Fatalf("disabled event state = %s", env.Payload.State)
		}
	}
}

// When the outbox insert fails the business change rolls back with it: a
// register leaves no key and no event. Induced with a NOT VALID check
// constraint that rejects one marker legal entity only.
func TestOutbox_FailedEventRollsBackChange(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	tenant := newTenant()
	marker := "le-reject-" + uuid.NewString()[:8]
	cname := pgx.Identifier{"test_reject_" + marker[10:]}.Sanitize()
	if _, err := pools.Admin.Exec(context.Background(),
		`ALTER TABLE outbox_events ADD CONSTRAINT `+cname+` CHECK (legal_entity_id IS DISTINCT FROM '`+marker+`') NOT VALID`); err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pools.Admin.Exec(context.Background(), `ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS `+cname)
	})
	if err := store.NewPgStore(app).CreateKey(actorCtx(tenant, "alice"), tenant, newScopedKey(marker)); err == nil {
		t.Fatal("create must fail when its event cannot be written")
	}
	var keys, evs int
	if err := pools.Admin.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM customer_keys WHERE legal_entity_id = $1),
		        (SELECT count(*) FROM outbox_events WHERE legal_entity_id = $1)`, marker).Scan(&keys, &evs); err != nil {
		t.Fatalf("count: %v", err)
	}
	if keys != 0 || evs != 0 {
		t.Fatalf("after rollback: keys=%d events=%d, want 0 and 0", keys, evs)
	}
}

// The relay, as the ordinary app role, records a failed publish and later
// publishes the row and marks it published.
func TestOutbox_RelayPublishesAndMarks(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	tenant := newTenant()
	key := newScopedKey("le-1")
	if err := store.NewPgStore(app).CreateKey(actorCtx(tenant, "alice"), tenant, key); err != nil {
		t.Fatalf("create: %v", err)
	}
	var id string
	if err := pools.Admin.QueryRow(context.Background(),
		`SELECT outbox_event_id::text FROM outbox_events WHERE aggregate_id = $1`, key.ID).Scan(&id); err != nil {
		t.Fatalf("outbox row: %v", err)
	}
	if _, err := outbox.NewRelay(app, &fakePub{fail: true}, time.Second, 1000, zap.NewNop()).RelayOnce(context.Background()); err != nil {
		t.Fatalf("relay (failing): %v", err)
	}
	var attempts int
	var published *time.Time
	if err := pools.Admin.QueryRow(context.Background(),
		`SELECT publish_attempts, published_at FROM outbox_events WHERE outbox_event_id = $1`, id).Scan(&attempts, &published); err != nil {
		t.Fatalf("read: %v", err)
	}
	if attempts < 1 || published != nil {
		t.Fatalf("after failed publish: attempts=%d published=%v", attempts, published)
	}
	pub := &fakePub{}
	relay := outbox.NewRelay(app, pub, time.Second, 1000, zap.NewNop())
	for i := 0; i < 20 && pub.sent[id] == nil; i++ {
		if _, err := relay.RelayOnce(context.Background()); err != nil {
			t.Fatalf("relay: %v", err)
		}
	}
	if pub.sent[id] == nil {
		t.Fatal("relay never published the key.registered row")
	}
	if err := pools.Admin.QueryRow(context.Background(),
		`SELECT published_at FROM outbox_events WHERE outbox_event_id = $1`, id).Scan(&published); err != nil || published == nil {
		t.Fatalf("not marked published: %v %v", published, err)
	}
}

// FORCE RLS: an unscoped app session sees no outbox rows; a request-path
// insert for another tenant is refused.
func TestOutbox_RLS(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	tenant := newTenant()
	if err := store.NewPgStore(app).CreateKey(actorCtx(tenant, "alice"), tenant, newScopedKey("le-1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	var visible int
	if err := app.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&visible); err != nil {
		t.Fatalf("count: %v", err)
	}
	if visible != 0 {
		t.Fatalf("unscoped app session sees %d outbox rows, want 0", visible)
	}
	err := asTenant(t, app, newTenant(), func(tx pgx.Tx) error {
		_, err := outbox.Insert(context.Background(), tx, store.SourceService, outbox.Event{
			AggregateType: "x", AggregateID: uuid.NewString(), EventType: "forged", TenantID: tenant, Payload: map[string]string{},
		})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("cross-tenant outbox insert must be refused by RLS, got %v", err)
	}
}
