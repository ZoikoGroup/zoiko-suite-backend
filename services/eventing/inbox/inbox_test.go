package inbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/eventing/integration"
	"zoiko.io/eventing/repair"
)

// harness gives each test its own schema holding the inbox, the repair log and
// a "business effect" table, so tests never see each other's rows.
type harness struct {
	pool   *pgxpool.Pool
	admin  *pgx.Conn
	schema string
	dsn    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" || os.Getenv("REQUIRE_DB_TESTS") != "" {
			t.Fatal("TEST_DATABASE_URL must be set when CI or REQUIRE_DB_TESTS is set")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "inbox_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{SchemaSQL, repair.SchemaSQL,
		`CREATE TABLE effects (id BIGSERIAL PRIMARY KEY, event_id TEXT NOT NULL, version BIGINT)`} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("apply ddl: %v", err)
		}
	}
	return &harness{pool: pool, admin: admin, schema: schema, dsn: dsn}
}

func (h *harness) inbox(t *testing.T, max int) *Inbox {
	t.Helper()
	i, err := New(PoolRunner{Pool: h.pool}, "test-consumer", Config{MaxAttempts: max})
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func (h *harness) effects(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM effects`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func msg(id string) Message {
	return Message{EventID: id, EventType: "com.zoikosuite.test.thing.changed", TenantID: "tenant-a", PayloadHash: "sha256:aaa"}
}

func versioned(id, agg string, v int64) Message {
	m := msg(id)
	m.AggregateID = agg
	m.AggregateVersion = &v
	return m
}

func applyEffect(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
	_, err := tx.Exec(ctx, `INSERT INTO effects (event_id) VALUES ($1)`, r.EventID)
	return "effect-" + r.EventID, err
}

func handler(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
	return applyEffect(ctx, tx, r)
}

var validAuth = repair.Authorization{Operator: "ops-1", Approver: "lead-1", ApprovalRef: "CHG-42", Reason: "fixed mapping bug"}

// ---- core guarantee --------------------------------------------------------

func TestProcessOnceThenDuplicate_INT_NP_007(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	ctx := context.Background()

	r1, err := in.Process(ctx, msg("e1"), handler)
	if err != nil || r1.Outcome != Processed || r1.ResultRef != "effect-e1" {
		t.Fatalf("first: %+v %v", r1, err)
	}
	var calls int32
	r2, err := in.Process(ctx, msg("e1"), func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
		atomic.AddInt32(&calls, 1)
		return applyEffect(ctx, tx, r)
	})
	if err != nil || r2.Outcome != Duplicate || calls != 0 {
		t.Fatalf("second: %+v err=%v calls=%d", r2, err, calls)
	}
	if h.effects(t) != 1 {
		t.Fatalf("effects = %d, want exactly 1", h.effects(t))
	}
	rec, err := in.Get(ctx, "tenant-a", "e1")
	if err != nil || rec.State != StateCompleted || rec.DuplicateCount != 1 || rec.CompletedAt == nil || rec.AttemptCount != 1 {
		t.Fatalf("receipt: %+v %v", rec, err)
	}
}

func TestConcurrentDuplicatesProduceOneEffect(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	var wg sync.WaitGroup
	var processed, duplicate int32
	for n := 0; n < 24; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := in.Process(context.Background(), msg("race"), handler)
			if err != nil {
				t.Errorf("process: %v", err)
				return
			}
			switch r.Outcome {
			case Processed:
				atomic.AddInt32(&processed, 1)
			case Duplicate:
				atomic.AddInt32(&duplicate, 1)
			}
		}()
	}
	wg.Wait()
	if processed != 1 || duplicate != 23 || h.effects(t) != 1 {
		t.Fatalf("processed=%d duplicate=%d effects=%d", processed, duplicate, h.effects(t))
	}
}

func TestCrashBeforeCommitLeavesNoEffectAndRetrySucceeds_INT_NP_013(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 5)
	ctx := context.Background()

	res, err := in.Process(ctx, msg("c1"), func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
		if _, e := applyEffect(ctx, tx, r); e != nil {
			return "", e
		}
		return "", errors.New("consumer died after writing the effect")
	})
	if err == nil || res.FailureState != StateFailed {
		t.Fatalf("want retryable failure, got %+v %v", res, err)
	}
	if h.effects(t) != 0 {
		t.Fatal("a failed handler's effect must roll back with it")
	}
	rec, _ := in.Get(ctx, "tenant-a", "c1")
	if rec.State != StateFailed || rec.AttemptCount != 1 || rec.LastErrorCode != "HANDLER_FAILED" {
		t.Fatalf("receipt after failure: %+v", rec)
	}
	res, err = in.Process(ctx, msg("c1"), handler)
	if err != nil || res.Outcome != Processed || res.Attempt != 2 || h.effects(t) != 1 {
		t.Fatalf("retry: %+v %v effects=%d", res, err, h.effects(t))
	}
}

func TestIdempotencyKeyIsStableAcrossRedelivery(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 5)
	var keys []string
	fail := true
	for n := 0; n < 2; n++ {
		_, _ = in.Process(context.Background(), msg("k1"), func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
			keys = append(keys, r.IdempotencyKey)
			if fail {
				fail = false
				return "", errors.New("transient")
			}
			return "", nil
		})
	}
	if len(keys) != 2 || keys[0] != keys[1] || keys[0] != "test-consumer:k1" {
		t.Fatalf("keys = %v", keys)
	}
}

func TestConflictingRedeliveryRejected_INT_IDEM_005(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	ctx := context.Background()
	if _, err := in.Process(ctx, msg("x"), handler); err != nil {
		t.Fatal(err)
	}
	other := msg("x")
	other.PayloadHash = "sha256:bbb"
	var ran bool
	_, err := in.Process(ctx, other, func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) { ran = true; return "", nil })
	if !errors.Is(err, integration.New(integration.CodeIdempotencyConflict, "")) {
		t.Fatalf("want INT-IDEM-005, got %v", err)
	}
	if ran || h.effects(t) != 1 {
		t.Fatal("a conflicting redelivery must change nothing")
	}
}

// ---- failure, quarantine, repair -------------------------------------------

func TestRetryBudgetThenQuarantineThenHeld(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	ctx := context.Background()
	boom := func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
		return "", errors.New("downstream 500")
	}

	var last Result
	for n := 1; n <= 3; n++ {
		last, _ = in.Process(ctx, msg("p1"), boom)
		want := StateFailed
		if n == 3 {
			want = StateQuarantine
		}
		if last.FailureState != want || last.Attempt != n {
			t.Fatalf("attempt %d: %+v, want state %s", n, last, want)
		}
	}
	var ran bool
	res, err := in.Process(ctx, msg("p1"), func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) { ran = true; return "", nil })
	if err != nil || res.Outcome != Held || ran {
		t.Fatalf("quarantined event must be held, not processed: %+v %v ran=%v", res, err, ran)
	}
}

func TestPoisonQuarantinesImmediately(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 10)
	res, err := in.Process(context.Background(), msg("poison"), func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
		return "", Poison(errors.New("unmappable payload"))
	})
	if err == nil || res.FailureState != StateQuarantine || res.Attempt != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	rec, _ := in.Get(context.Background(), "tenant-a", "poison")
	if rec.LastErrorCode != "POISON_MESSAGE" {
		t.Fatalf("code = %q", rec.LastErrorCode)
	}
}

func TestStoredErrorTextIsScrubbed_INT_NP_047(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	_, _ = in.Process(context.Background(), msg("s"), func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
		return "", errors.New("call failed password=hunter2 Bearer abcdefghijklmnop")
	})
	rec, _ := in.Get(context.Background(), "tenant-a", "s")
	if strings.Contains(rec.LastError, "hunter2") || strings.Contains(rec.LastError, "abcdefghijklmnop") {
		t.Fatalf("secret persisted: %q", rec.LastError)
	}
}

func TestRequeueNeedsApprovalAndIsLogged(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 1)
	ctx := context.Background()
	_, _ = in.Process(ctx, msg("q1"), func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) { return "", errors.New("bad") })

	for name, a := range map[string]repair.Authorization{
		"empty":         {},
		"self-approved": {Operator: "ops-1", Approver: "OPS-1", ApprovalRef: "CHG-1", Reason: "x"},
		"no reason":     {Operator: "a", Approver: "b", ApprovalRef: "CHG-1"},
		"no ref":        {Operator: "a", Approver: "b", Reason: "x"},
	} {
		if err := in.Requeue(ctx, "tenant-a", "q1", a); err == nil {
			t.Errorf("%s: repair without proper approval must be refused", name)
		}
	}
	if rec, _ := in.Get(ctx, "tenant-a", "q1"); rec.State != StateQuarantine {
		t.Fatalf("refused repairs must not change state: %s", rec.State)
	}
	if err := in.Requeue(ctx, "tenant-a", "q1", validAuth); err != nil {
		t.Fatal(err)
	}
	rec, _ := in.Get(ctx, "tenant-a", "q1")
	if rec.State != StateFailed || rec.AttemptCount != 0 || rec.LastError != "" {
		t.Fatalf("after requeue: %+v", rec)
	}
	var n int
	var op, ap, prior string
	if err := h.pool.QueryRow(ctx, `SELECT count(*), max(operator), max(approver), max(prior_state) FROM eventing_repair_log WHERE event_id='q1'`).Scan(&n, &op, &ap, &prior); err != nil {
		t.Fatal(err)
	}
	if n != 1 || op != "ops-1" || ap != "lead-1" || prior != StateQuarantine {
		t.Fatalf("repair log: n=%d %s %s %s", n, op, ap, prior)
	}
	res, err := in.Process(ctx, msg("q1"), handler)
	if err != nil || res.Outcome != Processed {
		t.Fatalf("replay after repair: %+v %v", res, err)
	}
}

func TestRequeueRefusals(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	ctx := context.Background()
	if err := in.Requeue(ctx, "tenant-a", "ghost", validAuth); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	_, _ = in.Process(ctx, msg("done"), handler)
	if err := in.Requeue(ctx, "tenant-a", "done", validAuth); !errors.Is(err, ErrNotRepairable) {
		t.Fatalf("completed receipts must not be requeued: %v", err)
	}
}

func TestRepairLogIsAppendOnly(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 1)
	ctx := context.Background()
	_, _ = in.Process(ctx, msg("a1"), func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) { return "", errors.New("x") })
	if err := in.Requeue(ctx, "tenant-a", "a1", validAuth); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `UPDATE eventing_repair_log SET reason='edited'`); err == nil {
		t.Error("repair log UPDATE must be refused")
	}
	if _, err := h.pool.Exec(ctx, `DELETE FROM eventing_repair_log`); err == nil {
		t.Error("repair log DELETE must be refused")
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO eventing_repair_log (repair_id,target_kind,tenant_id,event_id,action,operator,approver,approval_ref,reason,prior_state)
		VALUES (gen_random_uuid(),'inbox','t','e','requeue','same','same','r','x','quarantined')`); err == nil {
		t.Error("operator == approver must be refused by the database too")
	}
}

func TestQuarantineNeverOverwritesCompleted(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	ctx := context.Background()
	_, _ = in.Process(ctx, msg("d"), handler)
	if err := in.Quarantine(ctx, msg("d"), "MANUAL", "operator hold"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := in.Get(ctx, "tenant-a", "d"); rec.State != StateCompleted {
		t.Fatalf("an applied effect cannot be un-applied by quarantine: %s", rec.State)
	}
	if err := in.Quarantine(ctx, msg("fresh"), "SCHEMA", "bad shape"); err != nil {
		t.Fatal(err)
	}
	res, _ := in.Process(ctx, msg("fresh"), handler)
	if res.Outcome != Held || h.effects(t) != 1 {
		t.Fatalf("quarantined-before-processing must be held: %+v effects=%d", res, h.effects(t))
	}
}

// ---- ordering ---------------------------------------------------------------

func TestOutOfOrderDoesNotRegressState_INT_NP_008(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	ctx := context.Background()
	var applied []int64
	rec := func(v int64) Handler {
		return func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
			applied = append(applied, v)
			return "", nil
		}
	}
	if r, err := in.Process(ctx, versioned("v2", "agg-1", 2), rec(2)); err != nil || r.Outcome != Processed || r.Gap {
		t.Fatalf("v2: %+v %v", r, err)
	}
	r, err := in.Process(ctx, versioned("v1", "agg-1", 1), rec(1))
	if err != nil || r.Outcome != Stale || !strings.Contains(r.ResultRef, "stale") {
		t.Fatalf("v1 after v2 must be stale: %+v %v", r, err)
	}
	if r, _ := in.Process(ctx, versioned("v5", "agg-1", 5), rec(5)); r.Outcome != Processed || !r.Gap {
		t.Fatalf("v5 after v2 must process and flag the gap: %+v", r)
	}
	if fmt.Sprint(applied) != "[2 5]" {
		t.Fatalf("applied = %v", applied)
	}
	if got, _ := in.Get(ctx, "tenant-a", "v1"); got.State != StateSuperseded {
		t.Fatalf("stale receipt state = %s", got.State)
	}
	// redelivery of the stale event is a duplicate, not a second evaluation
	if r, _ := in.Process(ctx, versioned("v1", "agg-1", 1), rec(1)); r.Outcome != Duplicate {
		t.Fatalf("redelivered stale: %+v", r)
	}
	// another aggregate is independent
	if r, _ := in.Process(ctx, versioned("o1", "agg-2", 1), rec(1)); r.Outcome != Processed {
		t.Fatalf("independent aggregate: %+v", r)
	}
}

func TestConcurrentVersionsOnlyEverApplyIncreasing(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	var mu sync.Mutex
	var applied []int64
	var wg sync.WaitGroup
	for v := int64(1); v <= 12; v++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := in.Process(context.Background(), versioned(fmt.Sprintf("cv%d", v), "agg-c", v),
				func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
					mu.Lock()
					applied = append(applied, v)
					mu.Unlock()
					return "", nil
				})
			if err != nil {
				t.Errorf("v%d: %v", v, err)
			}
		}()
	}
	wg.Wait()
	for i := 1; i < len(applied); i++ {
		if applied[i] <= applied[i-1] {
			t.Fatalf("state regressed: applied order %v", applied)
		}
	}
	if len(applied) == 0 {
		t.Fatal("nothing applied")
	}
}

// ---- queries ---------------------------------------------------------------

func TestStatsAndListStuck(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 2)
	ctx := context.Background()
	_, _ = in.Process(ctx, msg("ok1"), handler)
	_, _ = in.Process(ctx, msg("ok1"), handler)
	boom := func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) { return "", errors.New("x") }
	_, _ = in.Process(ctx, msg("f1"), boom)
	_, _ = in.Process(ctx, msg("q1"), boom)
	_, _ = in.Process(ctx, msg("q1"), boom)

	s, err := in.ReadStats(ctx, "tenant-a")
	if err != nil || s.Completed != 1 || s.Failed != 1 || s.Quarantined != 1 || s.Duplicates != 1 {
		t.Fatalf("stats: %+v %v", s, err)
	}
	time.Sleep(1100 * time.Millisecond)
	stuck, err := in.ListStuck(ctx, "tenant-a", time.Second, 10)
	if err != nil || len(stuck) != 2 {
		t.Fatalf("stuck: %d %v", len(stuck), err)
	}
	if s2, _ := in.ReadStats(ctx, "tenant-a"); s2.OldestAttentionAge < time.Second {
		t.Fatalf("attention age = %v", s2.OldestAttentionAge)
	}
	if none, _ := in.ListStuck(ctx, "tenant-a", time.Hour, 10); len(none) != 0 {
		t.Fatal("nothing is an hour old")
	}
	if _, err := in.Get(ctx, "tenant-a", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing receipt: %v", err)
	}
}

func TestConsumersAreIndependent(t *testing.T) {
	h := newHarness(t)
	a := h.inbox(t, 3)
	b, _ := New(PoolRunner{Pool: h.pool}, "other-consumer", DefaultConfig())
	ctx := context.Background()
	if r, _ := a.Process(ctx, msg("same"), handler); r.Outcome != Processed {
		t.Fatal("a")
	}
	if r, _ := b.Process(ctx, msg("same"), handler); r.Outcome != Processed {
		t.Fatalf("a second consumer must still receive the event: %+v", r)
	}
	if h.effects(t) != 2 {
		t.Fatalf("effects = %d", h.effects(t))
	}
}

// ---- tenant isolation --------------------------------------------------------

func TestTenantIsolationUnderRLS(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	role := "inbox_rls_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for _, ddl := range []string{
		fmt.Sprintf(`CREATE ROLE %s NOLOGIN`, role),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, h.schema, role),
		fmt.Sprintf(`GRANT ALL ON ALL TABLES IN SCHEMA %s TO %s`, h.schema, role),
		fmt.Sprintf(`GRANT ALL ON ALL SEQUENCES IN SCHEMA %s TO %s`, h.schema, role),
		RLSSQL,
	} {
		if _, err := h.pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	t.Cleanup(func() {
		_, _ = h.admin.Exec(context.Background(), fmt.Sprintf(`DROP OWNED BY %s`, role))
		_, _ = h.admin.Exec(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role))
	})
	cfg, _ := pgxpool.ParseConfig(h.dsn)
	cfg.ConnConfig.RuntimeParams["search_path"] = h.schema
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+role)
		return err
	}
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	var super bool
	_ = app.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super)
	if super {
		t.Fatal("test role must be subject to RLS")
	}

	in, _ := New(PoolRunner{Pool: app, TenantSetting: "app.tenant_id"}, "test-consumer", DefaultConfig())
	ma, mb := msg("shared-id"), msg("shared-id")
	mb.TenantID = "tenant-b"
	if r, err := in.Process(ctx, ma, handlerNoEffects); err != nil || r.Outcome != Processed {
		t.Fatalf("tenant-a: %+v %v", r, err)
	}
	if r, err := in.Process(ctx, mb, handlerNoEffects); err != nil || r.Outcome != Processed {
		t.Fatalf("the same event id under another tenant is a different receipt: %+v %v", r, err)
	}
	if _, err := in.Get(ctx, "tenant-b", "shared-id"); err != nil {
		t.Fatal(err)
	}
	// tenant-a's session must not see tenant-b's row even with a direct query.
	err = PoolRunner{Pool: app, TenantSetting: "app.tenant_id"}.InTx(ctx, "tenant-a", func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eventing_inbox`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("tenant-a sees %d receipts, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// no tenant set -> no rows
	var n int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM eventing_inbox`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("without a tenant the inbox must show nothing: n=%d err=%v", n, err)
	}
}

func handlerNoEffects(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) { return "", nil }

// ---- no database needed -------------------------------------------------------

func TestParseCanonicalAndLegacy(t *testing.T) {
	canon := `{"id":"01","type":"com.zoikosuite.a.b.c","tenantid":"t1","aggregateid":"agg","aggregateversion":7,"payloadhash":"sha256:abc","data":{"x":1}}`
	m, err := Parse([]byte(canon))
	if err != nil || m.EventID != "01" || m.TenantID != "t1" || *m.AggregateVersion != 7 || m.PayloadHash != "sha256:abc" {
		t.Fatalf("canonical: %+v %v", m, err)
	}
	l1 := `{"event_id":"e9","event_type":"journal.posted","tenant_id":"t2","emitted_at":"2026-01-01T00:00:00Z","payload":{"a":1}}`
	l2 := `{"event_id":"e9","event_type":"journal.posted","tenant_id":"t2","emitted_at":"2026-02-02T00:00:00Z","payload":{"a":1}}`
	m1, err := Parse([]byte(l1))
	if err != nil || m1.EventID != "e9" || m1.TenantID != "t2" || !strings.HasPrefix(m1.PayloadHash, "sha256:") {
		t.Fatalf("legacy: %+v %v", m1, err)
	}
	m2, _ := Parse([]byte(l2))
	if m1.PayloadHash != m2.PayloadHash {
		t.Fatal("a redelivery differing only in emission time must not look like a conflicting event")
	}
	l3 := strings.Replace(l1, `"a":1`, `"a":2`, 1)
	if m3, _ := Parse([]byte(l3)); m3.PayloadHash == m1.PayloadHash {
		t.Fatal("different payload must hash differently")
	}
}

func TestParseRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":       `{`,
		"no id":          `{"type":"t","tenantid":"x","data":{}}`,
		"no tenant":      `{"id":"1","type":"t","data":{}}`,
		"no type":        `{"id":"1","tenantid":"x","data":{}}`,
		"half aggregate": `{"id":"1","type":"t","tenantid":"x","aggregateid":"a","data":{}}`,
		"version no id":  `{"id":"1","type":"t","tenantid":"x","aggregateversion":3,"data":{}}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected refusal", name)
		}
	}
}

func TestNewAndProcessValidation(t *testing.T) {
	if _, err := New(nil, "c", DefaultConfig()); err == nil {
		t.Error("nil runner accepted")
	}
	for _, bad := range []string{"", "Upper", "has space", strings.Repeat("a", 70)} {
		if _, err := New(PoolRunner{}, bad, DefaultConfig()); err == nil {
			t.Errorf("consumer %q accepted", bad)
		}
	}
	if _, err := New(PoolRunner{}, "c", Config{}); err == nil {
		t.Error("zero MaxAttempts accepted")
	}
	in, _ := New(PoolRunner{}, "c", DefaultConfig())
	if _, err := in.Process(context.Background(), Message{}, handlerNoEffects); !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("empty message: %v", err)
	}
	if _, err := in.Process(context.Background(), msg("x"), nil); err == nil {
		t.Error("nil handler accepted")
	}
	if err := in.Requeue(context.Background(), "t", "e", repair.Authorization{}); !errors.Is(err, repair.ErrNotAuthorized) {
		t.Errorf("requeue without approval must fail before touching the database: %v", err)
	}
}

func TestPoisonHelper(t *testing.T) {
	if Poison(nil) != nil {
		t.Fatal("Poison(nil) must be nil")
	}
	if !errors.Is(Poison(errors.New("x")), ErrPoison) {
		t.Fatal("Poison must wrap ErrPoison")
	}
}

func TestEvaluateInboxAlerts(t *testing.T) {
	if a := Evaluate(Stats{Completed: 10, Duplicates: 3}, Thresholds{MaxFailed: 5}); len(a) != 0 {
		t.Fatalf("healthy inbox alerted: %v", a)
	}
	a := Evaluate(Stats{Quarantined: 1, Failed: 9}, Thresholds{MaxFailed: 5})
	if len(a) != 2 || a[0].Event != "inbox_processing_quarantined" || a[1].Event != "inbox_processing_failing" {
		t.Fatalf("alerts = %+v", a)
	}
	if a := Evaluate(Stats{Failed: 99}, Thresholds{}); len(a) != 0 {
		t.Fatalf("zero threshold means unchecked: %v", a)
	}
}

// A delivery that waits on a concurrent delivery of the same event must give up
// after LockTimeout instead of waiting forever, and must record nothing.
func TestLockTimeoutBoundsWaitForConcurrentDelivery(t *testing.T) {
	h := newHarness(t)
	in, err := New(PoolRunner{Pool: h.pool}, "test-consumer", Config{MaxAttempts: 3, LockTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan Result, 1)
	go func() {
		r, _ := in.Process(ctx, msg("slow"), func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
			close(entered)
			<-release
			return applyEffect(ctx, tx, r)
		})
		done <- r
	}()
	<-entered

	start := time.Now()
	_, err = in.Process(ctx, msg("slow"), handler)
	if err == nil {
		t.Fatal("a delivery blocked behind another must time out, not succeed")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("lock wait was not bounded: %v", time.Since(start))
	}
	close(release)
	if r := <-done; r.Outcome != Processed {
		t.Fatalf("the first delivery must still complete: %+v", r)
	}
	if h.effects(t) != 1 {
		t.Fatalf("effects = %d, want 1", h.effects(t))
	}
	if rec, _ := in.Get(ctx, "tenant-a", "slow"); rec == nil || rec.State != StateCompleted || rec.AttemptCount != 1 {
		t.Fatalf("a timed-out wait must not disturb the receipt: %+v", rec)
	}
	// and the waiter, retried later, is correctly a duplicate
	if r, err := in.Process(ctx, msg("slow"), handler); err != nil || r.Outcome != Duplicate {
		t.Fatalf("retry after the first finished: %+v %v", r, err)
	}
}

func TestVerifySchema(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := VerifySchema(ctx, h.pool); err != nil {
		t.Fatalf("schema applied but VerifySchema failed: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `ALTER TABLE eventing_inbox DROP COLUMN result_ref`); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(ctx, h.pool); err == nil || !strings.Contains(err.Error(), "result_ref") {
		t.Fatalf("missing column not reported: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `DROP TABLE eventing_inbox`); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(ctx, h.pool); err == nil {
		t.Fatal("missing table not reported")
	}
}
