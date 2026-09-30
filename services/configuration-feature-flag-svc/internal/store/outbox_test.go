package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// Transactional-outbox tests (migration 000003).
//
// These assert the property the outbox exists for: a change that was recorded
// always has its event, and an event that exists always has its change. Before
// it, the handler wrote to Kafka after the store had committed and logged the
// error if it failed — so a broker hiccup answered 201 with the new value
// recorded and every consumer still reading the one it superseded. That stale
// value is valid data, so nothing downstream could detect it either.

// pendingByType counts the unpublished events of one type. The suite's real
// transitions now enqueue two events each — the updated event plus the
// snapshot.published the AA-001 write path mints — so a total depth count
// cannot express "exactly one update event", only a per-type count can.
func pendingByType(ctx context.Context, pool *pgxpool.Pool, eventType string) int {
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM event_outbox WHERE published_at IS NULL AND event_type = $1`,
		eventType).Scan(&n); err != nil {
		return -1
	}
	return n
}

// The central rule. A real transition enqueues exactly one event; re-asserting
// a value that is already in force enqueues none, because nothing happened.
func TestUpsert_EnqueuesOnlyOnRealTransition(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")

	params := domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Value: []byte(`100`), Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
		CorrelationID: "corr-1",
	}

	// 1. First write for this scope — a real transition.
	if _, created, err := s.UpsertConfigEntry(ctx, params); err != nil || !created {
		t.Fatalf("first write: created=%v err=%v", created, err)
	}
	if got := pendingByType(ctx, pool, "config.updated"); got != 1 {
		t.Fatalf("first write must enqueue exactly one config.updated, got %d", got)
	}

	// 2. The same value again — the idempotent path. Nothing was written, so
	// nothing happened, so no event. A consumer invalidating its cache here
	// would be doing it for a change that did not occur.
	if _, created, err := s.UpsertConfigEntry(ctx, params); err != nil || created {
		t.Fatalf("idempotent repeat: created=%v err=%v", created, err)
	}
	if got := pendingByType(ctx, pool, "config.updated"); got != 1 {
		t.Fatalf("an idempotent repeat must enqueue nothing, backlog went to %d", got)
	}

	// 3. A genuinely different value — a real transition again.
	changed := params
	changed.Value = []byte(`250`)
	if _, created, err := s.UpsertConfigEntry(ctx, changed); err != nil || !created {
		t.Fatalf("changed write: created=%v err=%v", created, err)
	}
	if got := pendingByType(ctx, pool, "config.updated"); got != 2 {
		t.Fatalf("a changed value must enqueue a second config.updated, got %d", got)
	}
}

func TestUpsertFeatureFlag_EnqueuesOnlyOnRealTransition(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedFlag(t, pool, "new_ui")

	params := domain.UpsertFeatureFlagParams{
		Key: "new_ui", Enabled: true, Environment: "staging", RolloutPercentage: 50,
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}
	if _, _, err := s.UpsertFeatureFlag(ctx, params); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, created, err := s.UpsertFeatureFlag(ctx, params); err != nil || created {
		t.Fatalf("idempotent repeat: created=%v err=%v", created, err)
	}
	if got := pendingByType(ctx, pool, "feature_flag.updated"); got != 1 {
		t.Fatalf("expected exactly one enqueued feature_flag.updated, got %d", got)
	}
}

// A global write has no scope tenant, but it is still made BY someone. The
// outbox row hangs off that someone — deriving the RLS session from the SCOPE
// instead would leave a global write unscoped, and under FORCE ROW LEVEL
// SECURITY its own outbox row would be invisible to the policy guarding it and
// refused at INSERT, failing the whole write at commit.
func TestUpsert_GlobalScopeWriteStillEnqueues(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "cutoff.hour")

	if _, created, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
		Key: "cutoff.hour", Value: []byte(`17`), Environment: "prod",
		TenantID:             nil, // the environment-wide default
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil || !created {
		t.Fatalf("global write: created=%v err=%v", created, err)
	}

	if got := pendingByType(ctx, pool, "config.updated"); got != 1 {
		t.Fatalf("a global write must enqueue its config.updated too, got %d", got)
	}

	var owner string
	if err := pool.QueryRow(ctx, `SELECT tenant_id FROM event_outbox LIMIT 1`).Scan(&owner); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if owner != testCallerTenant {
		t.Errorf("outbox row must be owned by the CALLER (%s), got %q", testCallerTenant, owner)
	}
}

// A write that cannot say who made it is refused rather than defaulted: the
// alternative is an append-only row and an event that name no owner.
func TestUpsert_RefusesAWriteWithNoCallerTenant(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	_, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
		Key: "k", Value: []byte(`1`), Environment: "staging", CreatedByPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrCallerTenantMissing) {
		t.Fatalf("expected ErrCallerTenantMissing, got %v", err)
	}
	if _, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{
		Key: "f", Enabled: true, Environment: "staging", CreatedByPrincipalID: "admin-1",
	}); !errors.Is(err, domain.ErrCallerTenantMissing) {
		t.Fatalf("expected ErrCallerTenantMissing, got %v", err)
	}
}

func TestClaimOutbox_MarksPublishedOnlyWhenTheHandlerSucceeds(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "k")

	if _, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
		Key: "k", Value: []byte(`1`), Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A failing publish must leave the rows claimable. Marking them published
	// here is the exact loss the outbox removes.
	sendErr := errors.New("broker unreachable")
	if err := s.ClaimOutbox(ctx, 10, func(recs []store.OutboxRecord) error {
		if len(recs) != 2 {
			t.Fatalf("expected 2 claimed records (config.updated + snapshot.published), got %d", len(recs))
		}
		return sendErr
	}); !errors.Is(err, sendErr) {
		t.Fatalf("expected the publish error to propagate, got %v", err)
	}

	if got := pendingByType(ctx, pool, "config.updated"); got != 1 {
		t.Fatalf("a failed publish must leave the event unpublished, got %d", got)
	}

	// The attempt and the reason are recorded on the row, so a stuck event can
	// be diagnosed from the table without correlating against logs.
	var attempts int
	var lastErr *string
	if err := pool.QueryRow(ctx, `SELECT attempts, last_error FROM event_outbox LIMIT 1`).Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if attempts != 1 {
		t.Errorf("expected attempts=1 after one failure, got %d", attempts)
	}
	if lastErr == nil || *lastErr == "" {
		t.Error("a failed publish must record why on the row")
	}

	// And a successful one drains them.
	if err := s.ClaimOutbox(ctx, 10, func(recs []store.OutboxRecord) error {
		if len(recs) != 2 {
			t.Fatalf("the failed events must be claimable again, got %d", len(recs))
		}
		if recs[0].EventType != "config.updated" {
			t.Errorf("unexpected first event type %q", recs[0].EventType)
		}
		if len(recs[0].Body) == 0 {
			t.Error("claimed record carries no envelope")
		}
		return nil
	}); err != nil {
		t.Fatalf("second drain: %v", err)
	}

	if got := pendingByType(ctx, pool, "config.updated"); got != 0 {
		t.Fatalf("a successful publish must clear the backlog, got %d", got)
	}
}

// Depth alone cannot tell a busy moment from a stalled relay, which is why the
// alert rule reads the age.
func TestOutboxDepth_ReportsAgeOfTheOldestUnpublishedEvent(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "k")

	if pending, age, err := s.OutboxDepth(ctx); err != nil || pending != 0 || age != 0 {
		t.Fatalf("empty outbox: pending=%d age=%v err=%v", pending, age, err)
	}

	if _, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
		Key: "k", Value: []byte(`1`), Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pending, age, err := s.OutboxDepth(ctx)
	if err != nil {
		t.Fatalf("outbox depth: %v", err)
	}
	if pending != 2 {
		t.Fatalf("expected backlog of 2 (config.updated + snapshot.published), got %d", pending)
	}
	if age <= 0 || age > time.Minute {
		t.Errorf("age of a just-written event should be small and positive, got %v", age)
	}
}

// The relay drains every tenant's backlog from one loop, so it names itself
// rather than running unscoped. Under FORCE ROW LEVEL SECURITY an unscoped
// relay sees nothing and presents as one that publishes nothing while reporting
// no error at all.
func TestClaimOutbox_RelayCrossesTenants(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	appPool := appRolePool(t, admin)
	s := store.New(appPool, zap.NewNop())
	seedConfig(t, admin, "k")

	tenantA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	tenantB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	for _, tenant := range []string{tenantA, tenantB} {
		if _, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
			Key: "k", Value: []byte(`1`), Environment: "staging", TenantID: &tenant,
			CreatedByPrincipalID: "admin-1", CallerTenantID: tenant,
		}); err != nil {
			t.Fatalf("seed %s: %v", tenant, err)
		}
	}

	claimed := 0
	if err := s.ClaimOutbox(ctx, 10, func(recs []store.OutboxRecord) error {
		for _, r := range recs {
			if r.EventType == "config.updated" {
				claimed++
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != 2 {
		t.Fatalf("the relay must see both tenants' update events, claimed %d", claimed)
	}
}
