package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/hris-connector-svc/internal/domain"
	"zoiko.io/hris-connector-svc/internal/events"
	"zoiko.io/hris-connector-svc/internal/middleware"
	"zoiko.io/hris-connector-svc/internal/outbox"
	"zoiko.io/hris-connector-svc/internal/store"
)

type fakeOutboxPublisher struct {
	mu   sync.Mutex
	fail bool
	sent map[string][]byte // aggregate id -> payload
}

func (f *fakeOutboxPublisher) PublishOutbox(_ context.Context, _, aggregateID string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("broker unavailable")
	}
	if f.sent == nil {
		f.sent = map[string][]byte{}
	}
	f.sent[aggregateID] = payload
	return nil
}

func countRows(t *testing.T, admin *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return n
}

// The event row is written in the same transaction as the integration and
// the sync job, carries the existing envelope (type, tenant, legal entity,
// actor, schema_version 1.0, payload), and is confined to its tenant by RLS.
func TestOutbox_WrittenWithTheChange(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := middleware.WithTenant(context.Background(), "tenant-ob-a")

	integ := &domain.HrisIntegration{LegalEntityID: "le-1", ProviderName: "WORKDAY", ApiEndpoint: "https://x", Status: domain.SyncPending}
	if err := s.CreateIntegration(ctx, integ, events.PublishParams{
		EventType: "hris.integration.created", TenantID: "forged-tenant", LegalEntityID: "le-1", ActorID: "p-1", CorrelationID: "c-1", Payload: integ,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	job := &domain.SyncJob{IntegrationID: integ.IntegrationID, SyncType: "ROSTER", Status: domain.SyncCompleted}
	if err := s.CreateSyncJob(ctx, job, events.PublishParams{EventType: "hris.sync.triggered", LegalEntityID: "le-1", ActorID: "p-1", Payload: job}); err != nil {
		t.Fatalf("job: %v", err)
	}

	rows, err := admin.Query(context.Background(),
		`SELECT aggregate_type, aggregate_id, event_type, tenant_id, payload FROM outbox_events ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var aggType, aggID, typ, tenant string
		var payload []byte
		if err := rows.Scan(&aggType, &aggID, &typ, &tenant, &payload); err != nil {
			t.Fatal(err)
		}
		var env events.Event
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatalf("payload is not the event envelope: %v", err)
		}
		if tenant != "tenant-ob-a" || env.TenantID != "tenant-ob-a" {
			t.Errorf("%s: tenant %q / envelope %q, want the verified tenant", typ, tenant, env.TenantID)
		}
		if env.SchemaVersion != "1.0" || env.EventType != typ || env.AggregateID != aggID || env.SourceService != "hris-connector-svc" {
			t.Errorf("envelope %+v does not match row (%s %s)", env, typ, aggID)
		}
		got = append(got, aggType+":"+typ)
	}
	if len(got) != 2 || got[0] != "hris_integration:hris.integration.created" || got[1] != "sync_job:hris.sync.triggered" {
		t.Fatalf("outbox rows %v", got)
	}

	// RLS: another tenant's session sees none of these rows.
	app := appRolePool(t, admin)
	tx, err := app.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', 'tenant-ob-b', true)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(context.Background(), "SELECT count(*) FROM outbox_events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("tenant-ob-b sees %d outbox rows of tenant-ob-a", n)
	}
}

// Rolling back leaves neither the business row nor the event: a failed
// business insert writes no event, and a failed event write undoes the row.
func TestOutbox_RollbackLeavesNeither(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := middleware.WithTenant(context.Background(), "tenant-ob-rb")

	// The event cannot be encoded: the outbox insert fails after the row.
	bad := &domain.HrisIntegration{IntegrationID: "integ-rb-1", LegalEntityID: "le-1", ProviderName: "WORKDAY", ApiEndpoint: "https://x"}
	if err := s.CreateIntegration(ctx, bad, events.PublishParams{EventType: "hris.integration.created", Payload: make(chan int)}); err == nil {
		t.Fatal("expected the unencodable event to fail the write")
	}
	if n := countRows(t, admin, "SELECT count(*) FROM hris_integrations WHERE integration_id = 'integ-rb-1'"); n != 0 {
		t.Fatalf("integration row survived a failed event write: %d", n)
	}

	// The business insert fails (duplicate id): no event is written.
	ok := &domain.HrisIntegration{IntegrationID: "integ-rb-2", LegalEntityID: "le-1", ProviderName: "WORKDAY", ApiEndpoint: "https://x"}
	if err := s.CreateIntegration(ctx, ok, events.PublishParams{EventType: "hris.integration.created", Payload: ok}); err != nil {
		t.Fatal(err)
	}
	dup := &domain.HrisIntegration{IntegrationID: "integ-rb-2", LegalEntityID: "le-1", ProviderName: "WORKDAY", ApiEndpoint: "https://x"}
	if err := s.CreateIntegration(ctx, dup, events.PublishParams{EventType: "hris.integration.created", Payload: dup}); err == nil {
		t.Fatal("expected duplicate id to fail")
	}
	if n := countRows(t, admin, "SELECT count(*) FROM outbox_events WHERE aggregate_id IN ('integ-rb-1','integ-rb-2')"); n != 1 {
		t.Fatalf("outbox rows %d, want exactly the one committed event", n)
	}
}

// The relay, running as the non-bypass app role, drains every tenant's rows
// (app.outbox_relay), publishes the stored envelope, marks rows published,
// and on a broker failure keeps the row for retry with the attempt counted.
func TestOutbox_RelayPublishesAndRetries(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	s := store.NewPgStore(app)
	for _, tenant := range []string{"tenant-relay-a", "tenant-relay-b"} {
		ctx := middleware.WithTenant(context.Background(), tenant)
		integ := &domain.HrisIntegration{IntegrationID: "integ-" + tenant, LegalEntityID: "le-1", ProviderName: "WORKDAY", ApiEndpoint: "https://x"}
		if err := s.CreateIntegration(ctx, integ, events.PublishParams{EventType: "hris.integration.created", Payload: integ}); err != nil {
			t.Fatal(err)
		}
	}

	failing := &fakeOutboxPublisher{fail: true}
	if n := outbox.NewRelay(app, failing, 0, 0, zap.NewNop()).RelayOnce(context.Background()); n != 0 {
		t.Fatalf("delivered %d with the broker down", n)
	}
	if n := countRows(t, admin, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND publish_attempts = 1"); n != 2 {
		t.Fatalf("failed rows kept for retry: %d, want 2", n)
	}

	pub := &fakeOutboxPublisher{}
	if n := outbox.NewRelay(app, pub, 0, 0, zap.NewNop()).RelayOnce(context.Background()); n != 2 {
		t.Fatalf("delivered %d, want both tenants' rows", n)
	}
	if n := countRows(t, admin, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL"); n != 0 {
		t.Fatalf("%d rows left unpublished", n)
	}
	var env events.Event
	if err := json.Unmarshal(pub.sent["integ-tenant-relay-a"], &env); err != nil || env.EventType != "hris.integration.created" || env.TenantID != "tenant-relay-a" {
		t.Fatalf("published message %s (%v)", pub.sent["integ-tenant-relay-a"], err)
	}
	if n := outbox.NewRelay(app, pub, 0, 0, zap.NewNop()).RelayOnce(context.Background()); n != 0 {
		t.Fatalf("re-delivered %d already published rows", n)
	}
}
