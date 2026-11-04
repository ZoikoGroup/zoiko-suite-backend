package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/filing-tracker-svc/internal/domain"
	"zoiko.io/filing-tracker-svc/internal/middleware"
	"zoiko.io/filing-tracker-svc/internal/outbox"
	"zoiko.io/filing-tracker-svc/internal/store"
)

// appPool connects as the NOSUPERUSER NOBYPASSRLS role named by
// TEST_APP_DATABASE_URL (same database as TEST_DATABASE_URL), granting it the
// tables the migrations just created, so FORCE RLS is really exercised.
// Returns nil when unset.
func appPool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		return nil
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_APP_DATABASE_URL: %v", err)
	}
	role := pgx.Identifier{cfg.ConnConfig.User}.Sanitize()
	ctx := context.Background()
	for _, stmt := range []string{
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + role,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect app role: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// storePool is the app-role pool when configured, else the admin pool.
func storePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	if p := appPool(t, admin); p != nil {
		return p
	}
	return admin
}

func outboxRows(t *testing.T, admin *pgxpool.Pool, aggregateID string) []string {
	t.Helper()
	rows, err := admin.Query(context.Background(),
		`SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, outbox_event_id`, aggregateID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func countFilings(t *testing.T, admin *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM filing_requirements`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every write enqueues exactly its event in the outbox, with the envelope the
// direct publish used to send (topic/type/payload unchanged, schema_version 1.0).
func TestPgStore_WritesEnqueueOutboxEvents(t *testing.T) {
	admin := freshTestDB(t)
	s := store.NewPgStore(storePool(t, admin))
	ctx := middleware.WithTenant(context.Background(), "tenant-ft")

	f := newReq("2026Q4", "2026-01-31")
	if err := s.Create(ctx, f, meta); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.Notes = "n"
	if err := s.Update(ctx, f, meta); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.MarkOverdue(ctx, f.FilingID, "2026-10-23", meta); err != nil {
		t.Fatalf("overdue: %v", err)
	}
	if _, err := s.Submit(ctx, f.FilingID, &domain.SubmitFilingRequest{SubmissionReference: "S", SubmittedBy: "p2"}, store.EventMeta{ActorID: "p2"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.Confirm(ctx, f.FilingID, &domain.ConfirmFilingRequest{ConfirmationReference: "A"}, store.EventMeta{ActorID: "p3"}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	got := outboxRows(t, admin, f.FilingID)
	want := []string{store.EventFilingDue, store.EventFilingRequirementUpdated, store.EventFilingOverdue, store.EventFilingSubmitted, store.EventFilingConfirmed}
	if len(got) != len(want) {
		t.Fatalf("outbox events: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outbox events: got %v, want %v", got, want)
		}
	}

	var payload []byte
	var tenant string
	if err := admin.QueryRow(context.Background(),
		`SELECT payload, tenant_id FROM outbox_events WHERE aggregate_id=$1 AND event_type=$2`, f.FilingID, store.EventFilingDue).Scan(&payload, &tenant); err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	if env["schema_version"] != "1.0" || env["event_type"] != store.EventFilingDue || env["source_service"] != "filing-tracker-svc" ||
		env["actor_id"] != "p1" || env["correlation_id"] != "corr-1" || env["tenant_id"] != "tenant-ft" || env["filing_id"] != f.FilingID || tenant != "tenant-ft" {
		t.Fatalf("unexpected envelope: %s", payload)
	}
}

// The outbox row is part of the business transaction: if the event cannot be
// written the change rolls back too, and a failed change writes no event.
func TestPgStore_OutboxIsSameTransaction(t *testing.T) {
	admin := freshTestDB(t)
	s := store.NewPgStore(storePool(t, admin))
	ctx := middleware.WithTenant(context.Background(), "tenant-ft")
	bg := context.Background()

	// 1. Event insert fails -> no filing row either.
	if _, err := admin.Exec(bg, `ALTER TABLE outbox_events ADD CONSTRAINT test_reject CHECK (event_type <> 'filing.due')`); err != nil {
		t.Fatal(err)
	}
	f := newReq("2027Q1", "2027-04-30")
	if err := s.Create(ctx, f, meta); err == nil {
		t.Fatal("create must fail when its outbox event cannot be written")
	}
	if n := countFilings(t, admin); n != 0 {
		t.Fatalf("rolled-back create left %d filing rows", n)
	}
	if _, err := admin.Exec(bg, `ALTER TABLE outbox_events DROP CONSTRAINT test_reject`); err != nil {
		t.Fatal(err)
	}

	// 2. Business change fails (duplicate) -> no second event.
	if err := s.Create(ctx, newReq("2027Q1", "2027-04-30"), meta); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Create(ctx, newReq("2027Q1", "2027-04-30"), meta); !errors.Is(err, domain.ErrDuplicateRequirement) {
		t.Fatalf("expected duplicate, got %v", err)
	}
	var n int
	if err := admin.QueryRow(bg, `SELECT count(*) FROM outbox_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 outbox row, got %d", n)
	}

	// 3. SoD refusal under lock -> no state change, no event.
	g := newReq("2027Q2", "2027-07-31")
	if err := s.Create(ctx, g, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(ctx, g.FilingID, &domain.SubmitFilingRequest{SubmissionReference: "S", SubmittedBy: "p2"}, store.EventMeta{ActorID: "p2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(ctx, g.FilingID, &domain.ConfirmFilingRequest{ConfirmationReference: "A"}, store.EventMeta{ActorID: "p2"}); !errors.Is(err, domain.ErrSegregationOfDuties) {
		t.Fatalf("submitter confirming: expected ErrSegregationOfDuties, got %v", err)
	}
	if got, _ := s.GetByID(ctx, g.FilingID); got.Status != domain.StatusSubmitted {
		t.Fatalf("SoD refusal changed status to %s", got.Status)
	}
	if ev := outboxRows(t, admin, g.FilingID); len(ev) != 2 {
		t.Fatalf("SoD refusal must not enqueue an event, got %v", ev)
	}
}

type fakePublisher struct {
	err  error
	sent map[string][]byte
}

func (p *fakePublisher) PublishOutbox(_ context.Context, id, _ string, payload []byte) error {
	if p.err != nil {
		return p.err
	}
	if p.sent == nil {
		p.sent = map[string][]byte{}
	}
	p.sent[id] = payload
	return nil
}

// The relay publishes committed rows across tenants (app.outbox_relay), marks
// them published, and retries a failed delivery instead of dropping it.
func TestRelay_PublishesAndMarks(t *testing.T) {
	admin := freshTestDB(t)
	pool := storePool(t, admin)
	s := store.NewPgStore(pool)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := s.Create(middleware.WithTenant(context.Background(), tenant), newReq("2027Q3", "2027-10-31"), meta); err != nil {
			t.Fatalf("create %s: %v", tenant, err)
		}
	}
	bg := context.Background()

	failing := &fakePublisher{err: errors.New("broker down")}
	if n, err := outbox.NewRelay(pool, failing, 0, 0, zap.NewNop()).RelayOnce(bg); err != nil || n != 0 {
		t.Fatalf("failing relay: delivered=%d err=%v", n, err)
	}
	var attempts, pending int
	if err := admin.QueryRow(bg, `SELECT COALESCE(min(publish_attempts),0), count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&attempts, &pending); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || pending != 2 {
		t.Fatalf("after failed pass: attempts=%d pending=%d, want 1 and 2", attempts, pending)
	}

	ok := &fakePublisher{}
	relay := outbox.NewRelay(pool, ok, 0, 0, zap.NewNop())
	if n, err := relay.RelayOnce(bg); err != nil || n != 2 {
		t.Fatalf("relay: delivered=%d err=%v, want 2 (both tenants)", n, err)
	}
	if err := admin.QueryRow(bg, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d rows still unpublished", pending)
	}
	if n, err := relay.RelayOnce(bg); err != nil || n != 0 {
		t.Fatalf("second pass must deliver nothing: delivered=%d err=%v", n, err)
	}
	for id, payload := range ok.sent {
		var env map[string]any
		if err := json.Unmarshal(payload, &env); err != nil || env["event_id"] != id || env["schema_version"] != "1.0" {
			t.Fatalf("published payload for %s: %s (%v)", id, payload, err)
		}
	}
}

// Under FORCE RLS (app role), a tenant transaction sees only its own outbox
// rows and cannot insert another tenant's.
func TestOutbox_RLSIsolation(t *testing.T) {
	admin := freshTestDB(t)
	app := appPool(t, admin)
	if app == nil {
		t.Skip("TEST_APP_DATABASE_URL not set — RLS needs a NOSUPERUSER NOBYPASSRLS role")
	}
	s := store.NewPgStore(app)
	if err := s.Create(middleware.WithTenant(context.Background(), "tenant-a"), newReq("2027Q4", "2027-12-31"), meta); err != nil {
		t.Fatal(err)
	}
	bg := context.Background()
	tx, err := app.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(bg) }()
	if _, err := tx.Exec(bg, `SELECT set_config('app.tenant_id', 'tenant-b', true)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(bg, `SELECT count(*) FROM outbox_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("tenant-b sees %d of tenant-a's outbox rows", n)
	}
	err = outbox.Insert(bg, tx, outbox.Event{AggregateType: "x", AggregateID: "x", EventType: "x", TenantID: "tenant-a", Payload: map[string]string{}})
	if err == nil {
		t.Fatal("tenant-b inserted a tenant-a outbox row")
	}
}
