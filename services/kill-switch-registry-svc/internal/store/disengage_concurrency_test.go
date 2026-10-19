package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/kill-switch-registry-svc/internal/domain"
	"zoiko.io/kill-switch-registry-svc/internal/events"
	svcmiddleware "zoiko.io/kill-switch-registry-svc/internal/middleware"
	"zoiko.io/kill-switch-registry-svc/internal/outbox"
	"zoiko.io/kill-switch-registry-svc/internal/store"
)

// These run against real PostgreSQL as the NOSUPERUSER NOBYPASSRLS test role
// (same harness as rls_test.go), because the property under test — two
// concurrent disengages of one scope cannot both succeed — is a property of
// real transaction behavior and cannot be shown with a mocked store.

func mkEvent(action domain.KillSwitchAction, dom string, tenant *string) (*domain.KillSwitchEvent, outbox.Event) {
	id := uuid.NewString()
	e := &domain.KillSwitchEvent{
		KillSwitchEventID: id, Domain: strp(dom), TenantID: tenant, Action: action,
		Reason: "concurrency test", ApprovedByPrincipalID: "approver-1",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "maker-1",
	}
	if action == domain.KillSwitchActionEngage {
		e.ReconciliationProcedureRef = strp("runbook:test")
	}
	et := events.EventTypeKillSwitchDisengaged
	if action == domain.KillSwitchActionEngage {
		et = events.EventTypeKillSwitchEngaged
	}
	return e, outbox.Event{AggregateType: "kill_switch", AggregateID: id, EventType: et, Payload: e, CorrelationID: "corr-" + id, TenantID: tenant}
}

func countRows(t *testing.T, admin *pgxpool.Pool, dom, action string) (events, outboxRows int) {
	t.Helper()
	ctx := context.Background()
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM kill_switch_events WHERE domain=$1 AND action=$2`, dom, action).Scan(&events); err != nil {
		t.Fatal(err)
	}
	et := "kill_switch.engaged"
	if action == "DISENGAGE" {
		et = "kill_switch.disengaged"
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM outbox_events o JOIN kill_switch_events k ON o.aggregate_id = k.kill_switch_event_id::text
		WHERE k.domain=$1 AND o.event_type=$2`, dom, et).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	return
}

func raceDisengage(s *store.PgStore, ctx context.Context, dom string, tenant *string, n int) (ok, conflict int, other []error) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, oe := mkEvent(domain.KillSwitchActionDisengage, dom, tenant)
			<-start
			err := s.DisengageIfEngaged(ctx, e, oe)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrNotCurrentlyEngaged):
				conflict++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	return
}

// widenRaceWindow makes every DISENGAGE insert pause briefly, which stretches
// the gap between "read the latest event" and "insert the new one". It does not
// change the service's behavior; it makes a missing serialization show up on
// every run instead of only when the scheduler happens to interleave.
func widenRaceWindow(t *testing.T, admin *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := admin.Exec(ctx, `
		CREATE OR REPLACE FUNCTION ks_test_slow_disengage() RETURNS trigger AS $$
		BEGIN
			IF NEW.action = 'DISENGAGE' THEN PERFORM pg_sleep(0.25); END IF;
			RETURN NEW;
		END; $$ LANGUAGE plpgsql;
		DROP TRIGGER IF EXISTS ks_test_slow_disengage ON kill_switch_events;
		CREATE TRIGGER ks_test_slow_disengage BEFORE INSERT ON kill_switch_events
			FOR EACH ROW EXECUTE FUNCTION ks_test_slow_disengage();`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DROP TRIGGER IF EXISTS ks_test_slow_disengage ON kill_switch_events; DROP FUNCTION IF EXISTS ks_test_slow_disengage();`)
	})
}

func TestDisengage_ConcurrentRequests_ExactlyOneTransition_PlatformWide(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()

	eng, engOut := mkEvent(domain.KillSwitchActionEngage, "RACE_PLATFORM", nil)
	if err := s.AppendEvent(ctx, eng, engOut); err != nil {
		t.Fatal(err)
	}
	widenRaceWindow(t, admin)

	ok, conflict, other := raceDisengage(s, ctx, "RACE_PLATFORM", nil, 12)
	if len(other) != 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if ok != 1 || conflict != 11 {
		t.Fatalf("expected exactly 1 success and 11 conflicts, got %d / %d", ok, conflict)
	}
	if ev, ob := countRows(t, admin, "RACE_PLATFORM", "DISENGAGE"); ev != 1 || ob != 1 {
		t.Fatalf("expected exactly 1 DISENGAGE row and 1 outbox event, got %d / %d", ev, ob)
	}
	if ev, ob := countRows(t, admin, "RACE_PLATFORM", "ENGAGE"); ev != 1 || ob != 1 {
		t.Fatalf("the ENGAGE history must be untouched, got %d / %d", ev, ob)
	}
	latest, err := s.LatestEventForScope(ctx, nil, strp("RACE_PLATFORM"), nil, nil)
	if err != nil || latest == nil || latest.Action != domain.KillSwitchActionDisengage {
		t.Fatalf("latest event must be the single DISENGAGE, got %+v err=%v", latest, err)
	}
}

func TestDisengage_ConcurrentRequests_ExactlyOneTransition_TenantScoped(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := svcmiddleware.WithTenant(context.Background(), tenantA)

	eng, engOut := mkEvent(domain.KillSwitchActionEngage, "RACE_TENANT", strp(tenantA))
	if err := s.AppendEvent(ctx, eng, engOut); err != nil {
		t.Fatal(err)
	}
	widenRaceWindow(t, admin)
	ok, conflict, other := raceDisengage(s, ctx, "RACE_TENANT", strp(tenantA), 12)
	if len(other) != 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if ok != 1 || conflict != 11 {
		t.Fatalf("expected exactly 1 success and 11 conflicts, got %d / %d", ok, conflict)
	}
	if ev, ob := countRows(t, admin, "RACE_TENANT", "DISENGAGE"); ev != 1 || ob != 1 {
		t.Fatalf("expected exactly 1 DISENGAGE row and 1 outbox event, got %d / %d", ev, ob)
	}
}

// The lock is per scope tuple and per transaction: it does not stick, and it
// does not make unrelated scopes wait on or refuse each other.
func TestDisengage_LockIsNotStickyAndIsPerScope(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()

	for _, dom := range []string{"SCOPE_ONE", "SCOPE_TWO"} {
		e, o := mkEvent(domain.KillSwitchActionEngage, dom, nil)
		if err := s.AppendEvent(ctx, e, o); err != nil {
			t.Fatal(err)
		}
	}
	// Disengage both scopes at the same time: both must succeed.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, dom := range []string{"SCOPE_ONE", "SCOPE_TWO"} {
		i, dom := i, dom
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, o := mkEvent(domain.KillSwitchActionDisengage, dom, nil)
			errs[i] = s.DisengageIfEngaged(ctx, e, o)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("different scopes must not conflict: %v / %v", errs[0], errs[1])
	}

	// Re-engage and disengage the same scope again: the earlier transition
	// must not poison later ones.
	e, o := mkEvent(domain.KillSwitchActionEngage, "SCOPE_ONE", nil)
	if err := s.AppendEvent(ctx, e, o); err != nil {
		t.Fatal(err)
	}
	d, do := mkEvent(domain.KillSwitchActionDisengage, "SCOPE_ONE", nil)
	if err := s.DisengageIfEngaged(ctx, d, do); err != nil {
		t.Fatalf("second disengage after a re-engage must succeed: %v", err)
	}
	// And a disengage with nothing engaged is the documented conflict.
	d2, do2 := mkEvent(domain.KillSwitchActionDisengage, "SCOPE_ONE", nil)
	if err := s.DisengageIfEngaged(ctx, d2, do2); !errors.Is(err, domain.ErrNotCurrentlyEngaged) {
		t.Fatalf("expected ErrNotCurrentlyEngaged, got %v", err)
	}
	if ev, _ := countRows(t, admin, "SCOPE_ONE", "DISENGAGE"); ev != 2 {
		t.Fatalf("expected 2 DISENGAGE rows for SCOPE_ONE (two real transitions), got %d", ev)
	}
}

func TestDisengage_NeverEngaged_And_ForeignTenantScope_AreConflicts_NothingWritten(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))

	// Never engaged.
	d, o := mkEvent(domain.KillSwitchActionDisengage, "NEVER_ENGAGED", nil)
	if err := s.DisengageIfEngaged(context.Background(), d, o); !errors.Is(err, domain.ErrNotCurrentlyEngaged) {
		t.Fatalf("expected ErrNotCurrentlyEngaged, got %v", err)
	}
	// Tenant A's switch is invisible to tenant B (RLS), so B's disengage of that
	// tuple is a conflict, and nothing is written for either tenant.
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	e, eo := mkEvent(domain.KillSwitchActionEngage, "TENANT_A_ONLY", strp(tenantA))
	if err := s.AppendEvent(ctxA, e, eo); err != nil {
		t.Fatal(err)
	}
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)
	d2, o2 := mkEvent(domain.KillSwitchActionDisengage, "TENANT_A_ONLY", strp(tenantA))
	if err := s.DisengageIfEngaged(ctxB, d2, o2); !errors.Is(err, domain.ErrNotCurrentlyEngaged) {
		t.Fatalf("tenant B must not be able to disengage tenant A's switch, got %v", err)
	}
	if ev, ob := countRows(t, admin, "TENANT_A_ONLY", "DISENGAGE"); ev != 0 || ob != 0 {
		t.Fatalf("nothing may be written, got %d / %d", ev, ob)
	}
}

// A failure inside the outbox insert rolls the DISENGAGE back with it: the
// switch stays ENGAGED and can still be disengaged afterwards.
func TestDisengage_OutboxFailure_RollsBackTheTransition(t *testing.T) {
	admin := openAdminPool(t)
	s := store.NewPgStore(appRolePool(t, admin))
	ctx := context.Background()

	eng, engOut := mkEvent(domain.KillSwitchActionEngage, "ROLLBACK_SCOPE", nil)
	if err := s.AppendEvent(ctx, eng, engOut); err != nil {
		t.Fatal(err)
	}
	d, o := mkEvent(domain.KillSwitchActionDisengage, "ROLLBACK_SCOPE", nil)
	// event_type is VARCHAR(128): a longer value makes the outbox insert fail.
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	o.EventType = string(long)
	if err := s.DisengageIfEngaged(ctx, d, o); err == nil || errors.Is(err, domain.ErrNotCurrentlyEngaged) {
		t.Fatalf("expected an outbox insert failure, got %v", err)
	}
	if ev, ob := countRows(t, admin, "ROLLBACK_SCOPE", "DISENGAGE"); ev != 0 || ob != 0 {
		t.Fatalf("the DISENGAGE row must roll back with the outbox failure, got %d / %d", ev, ob)
	}
	d2, o2 := mkEvent(domain.KillSwitchActionDisengage, "ROLLBACK_SCOPE", nil)
	if err := s.DisengageIfEngaged(ctx, d2, o2); err != nil {
		t.Fatalf("the switch must still be ENGAGED and disengageable: %v", err)
	}
}
