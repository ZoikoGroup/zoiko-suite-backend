package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/corporate-tax-svc/internal/domain"
	"zoiko.io/corporate-tax-svc/internal/middleware"
	"zoiko.io/corporate-tax-svc/internal/outbox"
	"zoiko.io/corporate-tax-svc/internal/store"
)

type fakePub struct {
	mu   sync.Mutex
	err  error
	sent []string // aggregate ids, in delivery order
	body [][]byte
}

func (f *fakePub) PublishOutbox(_ context.Context, _ string, aggregateID string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, aggregateID)
	f.body = append(f.body, payload)
	return nil
}

func newObj(tag string) *domain.TaxReturn {
	return &domain.TaxReturn{LegalEntityID: "le-" + tag, JurisdictionID: "GB", FiscalYear: 2026,
		AccountingPeriodStart: "2025-04-01", AccountingPeriodEnd: "2026-03-31", GrossRevenue: 100,
		TaxRatePercent: 25, Currency: "GBP", EffectiveFrom: "2026-04-01", CreatedBy: "p1"}
}

func create(s *store.PgStore, ctx context.Context, r *domain.TaxReturn, meta store.EventMeta) error {
	return s.Create(ctx, r, meta)
}

func objID(r *domain.TaxReturn) string { return r.ReturnID }

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// A create and its event commit together, and the event row
// carries the request's actor, correlation and the platform envelope.
func TestOutbox_CreateWritesEventInSameTransaction(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-ob")
	s := store.NewPgStore(pool)
	r := newObj("VAT-OB")
	if err := create(s, ctx, r, store.EventMeta{ActorID: "p1", CorrelationID: "corr-1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	var eventType, actor, corr, tenant string
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT event_type, actor_id, correlation_id, tenant_id, payload
		FROM outbox_events WHERE aggregate_id = $1`, objID(r)).Scan(&eventType, &actor, &corr, &tenant, &payload); err != nil {
		t.Fatalf("outbox row: %v", err)
	}
	if eventType != store.EventTaxReturnCreated || actor != "p1" || corr != "corr-1" || tenant != "tenant-ob" {
		t.Fatalf("unexpected outbox row: %s %s %s %s", eventType, actor, corr, tenant)
	}
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if env["schema_version"] != "1.0" || env["event_type"] != store.EventTaxReturnCreated || env["return_id"] != objID(r) {
		t.Fatalf("unexpected envelope: %v", env)
	}

	r.GrossRevenue = 200
	if err := s.Update(ctx, r, store.EventMeta{ActorID: "p1"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.Submit(ctx, objID(r), "p2", store.EventMeta{ActorID: "p2"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	req := &domain.AssessTaxReturnRequest{AssessedTaxAmount: 25, AssessmentReference: "REF"}
	if _, err := s.Assess(ctx, objID(r), req, "p3", store.EventMeta{ActorID: "p3"}); err != nil {
		t.Fatalf("assess: %v", err)
	}
	for _, et := range []string{store.EventTaxReturnUpdated, store.EventTaxReturnSubmitted, store.EventTaxReturnAssessed} {
		if n := countRows(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, objID(r), et); n != 1 {
			t.Fatalf("expected 1 %s event, got %d", et, n)
		}
	}
}

// If the outbox insert fails, the business change rolls back with it: neither
// the record nor an event exists. A trigger forces the failure.
func TestOutbox_FailedEventInsertRollsBackBusinessChange(t *testing.T) {
	pool := freshTestDB(t)
	if _, err := pool.Exec(context.Background(), `
		CREATE FUNCTION fail_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'outbox unavailable'; END $$;
		CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION fail_outbox();`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	ctx := middleware.WithTenant(context.Background(), "tenant-rb")
	s := store.NewPgStore(pool)
	if err := create(s, ctx, newObj("VAT-RB"), store.EventMeta{ActorID: "p1"}); err == nil {
		t.Fatal("expected create to fail when the outbox insert fails")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM corporate_tax_returns`); n != 0 {
		t.Fatalf("record must roll back with its event, found %d rows", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox_events`); n != 0 {
		t.Fatalf("expected no outbox rows, found %d", n)
	}
}

// The relay publishes pending rows, marks them published (not re-sent), and
// on a broker failure keeps the row and counts the attempt.
func TestOutbox_RelayPublishesAndRetries(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-relay")
	s := store.NewPgStore(pool)
	r := newObj("VAT-RELAY")
	if err := create(s, ctx, r, store.EventMeta{ActorID: "p1"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	failing := &fakePub{err: errors.New("broker down")}
	if n := outbox.NewRelay(pool, failing, time.Second, 50, zap.NewNop()).RelayOnce(context.Background()); n != 0 {
		t.Fatalf("failing broker delivered %d", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND publish_attempts = 1 AND last_error IS NOT NULL`); n != 1 {
		t.Fatalf("failed delivery must leave the row pending with one attempt recorded, got %d", n)
	}

	pub := &fakePub{}
	relay := outbox.NewRelay(pool, pub, time.Second, 50, zap.NewNop())
	if n := relay.RelayOnce(context.Background()); n != 1 {
		t.Fatalf("expected 1 delivered, got %d", n)
	}
	if len(pub.sent) != 1 || pub.sent[0] != objID(r) {
		t.Fatalf("event must be keyed by the aggregate id, got %v", pub.sent)
	}
	if n := relay.RelayOnce(context.Background()); n != 0 {
		t.Fatalf("published rows must not be re-sent, got %d", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL`); n != 1 {
		t.Fatalf("expected the row marked published, got %d", n)
	}
}

// Under FORCE RLS, as the ordinary service role: the store's tenant-scoped
// insert passes WITH CHECK, a session with no tenant and no relay flag sees
// nothing, a cross-tenant insert is refused, and the relay (app.outbox_relay)
// drains every tenant's backlog.
func TestOutbox_RLS_AppRole(t *testing.T) {
	_ = freshTestDB(t)
	app := appTestPool(t)
	s := store.NewPgStore(app)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := create(s, middleware.WithTenant(context.Background(), tenant), newObj("VAT-"+tenant), store.EventMeta{ActorID: "p1"}); err != nil {
			t.Fatalf("create as app role (%s): %v", tenant, err)
		}
	}
	if n := countRows(t, app, `SELECT count(*) FROM outbox_events`); n != 0 {
		t.Fatalf("no tenant and no relay flag: RLS must hide every outbox row, saw %d", n)
	}

	tx, err := app.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'tenant-a', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Insert(context.Background(), tx, outbox.Event{AggregateType: "x", AggregateID: "x",
		EventType: "Forged", TenantID: "tenant-b", Payload: map[string]string{}}); err == nil {
		t.Fatal("inserting an outbox row for another tenant must be refused")
	}
	_ = tx.Rollback(context.Background())

	pub := &fakePub{}
	if n := outbox.NewRelay(app, pub, time.Second, 50, zap.NewNop()).RelayOnce(context.Background()); n != 2 {
		t.Fatalf("relay must drain both tenants' events, delivered %d", n)
	}
}
