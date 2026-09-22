package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
	"zoiko.io/notification-svc/internal/store"
)

// Integration tests for the transactional outbox (migration 000005).
//
// WHAT THEY ARE FOR. Until 000005, notification.sent and notification.failed
// were written to Kafka from the handler and from the retry worker AFTER the
// delivery transaction had committed, with the error logged and discarded. A
// broker hiccup at the moment a notice concluded therefore left the delivery
// correctly recorded, the caller correctly told 201, the register correctly
// showing SENT — and no consumer anywhere ever learning that the notice went
// out, or that it did not.
//
// That failure was untestable in the old shape, because there was nothing
// between the commit and the broker to assert on. These tests assert the thing
// that now exists in between: a row in event_outbox, committed by the same
// transaction as the status transition.

// sentEvent and failedEvent seal an event the way the handler and the worker
// do. Shared by every CompleteDelivery call site in this package, because the
// argument is not optional and a test that passed a zero Outbound would be
// asserting against a refusal rather than against a conclusion.
func sentEvent(n *domain.Notification) events.Outbound {
	out, err := events.Sent("corr-test", *n)
	if err != nil {
		panic(err)
	}
	return out
}

func failedEvent(n *domain.Notification, reason string) events.Outbound {
	out, err := events.Failed("corr-test", *n, reason)
	if err != nil {
		panic(err)
	}
	return out
}

// The core property: concluding a delivery and recording the event that
// announces it are ONE transaction.
func TestOutbox_ConcludingADeliveryEnqueuesItsEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := seedNotification(t, s, "tenant-a", "corr-outbox-1")
	concluded := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "250 queued as ABC", &concluded, sentEvent(n)); err != nil {
		t.Fatalf("CompleteDelivery: %v", err)
	}

	rows := readOutbox(t, pool)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1", len(rows))
	}
	if rows[0].EventType != events.TypeSent {
		t.Errorf("event_type = %q, want %q", rows[0].EventType, events.TypeSent)
	}
	// The aggregate, not the correlation id. Two events about one notification
	// must land on one partition.
	if rows[0].AggregateKey != n.NotificationID {
		t.Errorf("aggregate_key = %q, want the notification id %q", rows[0].AggregateKey, n.NotificationID)
	}
	if rows[0].TenantID != "tenant-a" {
		t.Errorf("tenant_id = %q, want tenant-a", rows[0].TenantID)
	}
	if rows[0].PublishedAt != nil {
		t.Error("a freshly enqueued event must be unpublished")
	}
}

// A conclusion may not happen without an event. This is the structural half of
// the fix: the old shape allowed "record the transition, then optionally tell
// somebody", and the whole defect lived in the word optionally.
func TestOutbox_ConcludingWithoutAnEventIsRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := seedNotification(t, s, "tenant-a", "corr-outbox-noevent")
	concluded := time.Now().UTC()

	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "", &concluded, events.Outbound{}); err == nil {
		t.Fatal("CompleteDelivery accepted a conclusion with no event")
	}

	// And nothing was written: the refusal happens before the UPDATE, so the
	// notification is still PENDING and still deliverable.
	got, err := s.GetNotification(ctx, n.NotificationID)
	if err != nil {
		t.Fatalf("GetNotification: %v", err)
	}
	if got.Status != "PENDING" {
		t.Errorf("status = %q, want PENDING — a refused conclusion must not half-apply", got.Status)
	}
	if len(readOutbox(t, pool)) != 0 {
		t.Error("a refused conclusion must not leave an outbox row")
	}
}

// A conclusion that does not happen must not emit an event. Two replicas can
// race on the same notification, and the loser affects zero rows — if it
// enqueued anyway, one delivery would produce two notification.sent events.
func TestOutbox_ARaceLoserEnqueuesNothing(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := seedNotification(t, s, "tenant-a", "corr-outbox-race")
	concluded := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "accepted", &concluded, sentEvent(n)); err != nil {
		t.Fatalf("first conclusion: %v", err)
	}

	// The second conclusion matches no PENDING row and is refused.
	err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "accepted again", &concluded, sentEvent(n))
	if !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Fatalf("second conclusion = %v, want ErrNotificationNotFound", err)
	}

	if rows := readOutbox(t, pool); len(rows) != 1 {
		t.Errorf("outbox rows = %d, want 1 — one delivery, one event", len(rows))
	}
}

// The envelope is stored built, not assembled at publish time: it carries the
// actor and the correlation id of the request that caused the send, and the
// relay runs long after that request is gone.
func TestOutbox_StoredPayloadIsTheFinishedEnvelope(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := seedNotification(t, s, "tenant-a", "corr-outbox-envelope")
	concluded := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "FAILED", "550 no such mailbox", "", &concluded,
		failedEvent(n, "550 no such mailbox")); err != nil {
		t.Fatalf("CompleteDelivery: %v", err)
	}

	rows := readOutbox(t, pool)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(rows))
	}

	var env struct {
		EventID       string `json:"event_id"`
		EventType     string `json:"event_type"`
		EventVersion  string `json:"event_version"`
		SourceService string `json:"source_service"`
		TenantID      string `json:"tenant_id"`
		ActorID       string `json:"actor_id"`
		CorrelationID string `json:"correlation_id"`
	}
	if err := json.Unmarshal(rows[0].Payload, &env); err != nil {
		t.Fatalf("stored payload is not a JSON envelope: %v", err)
	}
	if env.EventType != events.TypeFailed || env.SourceService != "notification-svc" ||
		env.EventVersion != events.EventVersion || env.TenantID != "tenant-a" ||
		env.CorrelationID != "corr-test" || env.EventID == "" {
		t.Errorf("stored envelope is incomplete: %+v", env)
	}
	if env.ActorID != n.CreatedByPrincipalID {
		t.Errorf("actor_id = %q, want the SENDER %q", env.ActorID, n.CreatedByPrincipalID)
	}
}

// ── the relay's half ─────────────────────────────────────────────────────────

func TestOutbox_ClaimMarksPublishedOnlyWhenTheCallbackSucceeds(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := seedNotification(t, s, "tenant-a", "corr-claim-1")
	concluded := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "accepted", &concluded, sentEvent(n)); err != nil {
		t.Fatalf("CompleteDelivery: %v", err)
	}

	// A failing publish keeps the row claimable and records why, so a stuck
	// event can be diagnosed from the table rather than from logs.
	boom := errors.New("broker unreachable")
	if err := s.ClaimOutbox(context.Background(), 10, func(recs []store.OutboxRecord) error {
		if len(recs) != 1 {
			t.Errorf("claimed %d records, want 1", len(recs))
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("ClaimOutbox swallowed the publish failure: %v", err)
	}

	rows := readOutbox(t, pool)
	if rows[0].PublishedAt != nil {
		t.Fatal("a failed publish must NOT mark the event published — that is the loss the outbox exists to prevent")
	}
	if rows[0].Attempts != 1 {
		t.Errorf("attempts = %d, want 1", rows[0].Attempts)
	}
	if rows[0].LastError == nil || *rows[0].LastError == "" {
		t.Error("a failed publish must record why on the row")
	}

	// And the next drain still sees it.
	claimed := 0
	if err := s.ClaimOutbox(context.Background(), 10, func(recs []store.OutboxRecord) error {
		claimed = len(recs)
		return nil
	}); err != nil {
		t.Fatalf("second ClaimOutbox: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("re-claimed %d records, want the unpublished one", claimed)
	}
	if readOutbox(t, pool)[0].PublishedAt == nil {
		t.Error("a successful publish must mark the event published")
	}
}

// The relay drains EVERY tenant from one loop. Under FORCE ROW LEVEL SECURITY
// an unscoped relay sees nothing and reports no error, which presents as a
// relay that publishes nothing behind a perfectly healthy process — the same
// silent shape the outbox exists to remove. It must name itself.
func TestOutbox_RelayDrainsEveryTenant(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	for _, tenant := range []string{"tenant-a", "tenant-b", "tenant-c"} {
		ctx := tenantCtx(tenant)
		n := seedNotification(t, s, tenant, "corr-multi-"+tenant)
		concluded := time.Now().UTC()
		if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "accepted", &concluded, sentEvent(n)); err != nil {
			t.Fatalf("CompleteDelivery for %s: %v", tenant, err)
		}
	}

	seen := map[string]bool{}
	if err := s.ClaimOutbox(context.Background(), 100, func(recs []store.OutboxRecord) error {
		for _, r := range recs {
			seen[r.Key] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("ClaimOutbox: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("relay drained %d events across 3 tenants, want 3 — it is not crossing the tenant boundary", len(seen))
	}
}

// Depth AND age. A backlog of ten that is three seconds old is a busy service;
// a backlog of ten that is an hour old is a stopped relay, and on this service
// that is an hour of governed notices whose issue nobody has been told about.
func TestOutbox_DepthReportsBacklogAndAge(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	pending, age, err := s.OutboxDepth(context.Background())
	if err != nil {
		t.Fatalf("OutboxDepth on an empty outbox: %v", err)
	}
	if pending != 0 || age != 0 {
		t.Errorf("empty outbox reported pending=%d age=%s, want 0/0", pending, age)
	}

	for i := 0; i < 3; i++ {
		n := seedNotification(t, s, "tenant-a", "corr-depth-"+strconv.Itoa(i))
		concluded := time.Now().UTC()
		if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "accepted", &concluded, sentEvent(n)); err != nil {
			t.Fatalf("CompleteDelivery: %v", err)
		}
	}

	pending, age, err = s.OutboxDepth(context.Background())
	if err != nil {
		t.Fatalf("OutboxDepth: %v", err)
	}
	if pending != 3 {
		t.Errorf("pending = %d, want 3", pending)
	}
	if age <= 0 {
		t.Error("a non-empty outbox must report the age of its oldest entry")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

type outboxRow struct {
	TenantID     string
	EventType    string
	AggregateKey string
	Payload      []byte
	PublishedAt  *time.Time
	Attempts     int
	LastError    *string
}

// readOutbox reads the table directly, with the relay flag installed.
//
// Direct SQL rather than through ClaimOutbox, because the assertions are about
// what the table HOLDS and ClaimOutbox changes it. A test that could only see
// the outbox through the mechanism it is testing would pass against a claim
// that silently returned nothing — which, under FORCE ROW LEVEL SECURITY with a
// relay that failed to name itself, is exactly what would happen.
func readOutbox(t *testing.T, pool *pgxpool.Pool) []outboxRow {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		t.Fatalf("set relay scope: %v", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT tenant_id, event_type, aggregate_key, payload, published_at, attempts, last_error
		FROM event_outbox ORDER BY outbox_id`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.TenantID, &r.EventType, &r.AggregateKey, &r.Payload,
			&r.PublishedAt, &r.Attempts, &r.LastError); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("outbox rows: %v", err)
	}
	return out
}

// seedNotification writes one PENDING notification and returns it.
func seedNotification(t *testing.T, s *store.PgStore, tenantID, correlationID string) *domain.Notification {
	t.Helper()
	n := newNotification(tenantID, "entity-1", "recipient-1", correlationID)
	created, err := s.CreateNotification(tenantCtx(tenantID), n)
	if err != nil {
		t.Fatalf("CreateNotification: %v", err)
	}
	if !created {
		t.Fatalf("CreateNotification did not create %s", correlationID)
	}
	return n
}
