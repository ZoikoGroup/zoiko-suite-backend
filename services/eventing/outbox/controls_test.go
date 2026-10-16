package outbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/eventing/integration"
	"zoiko.io/eventing/repair"
)

var testAuth = repair.Authorization{Operator: "ops-1", Approver: "lead-1", ApprovalRef: "CHG-7", Reason: "consumer fixed"}

func quarantine(t *testing.T, pool *pgxpool.Pool, r *Relay, w *fakeWriter, id string) {
	t.Helper()
	w.fail = func(Message) error { return Permanent(errors.New("rejected")) }
	if _, err := r.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readRow(t, pool, id); got.state != StateQuarantined {
		t.Fatalf("setup: state %s", got.state)
	}
}

func repairCount(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_repair_log WHERE event_id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- privileged repair -------------------------------------------------------

func TestRequeueRefusedWithoutApprovalChangesNothing(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)
	w := &fakeWriter{}
	r := newRelay(t, pool, w, fastConfig())
	quarantine(t, pool, r, w, e.ID)

	for name, a := range map[string]repair.Authorization{
		"none":          {},
		"self-approved": {Operator: "x", Approver: "X", ApprovalRef: "c", Reason: "r"},
		"no reason":     {Operator: "a", Approver: "b", ApprovalRef: "c"},
	} {
		if ok, err := r.Requeue(context.Background(), e.ID, a); err == nil || ok {
			t.Errorf("%s: privileged repair without approval must be refused", name)
		}
	}
	if got := readRow(t, pool, e.ID); got.state != StateQuarantined {
		t.Fatalf("refused repair changed state to %s", got.state)
	}
	if repairCount(t, pool, e.ID) != 0 {
		t.Fatal("refused repairs must not be logged as performed")
	}
}

func TestRequeueIsLoggedWithPriorState(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)
	w := &fakeWriter{}
	r := newRelay(t, pool, w, fastConfig())
	quarantine(t, pool, r, w, e.ID)
	if ok, err := r.Requeue(context.Background(), e.ID, testAuth); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	var op, ap, prior, action, tenant string
	if err := pool.QueryRow(context.Background(),
		`SELECT operator, approver, prior_state, action, tenant_id FROM eventing_repair_log WHERE event_id=$1`, e.ID).
		Scan(&op, &ap, &prior, &action, &tenant); err != nil {
		t.Fatal(err)
	}
	if op != "ops-1" || ap != "lead-1" || prior != StateQuarantined || action != "requeue" || tenant != "tenant-1" {
		t.Fatalf("log = %s %s %s %s %s", op, ap, prior, action, tenant)
	}
	if ok, _ := r.Requeue(context.Background(), "no-such-event", testAuth); ok {
		t.Fatal("unknown event must report false")
	}
}

func TestRepairCannotMutatePayload_Hardening(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE eventing_outbox SET payload = '\x7b7d'::bytea`,
		`UPDATE eventing_outbox SET payload_hash = 'sha256:x'`,
		`UPDATE eventing_outbox SET event_id = 'other'`,
		`UPDATE eventing_outbox SET tenant_id = 'tenant-2'`,
		`UPDATE eventing_outbox SET region = 'us'`,
		`UPDATE eventing_outbox SET partition_key = 'k'`,
	} {
		if _, err := pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%s must be refused", stmt)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE eventing_outbox SET publish_state='failed', attempt_count=1`); err != nil {
		t.Fatalf("state changes must still be allowed: %v", err)
	}
}

// ---- controlled replay -------------------------------------------------------

func publishAll(t *testing.T, pool *pgxpool.Pool, r *Relay, ids ...string) {
	t.Helper()
	drainUntilIdle(t, r, 5)
	for _, id := range ids {
		if got := readRow(t, pool, id); got.state != StatePublished {
			t.Fatalf("setup: %s is %s", id, got.state)
		}
	}
}

func TestReplayRepublishesUnderOriginalID_INT_NP_010(t *testing.T) {
	pool := openPool(t)
	a, b := newEnvelope(t, "j-1"), newEnvelope(t, "j-2")
	enqueue(t, pool, a, b)
	w := &fakeWriter{}
	r := newRelay(t, pool, w, fastConfig())
	publishAll(t, pool, r, a.ID, b.ID)

	n, err := r.Replay(context.Background(), []string{a.ID}, testAuth)
	if err != nil || n != 1 {
		t.Fatalf("replay: %d %v", n, err)
	}
	drainUntilIdle(t, r, 5)
	count := map[string]int{}
	for _, id := range w.ids() {
		count[id]++
	}
	if count[a.ID] != 2 || count[b.ID] != 1 {
		t.Fatalf("deliveries = %v, want %s twice and %s once", count, a.ID, b.ID)
	}
	if repairCount(t, pool, a.ID) != 1 || repairCount(t, pool, b.ID) != 0 {
		t.Fatal("exactly the replayed event must be logged")
	}
	if got := readRow(t, pool, a.ID); got.state != StatePublished {
		t.Fatalf("after replay state = %s", got.state)
	}
}

func TestReplayIsAllOrNothing(t *testing.T) {
	pool := openPool(t)
	pub, pending := newEnvelope(t, "j-1"), newEnvelope(t, "j-2")
	enqueue(t, pool, pub)
	w := &fakeWriter{}
	r := newRelay(t, pool, w, fastConfig())
	publishAll(t, pool, r, pub.ID)
	enqueue(t, pool, pending)

	ctx := context.Background()
	if _, err := r.Replay(ctx, []string{pub.ID, pending.ID}, testAuth); err == nil {
		t.Fatal("replaying an unpublished event must be refused")
	}
	if _, err := r.Replay(ctx, []string{pub.ID, "ghost"}, testAuth); err == nil {
		t.Fatal("replaying an unknown event must be refused")
	}
	if got := readRow(t, pool, pub.ID); got.state != StatePublished || repairCount(t, pool, pub.ID) != 0 {
		t.Fatal("a refused replay must change and log nothing")
	}
}

func TestReplayNeedsApprovalAndBoundedBatch(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)
	r := newRelay(t, pool, &fakeWriter{}, fastConfig())
	publishAll(t, pool, r, e.ID)
	ctx := context.Background()
	if _, err := r.Replay(ctx, []string{e.ID}, repair.Authorization{}); err == nil {
		t.Error("replay without approval accepted")
	}
	if _, err := r.Replay(ctx, nil, testAuth); err == nil {
		t.Error("empty replay accepted")
	}
	big := make([]string, MaxReplayBatch+1)
	for i := range big {
		big[i] = fmt.Sprintf("e%d", i)
	}
	if _, err := r.Replay(ctx, big, testAuth); err == nil {
		t.Error("oversized replay accepted")
	}
}

func TestRepairCannotCrossRegions_INT_NP_045(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1") // residency region "uk"
	enqueue(t, pool, e)
	w := &fakeWriter{}
	r := newRelay(t, pool, w, fastConfig())
	publishAll(t, pool, r, e.ID)

	r.SetRegion("eu-west")
	_, err := r.Replay(context.Background(), []string{e.ID}, testAuth)
	if !errors.Is(err, integration.New(integration.CodeResidencyDenied, "")) {
		t.Fatalf("want INT-REG-002, got %v", err)
	}
	if repairCount(t, pool, e.ID) != 0 {
		t.Fatal("blocked replay must not be logged as performed")
	}

	e2 := newEnvelope(t, "j-2")
	enqueue(t, pool, e2)
	r.SetRegion("")
	w.fail = func(Message) error { return Permanent(errors.New("x")) }
	drainUntilIdle(t, r, 3)
	r.SetRegion("eu-west")
	if ok, err := r.Requeue(context.Background(), e2.ID, testAuth); ok || !errors.Is(err, integration.New(integration.CodeResidencyDenied, "")) {
		t.Fatalf("requeue across regions: %v %v", ok, err)
	}
	r.SetRegion("uk")
	w.fail = nil
	if ok, err := r.Requeue(context.Background(), e2.ID, testAuth); err != nil || !ok {
		t.Fatalf("same-region requeue: %v %v", ok, err)
	}
}

// ---- producer schema gate -----------------------------------------------------

type fakeRegistry struct {
	schemas map[string]string
	down    atomic.Bool
	calls   atomic.Int32
}

func (f *fakeRegistry) Schema(_ context.Context, name string, _ int) (*integration.Schema, error) {
	f.calls.Add(1)
	if f.down.Load() {
		return nil, fmt.Errorf("%w: test outage", integration.ErrRegistryUnavailable)
	}
	s, ok := f.schemas[name]
	if !ok {
		return nil, integration.Wrap(integration.CodeSchemaMismatch, "none", integration.ErrNoContract)
	}
	return &integration.Schema{EventName: name, Version: 3, JSONSchema: []byte(s)}, nil
}

const journalSchema = `{"type":"object","required":["journal_id","amount"],"properties":{"journal_id":{"type":"string"},"amount":{"type":"integer"}}}`

func TestGateQuarantinesIncompatiblePayloadBeforePublish_INT_NP_006(t *testing.T) {
	pool := openPool(t)
	bad := newEnvelope(t, "j-bad") // data has journal_id only: "amount" is required
	enqueue(t, pool, bad)
	w := &fakeWriter{}
	r := newRelay(t, pool, w, fastConfig())
	r.SetGate(&integration.SchemaGate{Registry: &fakeRegistry{schemas: map[string]string{"journal.posted": journalSchema}}})
	if _, err := r.DrainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := readRow(t, pool, bad.ID)
	if got.state != StateQuarantined || got.code == nil || *got.code != CodeSchemaRejected {
		t.Fatalf("row = %+v", got)
	}
	if len(w.ids()) != 0 {
		t.Fatal("an incompatible event must never reach the broker")
	}
}

func TestGateLetsCompatibleEventsThroughAndIsolatesPoison(t *testing.T) {
	pool := openPool(t)
	good := newEnvelope(t, "j-1")
	enqueue(t, pool, good)
	// a second, conforming schema for the same name: make the event valid by relaxing the contract
	reg := &fakeRegistry{schemas: map[string]string{"journal.posted": `{"type":"object","required":["journal_id"],"properties":{"journal_id":{"type":"string"}}}`}}
	w := &fakeWriter{}
	r := newRelay(t, pool, w, fastConfig())
	r.SetGate(&integration.SchemaGate{Registry: reg})
	drainUntilIdle(t, r, 3)
	if got := readRow(t, pool, good.ID); got.state != StatePublished {
		t.Fatalf("compatible event state = %s", got.state)
	}
}

func TestGateFailsClosedWhenRegistryDown_HoldsWithoutSpendingBudget(t *testing.T) {
	pool := openPool(t)
	e := newEnvelope(t, "j-1")
	enqueue(t, pool, e)
	reg := &fakeRegistry{schemas: map[string]string{"journal.posted": `{"type":"object","required":["journal_id"]}`}}
	reg.down.Store(true)
	w := &fakeWriter{}
	r := newRelay(t, pool, w, fastConfig())
	r.SetGate(&integration.SchemaGate{Registry: reg})

	for i := 0; i < 5; i++ { // more passes than MaxAttempts: budget must not be spent
		time.Sleep(5 * time.Millisecond)
		if _, err := r.DrainOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := readRow(t, pool, e.ID)
	if got.state == StateQuarantined || got.attempts != 0 || len(w.ids()) != 0 {
		t.Fatalf("outage must hold, not quarantine or publish: %+v delivered=%v", got, w.ids())
	}
	if got.code == nil || *got.code != CodeGateUnavailable {
		t.Fatalf("code = %v", got.code)
	}
	reg.down.Store(false)
	drainUntilIdle(t, r, 5)
	if got := readRow(t, pool, e.ID); got.state != StatePublished {
		t.Fatalf("after recovery state = %s", got.state)
	}
}

func TestGateUnregisteredPolicy(t *testing.T) {
	for _, allow := range []bool{false, true} {
		pool := openPool(t)
		e := newEnvelope(t, "j-1")
		enqueue(t, pool, e)
		w := &fakeWriter{}
		r := newRelay(t, pool, w, fastConfig())
		r.SetGate(&integration.SchemaGate{Registry: &fakeRegistry{schemas: map[string]string{}}, AllowUnregistered: allow})
		drainUntilIdle(t, r, 3)
		want := StateQuarantined
		if allow {
			want = StatePublished
		}
		if got := readRow(t, pool, e.ID); got.state != want {
			t.Errorf("AllowUnregistered=%v: state %s, want %s", allow, got.state, want)
		}
	}
}

func TestGateCachesContractsButNotOutages(t *testing.T) {
	reg := &fakeRegistry{schemas: map[string]string{"journal.posted": journalSchema}}
	now := time.Now()
	g := &integration.SchemaGate{Registry: reg, TTL: time.Minute, Now: func() time.Time { return now }}
	payload := []byte(`{"data":{"journal_id":"a","amount":1}}`)
	for i := 0; i < 5; i++ {
		if err := g.Check(context.Background(), "com.zoikosuite.accounting.journal.posted", payload); err != nil {
			t.Fatal(err)
		}
	}
	if reg.calls.Load() != 1 {
		t.Fatalf("registry hit %d times, want 1", reg.calls.Load())
	}
	now = now.Add(2 * time.Minute)
	_ = g.Check(context.Background(), "com.zoikosuite.accounting.journal.posted", payload)
	if reg.calls.Load() != 2 {
		t.Fatalf("expired entry must refetch: %d", reg.calls.Load())
	}
	reg.down.Store(true)
	g2 := &integration.SchemaGate{Registry: reg, TTL: time.Minute}
	for i := 0; i < 3; i++ {
		if err := g2.Check(context.Background(), "com.zoikosuite.accounting.journal.posted", payload); !errors.Is(err, integration.ErrRegistryUnavailable) {
			t.Fatalf("outage must surface every time: %v", err)
		}
	}
}

func TestEventNameResolution(t *testing.T) {
	cases := []struct{ typ, body, want string }{
		{"com.zoikosuite.accounting.journal.posted", `{}`, "journal.posted"},
		{"com.zoikosuite.accounting.journal.posted", `{"event_type":"gl.journal.posted"}`, "gl.journal.posted"},
		{"journal.posted", `{}`, "journal.posted"},
		{"com.zoikosuite.x.y", `{}`, "com.zoikosuite.x.y"}, // too short to reduce: left alone
	}
	for _, c := range cases {
		if got := integration.EventName(c.typ, []byte(c.body)); got != c.want {
			t.Errorf("%s %s -> %s, want %s", c.typ, c.body, got, c.want)
		}
	}
}

// ---- alerts -------------------------------------------------------------------

func TestEvaluateAlerts(t *testing.T) {
	if a := Evaluate(Stats{Backlog: 3, OldestBacklogAge: time.Second}, Thresholds{MaxBacklogAge: time.Minute, MaxBacklog: 100}); len(a) != 0 {
		t.Fatalf("healthy outbox alerted: %v", a)
	}
	a := Evaluate(Stats{Backlog: 3, OldestBacklogAge: 5 * time.Minute, Quarantined: 2}, Thresholds{MaxBacklogAge: time.Minute})
	if len(a) != 2 || a[0].Event != "outbox_publish_delayed" || a[1].Event != "outbox_quarantined" {
		t.Fatalf("alerts = %+v", a)
	}
	if a := Evaluate(Stats{Backlog: 500}, Thresholds{MaxBacklog: 100}); len(a) != 1 || !strings.Contains(a[0].Detail, "500") {
		t.Fatalf("size alert = %+v", a)
	}
	if a := Evaluate(Stats{Backlog: 500, OldestBacklogAge: time.Hour}, Thresholds{}); len(a) != 0 {
		t.Fatalf("zero thresholds mean unchecked: %v", a)
	}
}

// ---- partition-key certification (INT-CTRL-006, INT-NP-009) --------------------

func TestVerifyPartitioning(t *testing.T) {
	pool := openPool(t)
	r := newRelay(t, pool, &fakeWriter{}, fastConfig())
	ctx := context.Background()
	enqueue(t, pool, newEnvelope(t, "agg-1"), newEnvelope(t, "agg-1"), newEnvelope(t, "agg-2"))

	v, err := r.VerifyPartitioning(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(v) != 0 {
		t.Fatalf("envelope-derived keys must certify clean: %v %v", v, err)
	}

	// A producer that invents its own key for agg-1 breaks per-aggregate ordering.
	if _, err := pool.Exec(ctx, `
		INSERT INTO eventing_outbox (outbox_id, event_id, event_type, schema_version, tenant_id, region,
		                             aggregate_id, aggregate_version, partition_key, payload, payload_hash)
		SELECT gen_random_uuid(), 'rogue-1', event_type, schema_version, tenant_id, region,
		       aggregate_id, aggregate_version, 'orders', payload, payload_hash
		  FROM eventing_outbox WHERE aggregate_id = 'agg-1' LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	v, err = r.VerifyPartitioning(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, x := range v {
		kinds[x.Kind] = true
	}
	if !kinds["mixed-keys"] || !kinds["malformed-key"] {
		t.Fatalf("violations = %+v, want mixed-keys and malformed-key", v)
	}
	if v, _ := r.VerifyPartitioning(ctx, time.Now().Add(time.Hour)); len(v) != 0 {
		t.Fatalf("window excluding the events must be clean: %v", v)
	}
}
