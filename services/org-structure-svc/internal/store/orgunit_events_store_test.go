package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/events"
	"zoiko.io/org-structure-svc/internal/events/asyncapitest"
	"zoiko.io/org-structure-svc/internal/middleware"
	"zoiko.io/org-structure-svc/internal/outbox"
	"zoiko.io/org-structure-svc/internal/store"
)

// ── helpers ────────────────────────────────────────────────────────────────────

type outRow struct {
	Type      string
	Key       string
	Tenant    string
	Published bool
	Env       map[string]any // the platform envelope
	Payload   map[string]any // the event payload inside it
}

func outboxRows(t *testing.T, pool *pgxpool.Pool) []outRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT event_type, partition_key, tenant_id, published_at IS NOT NULL, payload::text FROM event_outbox ORDER BY seq`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []outRow
	for rows.Next() {
		var r outRow
		var body string
		if err := rows.Scan(&r.Type, &r.Key, &r.Tenant, &r.Published, &body); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(body), &r.Env); err != nil {
			t.Fatal(err)
		}
		r.Payload, _ = r.Env["payload"].(map[string]any)
		out = append(out, r)
	}
	return out
}

func types(rows []outRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Type
	}
	return out
}

func asCaller(ctx context.Context, actor, correlation string) context.Context {
	return middleware.WithCorrelation(middleware.WithActor(ctx, actor), correlation)
}

func detail(r outRow, key string) any {
	d, _ := r.Payload["details"].(map[string]any)
	return d[key]
}

func wantEvent(t *testing.T, r outRow, name, objectID string, version float64, effective string) {
	t.Helper()
	if r.Type != name {
		t.Fatalf("want event %s, got %s", name, r.Type)
	}
	if r.Payload["object_id"] != objectID || r.Payload["object_version"] != version || r.Payload["effective_at"] != effective {
		t.Fatalf("%s payload: object_id=%v version=%v effective_at=%v (want %s v%v %s)", name,
			r.Payload["object_id"], r.Payload["object_version"], r.Payload["effective_at"], objectID, version, effective)
	}
	if r.Env["event_type"] != name || r.Env["source_service"] != "org-structure-svc" {
		t.Fatalf("%s envelope: %v", name, r.Env)
	}
}

// ── one event per command, in the command's transaction ────────────────────────

func TestEvents_LifecycleCarriesObjectVersionAndEffectiveDate(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := asCaller(orgCtx("tenant-a"), "alice", "corr-1")

	// The create body's own correlation_id is an idempotency key; the event is
	// traced by the request's id (corr-1), not by it.
	u, _, err := s.CreateOrgUnit(ctx, newUnit("e1", "FIN"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOrgUnit(ctx, u.UnitID, domain.UnitCommandActivate, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOrgUnit(ctx, u.UnitID, domain.UnitCommandDeactivate, 2); err != nil {
		t.Fatal(err)
	}

	rows := outboxRows(t, pool)
	if len(rows) != 3 {
		t.Fatalf("one event per command, got %v", types(rows))
	}
	wantEvent(t, rows[0], "org.unit.created", u.UnitID, 1, "2026-01-01")
	wantEvent(t, rows[1], "org.unit.activated", u.UnitID, 2, time.Now().UTC().Format("2006-01-02"))
	wantEvent(t, rows[2], "org.unit.deactivated", u.UnitID, 3, time.Now().UTC().Format("2006-01-02"))

	for i, r := range rows {
		if r.Key != u.UnitID || r.Tenant != "tenant-a" || r.Payload["tenant_id"] != "tenant-a" {
			t.Fatalf("event %d: partition key and tenant: %+v", i, r)
		}
		if r.Payload["actor"] != "alice" || r.Payload["correlation_id"] != "corr-1" || r.Env["actor_id"] != "alice" {
			t.Fatalf("event %d must carry actor and correlation id: %v", i, r.Payload)
		}
		if r.Payload["legal_entity_id"] != "e1" || r.Payload["object_type"] != "org_unit" || r.Payload["recorded_at"] == nil {
			t.Fatalf("event %d payload: %v", i, r.Payload)
		}
	}
	if detail(rows[1], "from_status") != "DRAFT" || detail(rows[1], "to_status") != "ACTIVE" {
		t.Fatalf("activation details: %v", rows[1].Payload["details"])
	}
	if detail(rows[0], "code") != "FIN" || detail(rows[0], "status") != "DRAFT" {
		t.Fatalf("creation details: %v", rows[0].Payload["details"])
	}
}

func TestEvents_ReparentAndManagerChange(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	a := mkUnit(t, s, ctx, "e1", "A", nil)
	b := mkUnit(t, s, ctx, "e1", "B", nil)
	oldMgr, newMgr := uuid.NewString(), uuid.NewString()
	req := newUnit("e1", "FIN")
	req.ParentUnitID, req.ManagerEmployeeID = &a.UnitID, &oldMgr
	fin, _, err := s.CreateOrgUnit(ctx, req, "creator")
	if err != nil {
		t.Fatal(err)
	}
	base := len(outboxRows(t, pool))

	if _, err := s.ReparentOrgUnit(ctx, fin.UnitID, moveTo(&b.UnitID, "2026-03-01"), 1, "maker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeOrgUnitManager(ctx, fin.UnitID, changeTo(newMgr, "2026-04-01"), 2, "maker"); err != nil {
		t.Fatal(err)
	}
	// And a first manager on a unit that had none: the previous one is null.
	if _, err := s.ChangeOrgUnitManager(ctx, a.UnitID, changeTo(newMgr, "2026-04-01"), 1, "maker"); err != nil {
		t.Fatal(err)
	}

	rows := outboxRows(t, pool)[base:]
	if len(rows) != 3 {
		t.Fatalf("want 3 new events, got %v", types(rows))
	}
	wantEvent(t, rows[0], "org.unit.reparented", fin.UnitID, 2, "2026-03-01")
	if detail(rows[0], "previous_parent_unit_id") != a.UnitID || detail(rows[0], "new_parent_unit_id") != b.UnitID {
		t.Fatalf("reparent details: %v", rows[0].Payload["details"])
	}
	if rows[0].Payload["actor"] != "maker" {
		t.Fatalf("a direct move is attributed to its maker, got %v", rows[0].Payload["actor"])
	}
	if _, has := rows[0].Payload["evidence_ref"]; has {
		t.Fatal("a direct move has no approval evidence")
	}

	wantEvent(t, rows[1], "org.unit.manager.changed", fin.UnitID, 3, "2026-04-01")
	if detail(rows[1], "previous_manager_employee_id") != oldMgr || detail(rows[1], "new_manager_employee_id") != newMgr {
		t.Fatalf("manager details: %v", rows[1].Payload["details"])
	}
	if detail(rows[2], "previous_manager_employee_id") != nil {
		t.Fatalf("a unit with no manager has a null previous manager, got %v", detail(rows[2], "previous_manager_employee_id"))
	}
}

// A command that fails writes no event, and an idempotent replay writes none
// either: an event attests exactly one change.
func TestEvents_RefusalsAndReplaysEmitNothing(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	root := mkUnit(t, s, ctx, "e1", "ROOT", nil)
	kid := mkUnit(t, s, ctx, "e1", "KID", &root.UnitID)
	base := len(outboxRows(t, pool))

	for name, err := range map[string]error{
		"loop": func() error {
			_, e := s.ReparentOrgUnit(ctx, root.UnitID, moveTo(&kid.UnitID, "2026-03-01"), 1, "x")
			return e
		}(),
		"stale version": func() error { _, e := s.ReparentOrgUnit(ctx, kid.UnitID, moveTo(nil, "2026-03-01"), 9, "x"); return e }(),
		"bad transition": func() error {
			_, e := s.TransitionOrgUnit(ctx, kid.UnitID, domain.UnitCommandDeactivate, 1)
			return e
		}(),
		"same manager": func() error {
			_, e := s.ChangeOrgUnitManager(ctx, kid.UnitID, changeTo("not-a-uuid", "2026-03-01"), 1, "x")
			return e
		}(),
	} {
		if err == nil {
			t.Errorf("%s should have been refused", name)
		}
	}

	// Replay of an idempotent create.
	req := newUnit("e1", "REPLAY")
	if _, _, err := s.CreateOrgUnit(ctx, req, "a"); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.CreateOrgUnit(ctx, req, "a"); err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}

	rows := outboxRows(t, pool)[base:]
	if len(rows) != 1 || rows[0].Type != "org.unit.created" {
		t.Fatalf("only the one real create may emit, got %v", types(rows))
	}
}

// The event is in the change's transaction: if the event cannot be written, the
// change does not happen. Break the outbox and prove nothing is half-done.
func TestEvents_FailedEventWriteRollsTheChangeBack(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")
	root := mkUnit(t, s, ctx, "e1", "ROOT", nil)

	if _, err := pool.Exec(context.Background(), `ALTER TABLE event_outbox RENAME TO event_outbox_broken`); err != nil {
		t.Fatal(err)
	}
	_, _, createErr := s.CreateOrgUnit(ctx, newUnit("e1", "GHOST"), "a")
	_, activateErr := s.TransitionOrgUnit(ctx, root.UnitID, domain.UnitCommandActivate, 1)
	if _, err := pool.Exec(context.Background(), `ALTER TABLE event_outbox_broken RENAME TO event_outbox`); err != nil {
		t.Fatal(err)
	}

	if createErr == nil || activateErr == nil {
		t.Fatalf("with the outbox unavailable the commands must fail: create=%v activate=%v", createErr, activateErr)
	}
	list, _ := s.ListOrgUnits(ctx, "e1", "")
	if len(list) != 1 {
		t.Fatalf("the failed create must leave no unit behind, got %d units", len(list))
	}
	if got, _ := s.GetOrgUnit(ctx, root.UnitID, ""); got.Status != domain.UnitStatusDraft || got.RecordVersion != 1 {
		t.Fatalf("the failed activation must leave the unit untouched: %s v%d", got.Status, got.RecordVersion)
	}
}

// ── maker-checker events ───────────────────────────────────────────────────────

func TestEvents_MakerCheckerTrail(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	a := mkMapped(t, s, ctx, "e1", "A", nil, "", "")
	b := mkMapped(t, s, ctx, "e1", "B", nil, "", "")
	fin := mkMapped(t, s, ctx, "e1", "FIN", &a.UnitID, "CC1", "")
	base := len(outboxRows(t, pool))

	cr, _, err := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01")
	if err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, pool)[base:]
	if len(rows) != 1 {
		t.Fatalf("a proposal announces itself once (its dry run emits nothing), got %v", types(rows))
	}
	wantEvent(t, rows[0], "org.unit.reparent.requested", cr.RequestID, 1, "2026-03-01")
	if rows[0].Key != fin.UnitID || rows[0].Payload["unit_id"] != fin.UnitID || rows[0].Payload["actor"] != "maker" {
		t.Fatalf("keyed by the unit so its events stay ordered: %+v", rows[0])
	}
	if detail(rows[0], "requires_approval") != true || detail(rows[0], "mapped_unit_count") != float64(1) {
		t.Fatalf("request details: %v", rows[0].Payload["details"])
	}

	// A repeat proposal returns the pending request and announces nothing.
	if _, created, _ := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01"); created {
		t.Fatal("the same proposal must be a replay")
	}
	if n := len(outboxRows(t, pool)) - base; n != 1 {
		t.Fatalf("a replay must not emit, got %d events", n)
	}

	if _, _, err := s.ApproveChangeRequest(asCaller(ctx, "checker", "corr-9"), cr.RequestID, "checker"); err != nil {
		t.Fatal(err)
	}
	rows = outboxRows(t, pool)[base+1:]
	if len(rows) != 2 {
		t.Fatalf("approval writes the move and the approval together, got %v", types(rows))
	}
	wantEvent(t, rows[0], "org.unit.reparented", fin.UnitID, 2, "2026-03-01")
	if rows[0].Payload["actor"] != "maker" || rows[0].Payload["evidence_ref"] != cr.RequestID {
		t.Fatalf("the move is the maker's act, evidenced by the request: %v", rows[0].Payload)
	}
	wantEvent(t, rows[1], "org.unit.reparent.approved", cr.RequestID, 2, "2026-03-01")
	if rows[1].Payload["actor"] != "checker" || detail(rows[1], "requested_by") != "maker" {
		t.Fatalf("the approval is the checker's act: %v", rows[1].Payload)
	}
	if rows[0].Payload["correlation_id"] != "corr-9" {
		t.Fatalf("both events share the approval's correlation id, got %v", rows[0].Payload["correlation_id"])
	}
}

func TestEvents_RejectAndFailure(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	a := mkMapped(t, s, ctx, "e1", "A", nil, "", "")
	b := mkMapped(t, s, ctx, "e1", "B", nil, "", "")
	fin := mkMapped(t, s, ctx, "e1", "FIN", &a.UnitID, "CC1", "")
	other := mkMapped(t, s, ctx, "e1", "OTHER", &a.UnitID, "CC2", "")

	rejected, _, _ := propose(t, s, ctx, fin, &b.UnitID, "2026-03-01")
	base := len(outboxRows(t, pool))
	if _, err := s.RejectChangeRequest(ctx, rejected.RequestID, "checker", "wrong centre"); err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, pool)[base:]
	if len(rows) != 1 {
		t.Fatalf("got %v", types(rows))
	}
	wantEvent(t, rows[0], "org.unit.reparent.rejected", rejected.RequestID, 2, "2026-03-01")
	if detail(rows[0], "decision_reason") != "wrong centre" || rows[0].Payload["actor"] != "checker" {
		t.Fatalf("reject details: %v", rows[0].Payload)
	}

	// A stale proposal: closed as FAILED, announced, and the move NOT emitted.
	stale, _, _ := propose(t, s, ctx, other, &b.UnitID, "2026-03-01")
	if _, err := s.ChangeOrgUnitManager(ctx, other.UnitID, changeTo(uuid.NewString(), "2026-02-01"), 1, "x"); err != nil {
		t.Fatal(err)
	}
	base = len(outboxRows(t, pool))
	if _, _, err := s.ApproveChangeRequest(ctx, stale.RequestID, "checker"); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}
	rows = outboxRows(t, pool)[base:]
	if len(rows) != 1 {
		t.Fatalf("a failed approval announces only the failure, got %v", types(rows))
	}
	wantEvent(t, rows[0], "org.unit.reparent.failed", stale.RequestID, 2, "2026-03-01")
	if detail(rows[0], "failure_code") != "version_conflict" {
		t.Fatalf("failure details: %v", rows[0].Payload["details"])
	}
}

// ── relay against real rows ────────────────────────────────────────────────────

type captureWriter struct {
	got  []string // event types, in delivery order
	fail bool
}

func (w *captureWriter) WriteMessages(_ context.Context, msgs ...outbox.KafkaMessage) error {
	if w.fail {
		return errors.New("broker down")
	}
	for _, m := range msgs {
		var env struct {
			EventType string `json:"event_type"`
		}
		_ = json.Unmarshal(m.Value, &env)
		w.got = append(w.got, env.EventType)
	}
	return nil
}

func TestRelay_DeliversInOrderRetriesAndSkipsDeadLetters(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := orgCtx("tenant-a")

	u := mkUnit(t, s, ctx, "e1", "FIN", nil)
	if _, err := s.TransitionOrgUnit(ctx, u.UnitID, domain.UnitCommandActivate, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeOrgUnitManager(ctx, u.UnitID, changeTo(uuid.NewString(), "2026-03-01"), 2, "m"); err != nil {
		t.Fatal(err)
	}

	cfg := outbox.DefaultRelayConfig()
	w := &captureWriter{fail: true}
	relay := outbox.NewRelay(pool, w, cfg, zap.NewNop())

	// Broker down: nothing is delivered, the failure is recorded, nothing is lost.
	if n, err := relay.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("down: n=%d err=%v", n, err)
	}
	var attempts int
	var lastErr *string
	var due bool
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts, last_error, next_attempt_at > now() FROM event_outbox ORDER BY seq LIMIT 1`).Scan(&attempts, &lastErr, &due); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastErr == nil || !due {
		t.Fatalf("the failure must be recorded with a future retry: attempts=%d err=%v retryLater=%v", attempts, lastErr, due)
	}
	if n, _ := relay.DrainOnce(ctx); n != 0 {
		t.Fatal("an event in backoff must not be retried immediately")
	}

	// Broker back and the retry time reached: all three, in the order written.
	w.fail = false
	if _, err := pool.Exec(context.Background(), `UPDATE event_outbox SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	if n, err := relay.DrainOnce(ctx); err != nil || n != 3 {
		t.Fatalf("recovery: n=%d err=%v", n, err)
	}
	want := []string{"org.unit.created", "org.unit.activated", "org.unit.manager.changed"}
	for i := range want {
		if i >= len(w.got) || w.got[i] != want[i] {
			t.Fatalf("delivery order: got %v want %v", w.got, want)
		}
	}
	if n, _ := relay.DrainOnce(ctx); n != 0 {
		t.Fatal("delivered events are never sent again")
	}
	if pending, _ := relay.PendingCount(ctx); pending != 0 {
		t.Fatalf("nothing should remain pending, got %d", pending)
	}

	// A poison event past MaxAttempts is left alone and counted.
	if _, err := s.TransitionOrgUnit(ctx, u.UnitID, domain.UnitCommandDeactivate, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE event_outbox SET attempts = $1 WHERE published_at IS NULL`, cfg.MaxAttempts); err != nil {
		t.Fatal(err)
	}
	w.got = nil
	if n, _ := relay.DrainOnce(ctx); n != 0 || len(w.got) != 0 {
		t.Fatalf("a dead-lettered event must not be retried, delivered %v", w.got)
	}
	if dead, _ := relay.DeadLetterCount(ctx); dead != 1 {
		t.Fatalf("dead letters: want 1, got %d", dead)
	}
}

// ── row-level security on the outbox ───────────────────────────────────────────

// Superusers bypass RLS, so the policy is exercised as an ordinary role.
func TestOutbox_RowLevelSecurityPolicy(t *testing.T) {
	admin := openOrgUnitPool(t)
	s := store.New(admin)
	mkUnit(t, s, orgCtx("tenant-a"), "e1", "A1", nil)
	mkUnit(t, s, orgCtx("tenant-b"), "e1", "B1", nil)

	bg := context.Background()
	if _, err := admin.Exec(bg, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_rls_test') THEN CREATE ROLE org_rls_test LOGIN; END IF;
	END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(bg, `GRANT SELECT, INSERT, UPDATE ON event_outbox TO org_rls_test`); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = "org_rls_test"
	app, err := pgxpool.NewWithConfig(bg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	visible := func(setting, value string) int {
		tx, err := app.Begin(bg)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(bg) //nolint:errcheck
		if setting != "" {
			if _, err := tx.Exec(bg, `SELECT set_config($1, $2, true)`, setting, value); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := tx.QueryRow(bg, `SELECT COUNT(*) FROM event_outbox`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := visible("app.tenant_id", "tenant-a"); n != 1 {
		t.Fatalf("a tenant sees only its own events, got %d", n)
	}
	if n := visible("app.tenant_id", "tenant-b"); n != 1 {
		t.Fatalf("tenant-b sees only its own events, got %d", n)
	}
	if n := visible("", ""); n != 0 {
		t.Fatalf("with no tenant and no relay capability nothing is visible, got %d", n)
	}
	if n := visible("app.outbox_relay", "true"); n != 2 {
		t.Fatalf("the relay capability sees every tenant's events, got %d", n)
	}

	// An ordinary tenant cannot write another tenant's event.
	tx, _ := app.Begin(bg)
	defer tx.Rollback(bg) //nolint:errcheck
	_, _ = tx.Exec(bg, `SELECT set_config('app.tenant_id', 'tenant-a', true)`)
	if _, err := tx.Exec(bg, `INSERT INTO event_outbox (event_id, event_type, tenant_id, partition_key, payload)
		VALUES ($1, 'X', 'tenant-b', 'k', '{}')`, uuid.NewString()); err == nil {
		t.Fatal("RLS must refuse a write for another tenant")
	}
}

// ── the contract, checked against real events ──────────────────────────────────

// Run every command that emits, then hold each event to asyncapi.yaml: the
// envelope and payload carry everything the file requires, and the code emits
// nothing the file does not declare. This is what makes the file a contract and
// not a description.
func TestEvents_EverythingEmittedConformsToTheAsyncAPI(t *testing.T) {
	pool := openOrgUnitPool(t)
	s := store.New(pool)
	ctx := asCaller(orgCtx("tenant-a"), "alice", "corr-1")

	// created, activated, reparented (direct), manager changed, deactivated
	root := mkUnit(t, s, ctx, "e1", "ROOT", nil)
	other := mkUnit(t, s, ctx, "e1", "OTHER", nil)
	if _, err := s.TransitionOrgUnit(ctx, root.UnitID, domain.UnitCommandActivate, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOrgUnit(ctx, other.UnitID, domain.UnitCommandActivate, 1); err != nil {
		t.Fatal(err)
	}
	mgr := uuid.NewString()
	req := newUnit("e1", "LEAF")
	req.ParentUnitID, req.ManagerEmployeeID, req.CostCenterCode = &root.UnitID, &mgr, ""
	leaf, _, err := s.CreateOrgUnit(ctx, req, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReparentOrgUnit(ctx, leaf.UnitID, moveTo(&other.UnitID, "2026-03-01"), 1, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeOrgUnitManager(ctx, leaf.UnitID, changeTo(uuid.NewString(), "2026-04-01"), 2, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOrgUnit(ctx, leaf.UnitID, domain.UnitCommandActivate, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOrgUnit(ctx, leaf.UnitID, domain.UnitCommandDeactivate, 4); err != nil {
		t.Fatal(err)
	}

	// requested + approved (+ reparented with evidence), rejected, failed
	a := mkMapped(t, s, ctx, "e1", "A", nil, "", "")
	b := mkMapped(t, s, ctx, "e1", "B", nil, "", "")
	approved := mkMapped(t, s, ctx, "e1", "M1", &a.UnitID, "CC1", "")
	rejected := mkMapped(t, s, ctx, "e1", "M2", &a.UnitID, "CC2", "")
	stale := mkMapped(t, s, ctx, "e1", "M3", &a.UnitID, "CC3", "")
	crA, _, _ := propose(t, s, ctx, approved, &b.UnitID, "2026-03-01")
	crR, _, _ := propose(t, s, ctx, rejected, &b.UnitID, "2026-03-01")
	crS, _, _ := propose(t, s, ctx, stale, &b.UnitID, "2026-03-01")
	if _, _, err := s.ApproveChangeRequest(ctx, crA.RequestID, "checker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RejectChangeRequest(ctx, crR.RequestID, "checker", "no"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeOrgUnitManager(ctx, stale.UnitID, changeTo(uuid.NewString(), "2026-02-01"), 1, "x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApproveChangeRequest(ctx, crS.RequestID, "checker"); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("setup: want a stale approval, got %v", err)
	}

	spec := asyncapitest.Load(t)
	rows := outboxRows(t, pool)
	seen := map[string]bool{}
	common := spec.Component("OrgUnitPayload")

	for i, r := range rows {
		seen[r.Type] = true
		m, ok := spec.ByName(r.Type)
		if !ok {
			t.Errorf("event %d: %s is emitted but not declared in asyncapi.yaml", i, r.Type)
			continue
		}

		env := spec.Envelope(m)
		for _, field := range env.Required {
			if _, ok := r.Env[field]; !ok {
				t.Errorf("%s: envelope is missing the required field %q", r.Type, field)
			}
		}
		for field := range r.Env {
			if _, ok := env.Properties[field]; !ok {
				t.Errorf("%s: envelope carries %q, which asyncapi.yaml does not declare", r.Type, field)
			}
		}

		payload := spec.Payload(m)
		for _, field := range common.Required {
			if _, ok := r.Payload[field]; !ok {
				t.Errorf("%s: payload is missing the required field %q", r.Type, field)
			}
		}
		for field := range r.Payload {
			if _, ok := payload.Properties[field]; !ok {
				t.Errorf("%s: payload carries %q, which asyncapi.yaml does not declare", r.Type, field)
			}
		}

		details := spec.Details(m)
		got, _ := r.Payload["details"].(map[string]any)
		for _, field := range details.Required {
			if _, ok := got[field]; !ok {
				t.Errorf("%s: details is missing the required %q", r.Type, field)
			}
		}
		for field := range got {
			if _, ok := details.Properties[field]; !ok {
				t.Errorf("%s: details carries %q, which asyncapi.yaml does not declare", r.Type, field)
			}
		}
	}

	for _, name := range events.OrgEventNames {
		if !seen[name] {
			t.Errorf("the scenario never emitted %s, so its contract was not exercised", name)
		}
	}
}
