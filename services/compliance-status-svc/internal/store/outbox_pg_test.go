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

	"zoiko.io/compliance-status-svc/internal/domain"
	"zoiko.io/compliance-status-svc/internal/middleware"
	"zoiko.io/compliance-status-svc/internal/outbox"
	"zoiko.io/compliance-status-svc/internal/store"
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

func storePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	if p := appPool(t, admin); p != nil {
		return p
	}
	return admin
}

func outboxTypes(t *testing.T, admin *pgxpool.Pool, aggregateID string) []string {
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

func newGap() *domain.ComplianceGap {
	return &domain.ComplianceGap{LegalEntityID: "le", JurisdictionID: "GB", DomainName: "TAX", GapType: "MISSING_EVIDENCE"}
}

var (
	maker   = store.EventMeta{ActorID: "maker", CorrelationID: "corr-1"}
	checker = store.EventMeta{ActorID: "checker", CorrelationID: "corr-2"}
)

func TestPgStore_WritesEnqueueOutboxEvents(t *testing.T) {
	admin := freshTestDB(t)
	s := store.NewPgStore(storePool(t, admin))
	ctx := middleware.WithTenant(context.Background(), "tenant-cs")

	c := &domain.ComplianceHealth{LegalEntityID: "le", JurisdictionID: "GB", DomainName: "TAX", TotalObligations: 1, FulfilledObligations: 1, CreatedBy: "maker"}
	if err := s.Evaluate(ctx, c, maker); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got := outboxTypes(t, admin, c.StatusID); len(got) != 1 || got[0] != store.EventStatusChanged {
		t.Fatalf("evaluate events: %v", got)
	}

	g := newGap()
	if err := s.CreateGap(ctx, g, maker); err != nil {
		t.Fatalf("create gap: %v", err)
	}
	if g.CreatedBy == nil || *g.CreatedBy != "maker" {
		t.Fatalf("created_by not recorded: %v", g.CreatedBy)
	}
	resolved, err := s.ResolveGap(ctx, g.GapID, &domain.ResolveGapRequest{RemediationNotes: "done"}, checker)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ResolvedBy == nil || *resolved.ResolvedBy != "checker" {
		t.Fatalf("resolved_by not recorded: %v", resolved.ResolvedBy)
	}
	got, err := s.GetGapByID(ctx, g.GapID)
	if err != nil || got.CreatedBy == nil || *got.CreatedBy != "maker" || got.ResolvedBy == nil || *got.ResolvedBy != "checker" {
		t.Fatalf("persisted actors: %+v %v", got, err)
	}
	if ev := outboxTypes(t, admin, g.GapID); len(ev) != 2 || ev[0] != store.EventGapDetected || ev[1] != store.EventGapResolved {
		t.Fatalf("gap events: %v", ev)
	}

	var payload []byte
	if err := admin.QueryRow(context.Background(), `SELECT payload FROM outbox_events WHERE aggregate_id=$1 AND event_type=$2`,
		g.GapID, store.EventGapResolved).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	if env["schema_version"] != "1.0" || env["event_type"] != store.EventGapResolved || env["source_service"] != "compliance-status-svc" ||
		env["actor_id"] != "checker" || env["correlation_id"] != "corr-2" || env["subject_id"] != g.GapID || env["tenant_id"] != "tenant-cs" {
		t.Fatalf("unexpected envelope: %s", payload)
	}
}

func TestPgStore_OutboxIsSameTransaction(t *testing.T) {
	admin := freshTestDB(t)
	s := store.NewPgStore(storePool(t, admin))
	ctx := middleware.WithTenant(context.Background(), "tenant-cs")
	bg := context.Background()

	// Event insert fails -> the gap is not stored either.
	if _, err := admin.Exec(bg, `ALTER TABLE outbox_events ADD CONSTRAINT test_reject CHECK (event_type <> 'compliance.gap.detected')`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGap(ctx, newGap(), maker); err == nil {
		t.Fatal("create gap must fail when its outbox event cannot be written")
	}
	var n int
	if err := admin.QueryRow(bg, `SELECT count(*) FROM compliance_gaps`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rolled-back create left %d gap rows", n)
	}
	if _, err := admin.Exec(bg, `ALTER TABLE outbox_events DROP CONSTRAINT test_reject`); err != nil {
		t.Fatal(err)
	}

	// Business change refused (SoD under lock) -> no state change, no event.
	g := newGap()
	if err := s.CreateGap(ctx, g, maker); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveGap(ctx, g.GapID, &domain.ResolveGapRequest{}, maker); !errors.Is(err, domain.ErrSegregationOfDuties) {
		t.Fatalf("creator resolving: expected ErrSegregationOfDuties, got %v", err)
	}
	if got, _ := s.GetGapByID(ctx, g.GapID); got.Status != domain.GapOpen || got.ResolvedBy != nil {
		t.Fatalf("SoD refusal changed the gap: %+v", got)
	}
	if ev := outboxTypes(t, admin, g.GapID); len(ev) != 1 {
		t.Fatalf("SoD refusal must not enqueue an event: %v", ev)
	}
	// Already resolved -> no second event.
	if _, err := s.ResolveGap(ctx, g.GapID, &domain.ResolveGapRequest{}, checker); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveGap(ctx, g.GapID, &domain.ResolveGapRequest{}, checker); !errors.Is(err, domain.ErrGapAlreadyResolved) {
		t.Fatalf("expected ErrGapAlreadyResolved, got %v", err)
	}
	if ev := outboxTypes(t, admin, g.GapID); len(ev) != 2 {
		t.Fatalf("expected 2 events, got %v", ev)
	}
}

// A gap logged before creators were recorded (created_by NULL) can still be
// resolved; there is no creator to compare.
func TestPgStore_ResolveLegacyGapWithoutCreator(t *testing.T) {
	admin := freshTestDB(t)
	s := store.NewPgStore(storePool(t, admin))
	ctx := middleware.WithTenant(context.Background(), "tenant-cs")
	g := newGap()
	if err := s.CreateGap(ctx, g, maker); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(context.Background(), `UPDATE compliance_gaps SET created_by = NULL WHERE gap_id = $1`, g.GapID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveGap(ctx, g.GapID, &domain.ResolveGapRequest{}, maker); err != nil {
		t.Fatalf("legacy gap resolve: %v", err)
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

func TestRelay_PublishesAndMarks(t *testing.T) {
	admin := freshTestDB(t)
	pool := storePool(t, admin)
	s := store.NewPgStore(pool)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := s.CreateGap(middleware.WithTenant(context.Background(), tenant), newGap(), maker); err != nil {
			t.Fatalf("create %s: %v", tenant, err)
		}
	}
	bg := context.Background()
	if n, err := outbox.NewRelay(pool, &fakePublisher{err: errors.New("broker down")}, 0, 0, zap.NewNop()).RelayOnce(bg); err != nil || n != 0 {
		t.Fatalf("failing relay: delivered=%d err=%v", n, err)
	}
	var attempts, pending int
	if err := admin.QueryRow(bg, `SELECT COALESCE(min(publish_attempts),0), count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&attempts, &pending); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || pending != 2 {
		t.Fatalf("after failed pass: attempts=%d pending=%d", attempts, pending)
	}
	ok := &fakePublisher{}
	relay := outbox.NewRelay(pool, ok, 0, 0, zap.NewNop())
	if n, err := relay.RelayOnce(bg); err != nil || n != 2 {
		t.Fatalf("relay: delivered=%d err=%v, want 2", n, err)
	}
	if n, err := relay.RelayOnce(bg); err != nil || n != 0 {
		t.Fatalf("second pass: delivered=%d err=%v", n, err)
	}
	for id, payload := range ok.sent {
		var env map[string]any
		if err := json.Unmarshal(payload, &env); err != nil || env["event_id"] != "evt-"+id || env["schema_version"] != "1.0" {
			t.Fatalf("published payload for %s: %s (%v)", id, payload, err)
		}
	}
}

func TestOutbox_RLSIsolation(t *testing.T) {
	admin := freshTestDB(t)
	app := appPool(t, admin)
	if app == nil {
		t.Skip("TEST_APP_DATABASE_URL not set — RLS needs a NOSUPERUSER NOBYPASSRLS role")
	}
	if err := store.NewPgStore(app).CreateGap(middleware.WithTenant(context.Background(), "tenant-a"), newGap(), maker); err != nil {
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
	if err := outbox.Insert(bg, tx, outbox.Event{AggregateType: "x", AggregateID: "x", EventType: "x", TenantID: "tenant-a", Payload: map[string]string{}}); err == nil {
		t.Fatal("tenant-b inserted a tenant-a outbox row")
	}
}
