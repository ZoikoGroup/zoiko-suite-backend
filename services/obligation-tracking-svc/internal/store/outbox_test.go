package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/obligation-tracking-svc/internal/domain"
	"zoiko.io/obligation-tracking-svc/internal/middleware"
	"zoiko.io/obligation-tracking-svc/internal/outbox"
	"zoiko.io/obligation-tracking-svc/internal/store"
)

func outboxTypes(t *testing.T, pool *pgxpool.Pool, aggregateID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, outbox_event_id`, aggregateID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func asActor(tenantID, actor string) context.Context {
	return store.WithAudit(middleware.WithTenant(context.Background(), tenantID), store.Audit{ActorID: actor, CorrelationID: "corr-1"})
}

// Every write emits exactly its event in its own transaction, with the
// envelope the direct publish used to send.
func TestOutbox_EveryWriteEmitsItsEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")

	o := newObligation("le-us", "drafter-1")
	o.ExtractedByAI = true
	if err := s.CreateObligation(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.UpdateObligation(ctx, o); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.ValidateExtractedObligation(ctx, o.ObligationID, "checker-1"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := s.Schedule(ctx, o.ObligationID, &domain.ScheduleObligationRequest{TriggerDescription: "x", CalculationMethod: "y"}); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := s.MarkDue(ctx, o.ObligationID, "owner"); err != nil {
		t.Fatalf("due: %v", err)
	}
	if _, err := s.MarkInProgress(ctx, o.ObligationID, "owner"); err != nil {
		t.Fatalf("in progress: %v", err)
	}
	if _, err := s.Dispute(ctx, o.ObligationID, &domain.DisputeObligationRequest{DisputeReason: "r"}); err != nil {
		t.Fatalf("dispute: %v", err)
	}
	want := []string{"obligation.created", "obligation.updated", "obligation.validated", "obligation.activated",
		"obligation.due", "obligation.in_progress", "obligation.disputed"}
	if got := outboxTypes(t, pool, o.ObligationID); !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}

	// DISPUTED is final, so supersede is exercised on a second, live obligation.
	o2 := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(ctx, o2); err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := s.Supersede(ctx, o2.ObligationID, "obl-next"); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if got, want := outboxTypes(t, pool, o2.ObligationID), []string{"obligation.created", "obligation.superseded"}; !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}

	var id string
	var payload []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT outbox_event_id::text, payload FROM outbox_events WHERE aggregate_id=$1 AND event_type='obligation.created'`,
		o.ObligationID).Scan(&id, &payload); err != nil {
		t.Fatalf("read event: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if env["event_id"] != "evt-"+id || env["schema_version"] != "1.0" || env["obligation_id"] != o.ObligationID ||
		env["tenant_id"] != "tenant-a" || env["actor_id"] != "test-actor" || env["correlation_id"] != "corr-test" ||
		env["source_service"] != "obligation-tracking-svc" {
		t.Fatalf("unexpected envelope: %v", env)
	}
}

// Actor columns record the attributed principal, never the body field.
func TestActorColumns_RecordThePrincipal(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")
	o := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.Schedule(asActor("tenant-a", "scheduler-1"), o.ObligationID, &domain.ScheduleObligationRequest{TriggerDescription: "x", CalculationMethod: "y", ScheduledBy: "forged"})
	if err != nil || got.ScheduledBy == nil || *got.ScheduledBy != "scheduler-1" {
		t.Fatalf("scheduled_by = %v (%v), want scheduler-1", got, err)
	}
	got, err = s.RecordBreach(asActor("tenant-a", "breacher-1"), o.ObligationID, &domain.RecordBreachRequest{BreachedBy: "forged"})
	if err != nil || got.BreachedBy == nil || *got.BreachedBy != "breacher-1" {
		t.Fatalf("breached_by wrong (%v)", err)
	}
	reread, err := s.GetObligation(ctx, o.ObligationID)
	if err != nil || *reread.ScheduledBy != "scheduler-1" || *reread.BreachedBy != "breacher-1" {
		t.Fatalf("persisted actors wrong: %+v (%v)", reread, err)
	}

	o2 := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(ctx, o2); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Schedule(ctx, o2.ObligationID, &domain.ScheduleObligationRequest{TriggerDescription: "x", CalculationMethod: "y"}); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	waived, err := s.Waive(asActor("tenant-a", "waiver-1"), o2.ObligationID, &domain.WaiveObligationRequest{WaiverAuthorityReference: "ref", WaivedBy: "forged"})
	if err != nil || *waived.WaivedBy != "waiver-1" {
		t.Fatalf("waived_by wrong (%v)", err)
	}
	o3 := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(ctx, o3); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Schedule(ctx, o3.ObligationID, &domain.ScheduleObligationRequest{TriggerDescription: "x", CalculationMethod: "y"}); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	disputed, err := s.Dispute(asActor("tenant-a", "disputer-1"), o3.ObligationID, &domain.DisputeObligationRequest{DisputedBy: "forged", DisputeReason: "r"})
	if err != nil || *disputed.DisputedBy != "disputer-1" {
		t.Fatalf("disputed_by wrong (%v)", err)
	}
}

// Waive and validate are refused on the locked row when the deciding
// principal created the obligation; nothing is written or emitted.
func TestSegregationOfDuties_WaiveAndValidate(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	creator := asActor("tenant-a", "creator-1")

	o := newObligation("le-us", "creator-1")
	if err := s.CreateObligation(creator, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Schedule(creator, o.ObligationID, &domain.ScheduleObligationRequest{TriggerDescription: "x", CalculationMethod: "y"}); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := s.Waive(creator, o.ObligationID, &domain.WaiveObligationRequest{WaiverAuthorityReference: "ref"}); !errors.Is(err, domain.ErrSegregationOfDuties) {
		t.Fatalf("self-waive returned %v, want ErrSegregationOfDuties", err)
	}
	if got, _ := s.GetObligation(creator, o.ObligationID); got.Status != domain.ObligationStatusActive {
		t.Fatalf("status after refused waive = %s", got.Status)
	}
	if n := len(outboxTypes(t, pool, o.ObligationID)); n != 2 {
		t.Fatalf("refused waive emitted: %d rows", n)
	}

	c := newObligation("le-us", "creator-1")
	c.ExtractedByAI = true
	if err := s.CreateObligation(creator, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ValidateExtractedObligation(creator, c.ObligationID, "creator-1"); !errors.Is(err, domain.ErrSegregationOfDuties) {
		t.Fatalf("self-validate returned %v, want ErrSegregationOfDuties", err)
	}
	if _, err := s.ValidateExtractedObligation(asActor("tenant-a", "checker-1"), c.ObligationID, "checker-1"); err != nil {
		t.Fatalf("validate by another principal: %v", err)
	}
}

// A write with no attributed actor is refused, not emitted unattributed.
func TestOutbox_WriteWithoutAudit_IsRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	if err := s.CreateObligation(ctx, newObligation("le-us", "drafter-1")); !errors.Is(err, store.ErrAuditMissing) {
		t.Fatalf("create without audit returned %v, want ErrAuditMissing", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM obligations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("obligations = %d (%v), want 0", n, err)
	}
}

// If the event cannot be written the transition rolls back too.
func TestOutbox_SameTransaction_RollbackLeavesNeither(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	ctx := tenantCtx("tenant-a")
	o := newObligation("le-us", "drafter-1")
	if err := s.CreateObligation(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`ALTER TABLE outbox_events ADD CONSTRAINT test_block CHECK (event_type <> 'obligation.activated')`); err != nil {
		t.Fatalf("add blocking constraint: %v", err)
	}
	if _, err := s.Schedule(ctx, o.ObligationID, &domain.ScheduleObligationRequest{TriggerDescription: "x", CalculationMethod: "y"}); err == nil {
		t.Fatal("schedule succeeded although its event could not be written")
	}
	got, err := s.GetObligation(ctx, o.ObligationID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.ObligationStatusPlanned || got.ScheduledBy != nil {
		t.Fatalf("transition committed without its event: %s", got.Status)
	}
	if n := len(outboxTypes(t, pool, o.ObligationID)); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}

type fakePub struct{ fail bool }

func (p *fakePub) PublishOutbox(context.Context, string, string, []byte) error {
	if p.fail {
		return errors.New("broker down")
	}
	return nil
}

func TestOutboxRelay_DeliversAndRetries(t *testing.T) {
	pool := openTestPool(t)
	s := store.NewPgStore(pool)
	if err := s.CreateObligation(tenantCtx("tenant-a"), newObligation("le-us", "drafter-1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	pub := &fakePub{fail: true}
	relay := outbox.NewRelay(pool, pub, time.Second, 50, zap.NewNop())
	if n := relay.RelayOnce(context.Background()); n != 0 {
		t.Fatalf("broker down: delivered %d", n)
	}
	var attempts, unpublished int
	if err := pool.QueryRow(context.Background(),
		`SELECT max(publish_attempts), count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&attempts, &unpublished); err != nil {
		t.Fatalf("read: %v", err)
	}
	if attempts != 1 || unpublished != 1 {
		t.Fatalf("attempts=%d unpublished=%d, want 1/1", attempts, unpublished)
	}
	pub.fail = false
	if n := relay.RelayOnce(context.Background()); n != 1 {
		t.Fatalf("delivered %d, want 1", n)
	}
	if n := relay.RelayOnce(context.Background()); n != 0 {
		t.Fatalf("published rows re-sent: %d", n)
	}
}

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_APP_DATABASE_URL not set: needs an ordinary role for RLS")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect app role: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// As a NOSUPERUSER NOBYPASSRLS role: an unscoped session sees no outbox row,
// the relay drains every tenant, a cross-tenant insert is refused.
func TestOutboxRLS_AsAppRole(t *testing.T) {
	_ = openTestPool(t)
	app := appPool(t)
	s := store.NewPgStore(app)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := s.CreateObligation(tenantCtx(tenant), newObligation("le-us", "drafter-1")); err != nil {
			t.Fatalf("create as app role (%s): %v", tenant, err)
		}
	}
	var visible int
	if err := app.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&visible); err != nil {
		t.Fatalf("count: %v", err)
	}
	if visible != 0 {
		t.Fatalf("unscoped app session sees %d outbox rows, want 0", visible)
	}
	if n := outbox.NewRelay(app, &fakePub{}, time.Second, 50, zap.NewNop()).RelayOnce(context.Background()); n != 2 {
		t.Fatalf("relay delivered %d, want 2", n)
	}
	tx, err := app.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'tenant-a', true)`); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if _, err := outbox.Insert(context.Background(), tx, outbox.Event{
		AggregateType: "obligation", AggregateID: "x", EventType: "Forged", TenantID: "tenant-b", Payload: map[string]string{},
	}); err == nil {
		t.Fatal("cross-tenant outbox insert was accepted")
	}
}
