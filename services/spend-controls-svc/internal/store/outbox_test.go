package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	svcmiddleware "zoiko.io/spend-controls-svc/internal/middleware"
	"zoiko.io/spend-controls-svc/internal/outbox"
	"zoiko.io/spend-controls-svc/internal/store"
)

// storePool is the pool the store runs on: the ordinary app role when
// TEST_APP_DATABASE_URL names one (so FORCE RLS is really exercised), else the
// owner. Migrations always run as the owner (openTestPool).
func storePool(t *testing.T) (app, owner *pgxpool.Pool) {
	t.Helper()
	owner = openTestPool(t)
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		return owner, owner
	}
	requireThrowawayDatabase(t, dsn)
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect as app role: %v", err)
	}
	t.Cleanup(p.Close)
	return p, owner
}

func countRows(t *testing.T, owner *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// A fresh BLOCKED decision writes its two events in the decision's own
// transaction; an ALLOWED one and a replayed block write none.
func TestOutbox_FreshBlockWritesBothEvents_AllowedAndReplayWriteNone(t *testing.T) {
	app, owner := storePool(t)
	s := store.New(app)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	policy := seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 100)

	if _, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 50)); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, owner, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenantID); n != 0 {
		t.Fatalf("an allowed check must emit nothing, got %d rows", n)
	}

	ev := evaluation("le-1", "PROCUREMENT", "GBP", "corr-block", 5000)
	ev.EventCorrelationID = "req-corr-1"
	d, err := s.EvaluateSpend(ctx, ev)
	if err != nil || d.Outcome != "BLOCKED" {
		t.Fatalf("expected BLOCKED, got %+v %v", d, err)
	}
	if _, err := s.EvaluateSpend(ctx, ev); err != nil { // replay
		t.Fatal(err)
	}

	rows, err := owner.Query(context.Background(),
		`SELECT event_type, aggregate_id, correlation_id, actor_id, payload FROM outbox_events WHERE tenant_id = $1 ORDER BY event_type`, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var et, agg, corr, actor string
		var payload []byte
		if err := rows.Scan(&et, &agg, &corr, &actor, &payload); err != nil {
			t.Fatal(err)
		}
		types = append(types, et)
		if agg != policy.SpendPolicyID || corr != "req-corr-1" || actor != "principal-1" {
			t.Errorf("%s: aggregate=%s correlation=%s actor=%s", et, agg, corr, actor)
		}
		var env map[string]any
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatal(err)
		}
		if env["schema_version"] != "1.0" || env["event_type"] != et || env["tenant_id"] != tenantID || env["source_service"] != "spend-controls-svc" {
			t.Errorf("%s envelope: %v", et, env)
		}
	}
	if strings.Join(types, ",") != "spend.block.applied,spend.threshold.breached" {
		t.Fatalf("a fresh block (replayed once) must leave exactly the two events, got %v", types)
	}
}

// Same transaction: when the outbox write fails, the BLOCKED consumption is
// rolled back with it — neither exists.
func TestOutbox_InsertFailure_RollsBackTheDecision(t *testing.T) {
	app, owner := storePool(t)
	s := store.New(app)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 100)

	// Make the SECOND of the two inserts fail, so the first one is also
	// proven to roll back.
	if _, err := owner.Exec(context.Background(),
		`ALTER TABLE outbox_events ADD CONSTRAINT test_reject_block_applied CHECK (event_type <> 'spend.block.applied')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS test_reject_block_applied`)
	})

	corr := uuid.NewString()
	if _, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", corr, 5000)); err == nil {
		t.Fatal("a failed outbox write must fail the decision")
	}
	if n := countRows(t, owner, `SELECT count(*) FROM spend_consumptions WHERE correlation_id = $1`, corr); n != 0 {
		t.Fatalf("the BLOCKED consumption survived a failed outbox write (%d rows)", n)
	}
	if n := countRows(t, owner, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenantID); n != 0 {
		t.Fatalf("an outbox row survived the rolled-back decision (%d rows)", n)
	}
}

type capturePub struct {
	mu   sync.Mutex
	got  map[string][]byte
	fail error
}

func (c *capturePub) PublishOutbox(_ context.Context, id, _ string, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	if c.got == nil {
		c.got = map[string][]byte{}
	}
	c.got[id] = payload
	return nil
}

// The relay drains every tenant's rows (app.outbox_relay), keeps a row on a
// failed publish for retry, and marks delivered rows published.
func TestOutbox_RelayPublishesAcrossTenants_RetriesFailures(t *testing.T) {
	app, owner := storePool(t)
	s := store.New(app)
	for i := 0; i < 2; i++ {
		tenantID := uuid.NewString()
		ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
		seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 100)
		if _, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 5000)); err != nil {
			t.Fatal(err)
		}
	}

	pub := &capturePub{fail: errors.New("broker down")}
	relay := outbox.NewRelay(app, pub, time.Second, 100, zap.NewNop())
	if n := relay.RelayOnce(context.Background()); n != 0 {
		t.Fatalf("a failing broker delivered %d", n)
	}
	if n := countRows(t, owner, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND publish_attempts = 1`); n != 4 {
		t.Fatalf("failed rows must stay pending with one attempt recorded, got %d", n)
	}

	pub.fail = nil
	if n := relay.RelayOnce(context.Background()); n != 4 {
		t.Fatalf("relay delivered %d of 4 rows across two tenants", n)
	}
	if n := countRows(t, owner, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`); n != 0 {
		t.Fatalf("%d rows left unpublished", n)
	}
	if len(pub.got) != 4 {
		t.Fatalf("publisher saw %d events", len(pub.got))
	}
}

// Under the app role, an unscoped session sees no outbox rows and a tenant
// cannot write another tenant's row.
func TestOutbox_RLS_UnscopedSeesNothing_CrossTenantInsertRefused(t *testing.T) {
	if os.Getenv("TEST_APP_DATABASE_URL") == "" {
		t.Skip("TEST_APP_DATABASE_URL not set: needs an ordinary role for RLS")
	}
	app, _ := storePool(t)
	s := store.New(app)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	seedPolicy(t, s, ctx, tenantID, "le-1", "PROCUREMENT", "MONTHLY", "GBP", 100)
	if _, err := s.EvaluateSpend(ctx, evaluation("le-1", "PROCUREMENT", "GBP", uuid.NewString(), 5000)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := app.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unscoped: RLS must hide every outbox row, saw %d", n)
	}
	tx, err := app.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(context.Background(), `
		INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, tenant_id, legal_entity_id, payload)
		VALUES ('x', 'y', 'Forged', $1, 'le', '{}')`, uuid.NewString())
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("writing another tenant's outbox row must be refused by RLS, got %v", err)
	}
}
