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

// Integration tests for the transactional outbox (migration 000010).
//
// WHAT THEY ARE FOR. Until 000010, every event (notification.* and template.*)
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


// The core property: concluding a delivery and recording the event that
// announces it are ONE transaction.
func TestOutbox_ConcludingADeliveryEnqueuesItsEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := seedNotification(t, s, "tenant-a", "corr-outbox-1")
	concluded := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "250 queued as ABC", &concluded, "corr-test", domain.AttemptMeta{}); err != nil {
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


// A conclusion that does not happen must not emit an event. Two replicas can
// race on the same notification, and the loser affects zero rows — if it
// enqueued anyway, one delivery would produce two notification.sent events.
func TestOutbox_ARaceLoserEnqueuesNothing(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := seedNotification(t, s, "tenant-a", "corr-outbox-race")
	concluded := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "accepted", &concluded, "corr-test", domain.AttemptMeta{}); err != nil {
		t.Fatalf("first conclusion: %v", err)
	}

	// The second conclusion matches no PENDING row and is refused.
	err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "accepted again", &concluded, "corr-test", domain.AttemptMeta{})
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
		"corr-test", domain.AttemptMeta{}); err != nil {
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
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "accepted", &concluded, "corr-test", domain.AttemptMeta{}); err != nil {
		t.Fatalf("CompleteDelivery: %v", err)
	}

	// A failing publish keeps the row claimable and records why, so a stuck
	// event can be diagnosed from the table rather than from logs.
	boom := errors.New("broker unreachable")
	if err := s.ClaimOutbox(context.Background(), 10, func(recs []store.OutboxRecord) error {
		// Three: communication.prepared (written at creation), notification.sent and its
		// delivery.attempt.created (000014).
		if len(recs) != 3 {
			t.Errorf("claimed %d records, want 3", len(recs))
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
	if claimed != 3 {
		t.Fatalf("re-claimed %d records, want the three unpublished ones", claimed)
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
		if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "accepted", &concluded, "corr-test", domain.AttemptMeta{}); err != nil {
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
		if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "accepted", &concluded, "corr-test", domain.AttemptMeta{}); err != nil {
			t.Fatalf("CompleteDelivery: %v", err)
		}
	}

	pending, age, err = s.OutboxDepth(context.Background())
	if err != nil {
		t.Fatalf("OutboxDepth: %v", err)
	}
	// Nine: each notification enqueues communication.prepared at creation, then
	// notification.sent and delivery.attempt.created when it concludes.
	if pending != 9 {
		t.Errorf("pending = %d, want 9", pending)
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
		FROM event_outbox
		-- The per-attempt delivery.attempt.* events (migration 000014) ride
		-- alongside every conclusion; these tests are about the notification
		-- and template events, and attempt events have their own test.
		WHERE event_type NOT LIKE 'delivery.attempt.%' AND event_type <> 'communication.prepared'
		ORDER BY outbox_id`)
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

// eventTypesFor returns the outbox event types enqueued for one aggregate, in
// order.
func eventTypesFor(t *testing.T, pool *pgxpool.Pool, aggregate string) []string {
	t.Helper()
	var out []string
	for _, r := range readOutbox(t, pool) {
		if r.AggregateKey == aggregate {
			out = append(out, r.EventType)
		}
	}
	return out
}

// The event is sealed by the store from the row its UPDATE returned, so what
// goes on the wire is the committed state — delivery_attempts is 1 because
// the database says so, not because a caller predicted it.
func TestOutbox_EventCarriesTheCommittedRow(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := seedNotification(t, s, "tenant-a", "corr-outbox-committed")
	concluded := time.Now().UTC()
	if err := s.CompleteDelivery(ctx, n.NotificationID, "SENT", "", "250 queued as XYZ", &concluded, "corr-committed", domain.AttemptMeta{}); err != nil {
		t.Fatalf("CompleteDelivery: %v", err)
	}
	rows := readOutbox(t, pool)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(rows))
	}
	var env struct {
		CorrelationID string         `json:"correlation_id"`
		Payload       map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(rows[0].Payload, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.CorrelationID != "corr-committed" {
		t.Errorf("correlation_id = %q, want the one passed to the transition", env.CorrelationID)
	}
	if env.Payload["delivery_attempts"] != float64(1) {
		t.Errorf("delivery_attempts = %v, want 1 (the committed count)", env.Payload["delivery_attempts"])
	}
	if env.Payload["provider_response"] != "250 queued as XYZ" {
		t.Errorf("provider_response = %v", env.Payload["provider_response"])
	}
}

// PENDING -> PENDING_UNKNOWN enqueues notification.outcome_unknown, and
// resolving it enqueues the conclusion the original attempt would have had —
// both in the transitions' own transactions.
func TestOutbox_UnknownThenResolvedEnqueuesBoth(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	n := seedNotification(t, s, "tenant-a", "corr-outbox-unknown")
	if err := s.MarkOutcomeUnknown(ctx, n.NotificationID, "tenant-a", "reset after DATA", time.Now().UTC(), "corr-u", domain.AttemptMeta{}); err != nil {
		t.Fatalf("MarkOutcomeUnknown: %v", err)
	}
	if err := s.ResolveDeliveryOutcome(ctx, domain.ResolveDeliveryOutcomeParams{
		NotificationID: n.NotificationID, TenantID: "tenant-a", ActorPrincipalID: "operator-1",
		ResolvedStatus: domain.StatusSent, ResolutionNote: "provider confirmed delivery", CorrelationID: "corr-u",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("ResolveDeliveryOutcome: %v", err)
	}
	got := eventTypesFor(t, pool, n.NotificationID)
	want := []string{events.TypeOutcomeUnknown, events.TypeSent}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events = %v, want %v", got, want)
	}

	// A second resolution is refused and emits nothing.
	if err := s.ResolveDeliveryOutcome(ctx, domain.ResolveDeliveryOutcomeParams{
		NotificationID: n.NotificationID, TenantID: "tenant-a", ActorPrincipalID: "operator-1",
		ResolvedStatus: domain.StatusFailed, ResolutionNote: "second opinion",
	}, time.Now().UTC()); !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Fatalf("second resolve: want not-found, got %v", err)
	}
	if n := len(eventTypesFor(t, pool, n.NotificationID)); n != 2 {
		t.Fatalf("a refused resolution enqueued an event: %d events", n)
	}
}

// The four BIZ-03 lifecycle writes each enqueue their event, keyed on the
// template, in their own transaction.
func TestOutbox_TemplateLifecycleEnqueuesEachEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	tmpl := newTestTemplate(t, s, ctx, "owner-1")
	v, err := s.CreateVersion(ctx, domain.CreateVersionParams{
		TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>hi {{.name}}</p>",
		VariableSchema: []string{"name"}, CreatedByPrincipalID: "owner-1",
	})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	if _, err := s.ValidateTemplate(ctx, v.VersionID); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := s.ApproveTemplate(ctx, domain.ApproveVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "approver-1"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := s.PublishTemplate(ctx, domain.PublishVersionParams{VersionID: v.VersionID, PublishedByPrincipalID: "approver-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := s.RetireTemplate(ctx, domain.RetireTemplateParams{TemplateID: tmpl.TemplateID, RetiredByPrincipalID: "approver-1"}); err != nil {
		t.Fatalf("retire: %v", err)
	}

	got := eventTypesFor(t, pool, tmpl.TemplateID)
	want := []string{events.TypeTemplateCreated, events.TypeTemplateVersionApproved, events.TypeTemplatePublished, events.TypeTemplateRetired}
	if len(got) != len(want) {
		t.Fatalf("template events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("template events = %v, want %v", got, want)
		}
	}
}

// Outside the relay hatch, event_outbox is tenant-isolated: tenant B cannot
// see tenant A's event. Only meaningful when the suite does NOT connect as a
// superuser, which bypasses row-level security even under FORCE.
func TestOutbox_TenantIsolatedOutsideTheRelayHatch(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	var super bool
	if err := pool.QueryRow(context.Background(), "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil {
		t.Fatalf("role check: %v", err)
	}
	if super {
		t.Skip("connected as a superuser, which bypasses RLS; run as the owning non-superuser role to exercise this")
	}

	n := seedNotification(t, s, "tenant-a", "corr-outbox-rls")
	concluded := time.Now().UTC()
	if err := s.CompleteDelivery(tenantCtx("tenant-a"), n.NotificationID, "SENT", "", "ok", &concluded, "c", domain.AttemptMeta{}); err != nil {
		t.Fatalf("CompleteDelivery: %v", err)
	}

	count := func(setting, value string) int {
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", setting, value); err != nil {
			t.Fatalf("set %s: %v", setting, err)
		}
		var c int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM event_outbox WHERE event_type NOT LIKE 'delivery.attempt.%' AND event_type <> 'communication.prepared'").Scan(&c); err != nil {
			t.Fatalf("count: %v", err)
		}
		return c
	}
	if got := count("app.tenant_id", "tenant-b"); got != 0 {
		t.Errorf("tenant-b sees %d of tenant-a's outbox rows, want 0", got)
	}
	if got := count("app.tenant_id", "tenant-a"); got != 1 {
		t.Errorf("tenant-a sees %d outbox rows, want 1", got)
	}
	if got := count("app.outbox_relay", "true"); got != 1 {
		t.Errorf("the relay hatch sees %d rows, want 1", got)
	}
}

// §10.2: every durable attempt enqueues delivery.attempt.created in the same
// transaction as its row; an ambiguous one also enqueues delivery.attempt.unknown
// with a resolution deadline and the spec's NCD-014.
func TestOutbox_EveryAttemptEnqueuesItsAttemptEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")
	n := seedNotification(t, s, "tenant-a", "corr-attempt-events")

	at := time.Now().UTC()
	if err := s.ScheduleRetry(ctx, n.NotificationID, "tenant-a", "421", at, at.Add(time.Minute), domain.AttemptMeta{}); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}
	if err := s.MarkOutcomeUnknown(ctx, n.NotificationID, "tenant-a", "reset after DATA", time.Now().UTC(), "c", domain.AttemptMeta{}); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}

	var types []string
	var unknownPayload map[string]any
	ctxb := context.Background()
	tx, err := pool.Begin(ctxb)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctxb) }()
	if _, err := tx.Exec(ctxb, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		t.Fatalf("relay scope: %v", err)
	}
	rows, err := tx.Query(ctxb, `SELECT event_type, payload FROM event_outbox
		WHERE aggregate_key = $1 AND event_type LIKE 'delivery.attempt.%' ORDER BY outbox_id`, n.NotificationID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
		var et string
		var body []byte
		if err := rows.Scan(&et, &body); err != nil {
			t.Fatalf("scan: %v", err)
		}
		types = append(types, et)
		if et == "delivery.attempt.unknown" {
			var env struct {
				Payload map[string]any `json:"payload"`
			}
			_ = json.Unmarshal(body, &env)
			unknownPayload = env.Payload
		}
	}
	rows.Close()
	want := []string{"delivery.attempt.created", "delivery.attempt.created", "delivery.attempt.unknown"}
	if len(types) != len(want) {
		t.Fatalf("attempt events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("attempt events = %v, want %v", types, want)
		}
	}
	if unknownPayload["reason_code"] != "NCD-014" || unknownPayload["resolution_due_at"] == nil {
		t.Fatalf("delivery.attempt.unknown payload = %v", unknownPayload)
	}
}
