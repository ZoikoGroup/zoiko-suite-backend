package decisionledger

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func sample(id string) Record {
	return Record{
		DecisionID: id, TenantID: "11111111-1111-1111-1111-111111111111", LegalEntityID: "22222222-2222-2222-2222-222222222222",
		ActionType: "ACCESS_PAYMENT_RELEASE", Outcome: "DENY", RuleBasis: "cfg.7 reasons:NO_GRANT",
		CorrelationID: "corr-1", DecidedAt: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC),
		EvaluationContext: map[string]any{OnBehalfOfKey: "person-1"},
	}
}

func TestNormalize_ClipsTextAndRefusesBadIdentifiers(t *testing.T) {
	r := sample("d-1")
	r.RuleBasis = strings.Repeat("r", 300)
	r.Title = strings.Repeat("t", 250)
	if err := r.Normalize(); err != nil {
		t.Fatal(err)
	}
	if len(r.RuleBasis) != 256 || len(r.Title) != 200 {
		t.Fatalf("clip: rule_basis %d title %d", len(r.RuleBasis), len(r.Title))
	}
	for name, mutate := range map[string]func(*Record){
		"no decision id":       func(r *Record) { r.DecisionID = "" },
		"long decision id":     func(r *Record) { r.DecisionID = strings.Repeat("x", 65) },
		"no correlation":       func(r *Record) { r.CorrelationID = "" },
		"long outcome":         func(r *Record) { r.Outcome = strings.Repeat("O", 33) },
		"no decided_at":        func(r *Record) { r.DecidedAt = time.Time{} },
		"no legal entity":      func(r *Record) { r.LegalEntityID = "" },
		"long correlation id":  func(r *Record) { r.CorrelationID = strings.Repeat("c", 65) },
		"no rule basis":        func(r *Record) { r.RuleBasis = "" },
		"no tenant":            func(r *Record) { r.TenantID = "" },
		"no action type":       func(r *Record) { r.ActionType = "" },
		"no outcome":           func(r *Record) { r.Outcome = "" },
		"long tenant":          func(r *Record) { r.TenantID = strings.Repeat("t", 65) },
		"long legal entity id": func(r *Record) { r.LegalEntityID = strings.Repeat("e", 65) },
	} {
		r := sample("d-2")
		mutate(&r)
		if err := r.Normalize(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: %v, want ErrInvalidRecord", name, err)
		}
	}
}

func TestHTTPSender_HeadersBodyAndStatusMapping(t *testing.T) {
	var mu sync.Mutex
	var got []*http.Request
	var bodies []map[string]any
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, b)
		mu.Unlock()
		w.WriteHeader(status)
	}))
	defer srv.Close()
	s := &HTTPSender{BaseURL: srv.URL + "/", Principal: "svc-authorization", KeyPrefix: "authorization-ledger",
		Purpose: "access_decision_evidence", Client: srv.Client()}

	rec := sample("ad-1")
	rec.CausationID = "cause-1"
	if err := s.Send(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	r, b := got[0], bodies[0]
	if r.URL.Path != "/v1/decisions" || r.Header.Get("X-Principal-Id") != "svc-authorization" ||
		r.Header.Get("X-Tenant-Id") != rec.TenantID || r.Header.Get("Idempotency-Key") != "authorization-ledger:ad-1" ||
		r.Header.Get("X-Correlation-ID") != "corr-1" || r.Header.Get("X-Causation-Id") != "cause-1" ||
		r.Header.Get("X-Purpose-Context") != "access_decision_evidence" || r.Header.Get("X-Source-Channel") != "system" {
		t.Fatalf("headers %v", r.Header)
	}
	if b["decision_id"] != "ad-1" || b["outcome"] != "DENY" || b["actor_id"] != nil {
		t.Fatalf("body %v", b)
	}
	if ec, _ := b["evaluation_context"].(map[string]any); ec[OnBehalfOfKey] != "person-1" {
		t.Fatalf("on_behalf_of: %v", b["evaluation_context"])
	}

	status = http.StatusOK // a replay
	if err := s.Send(context.Background(), rec); err != nil {
		t.Fatalf("200 replay: %v", err)
	}
	status = http.StatusConflict
	if err := s.Send(context.Background(), rec); !errors.Is(err, ErrPermanent) {
		t.Fatalf("409: %v, want ErrPermanent", err)
	}
	for _, st := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusServiceUnavailable} {
		status = st
		if err := s.Send(context.Background(), rec); err == nil || errors.Is(err, ErrPermanent) {
			t.Fatalf("%d: %v, want a retryable error", st, err)
		}
	}
	if err := (&HTTPSender{BaseURL: srv.URL, Client: srv.Client()}).Send(context.Background(), rec); err == nil {
		t.Fatal("no principal must not send")
	}
}

type fakeQueue struct {
	pending                    []Pending
	delivered, parked, retried []string
	next                       map[string]time.Time
}

func (q *fakeQueue) Claim(context.Context, int, time.Duration) ([]Pending, error) {
	p := q.pending
	q.pending = nil
	return p, nil
}
func (q *fakeQueue) MarkDelivered(_ context.Context, id string) error {
	q.delivered = append(q.delivered, id)
	return nil
}
func (q *fakeQueue) MarkRetry(_ context.Context, id, _ string, next time.Time) error {
	q.retried = append(q.retried, id)
	if q.next == nil {
		q.next = map[string]time.Time{}
	}
	q.next[id] = next
	return nil
}
func (q *fakeQueue) MarkParked(_ context.Context, id, _ string) error {
	q.parked = append(q.parked, id)
	return nil
}

type fakeSender map[string]error

func (f fakeSender) Send(_ context.Context, r Record) error { return f[r.DecisionID] }

func TestRelay_DeliversRetriesAndParks(t *testing.T) {
	q := &fakeQueue{pending: []Pending{
		{DecisionID: "ok", Record: sample("ok"), Attempts: 1},
		{DecisionID: "down", Record: sample("down"), Attempts: 3},
		{DecisionID: "conflict", Record: sample("conflict"), Attempts: 1},
	}}
	r := NewRelay(q, fakeSender{"down": errors.New("503"), "conflict": ErrPermanent}, time.Second, nil)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	var delivered, failed, parked int
	r.OnDelivered = func() { delivered++ }
	r.OnFailed = func(p bool) {
		failed++
		if p {
			parked++
		}
	}
	if n := r.RunOnce(context.Background()); n != 3 {
		t.Fatalf("claimed %d", n)
	}
	if strings.Join(q.delivered, ",") != "ok" || strings.Join(q.retried, ",") != "down" || strings.Join(q.parked, ",") != "conflict" {
		t.Fatalf("delivered %v retried %v parked %v", q.delivered, q.retried, q.parked)
	}
	if got := q.next["down"].Sub(now); got != 8*time.Second {
		t.Fatalf("backoff after 3 attempts = %s, want 8s", got)
	}
	if delivered != 1 || failed != 2 || parked != 1 {
		t.Fatalf("counters %d %d %d", delivered, failed, parked)
	}
	if backoff(20) != maxBackoff || backoff(0) != time.Second {
		t.Fatal("backoff bounds")
	}
}

// PgQueue against a real Postgres (TEST_DATABASE_URL), as a NOSUPERUSER
// NOBYPASSRLS role when the URL names one: enqueue in a tenant transaction,
// claim across tenants as the relay, lease, deliver, retry, park, backlog,
// purge, and RLS hides another tenant's rows outside the relay.
func TestPgQueue_Lifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_DB_TESTS") != "" {
			t.Fatal("TEST_DATABASE_URL is not set but REQUIRE_DB_TESTS is")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS decision_log_outbox`,
		`CREATE TABLE decision_log_outbox (
			decision_id VARCHAR(64) PRIMARY KEY, tenant_id VARCHAR(64) NOT NULL, record JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), attempts INT NOT NULL DEFAULT 0,
			next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(), delivered_at TIMESTAMPTZ, parked_at TIMESTAMPTZ, last_error TEXT)`,
		`ALTER TABLE decision_log_outbox ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE decision_log_outbox FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY decision_log_outbox_scope ON decision_log_outbox USING (
			tenant_id = current_setting('app.tenant_id', true) OR current_setting('app.decision_log_relay', true) = 'on')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	enqueue := func(tenant, id string) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
			t.Fatal(err)
		}
		rec := sample(id)
		rec.TenantID = tenant
		if err := Enqueue(ctx, tx, rec); err != nil {
			t.Fatal(err)
		}
		if err := Enqueue(ctx, tx, rec); err != nil { // the same decision twice: one row
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	enqueue("tenant-a", "a-1")
	enqueue("tenant-b", "b-1")
	enqueue("tenant-b", "b-2")

	// Outside the relay a tenant sees only its own rows.
	tx, _ := pool.Begin(ctx)
	_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id', 'tenant-a', true)`)
	var visible int
	_ = tx.QueryRow(ctx, `SELECT count(*) FROM decision_log_outbox`).Scan(&visible)
	_ = tx.Rollback(ctx)
	if visible != 1 {
		t.Fatalf("tenant-a sees %d rows, want 1", visible)
	}

	q := NewPgQueue(pool)
	claimed, err := q.Claim(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("claim: %d %v", len(claimed), err)
	}
	if again, _ := q.Claim(ctx, 10, time.Minute); len(again) != 0 {
		t.Fatalf("leased rows claimed again: %d", len(again))
	}
	if claimed[0].Record.Outcome != "DENY" || claimed[0].Attempts != 1 {
		t.Fatalf("claimed %+v", claimed[0])
	}
	if err := q.MarkDelivered(ctx, "a-1"); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkParked(ctx, "b-1", "409"); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkRetry(ctx, "b-2", "503", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	b, err := q.Backlog(ctx)
	if err != nil || b.Pending != 1 || b.Parked != 1 || b.OldestPending <= 0 {
		t.Fatalf("backlog %+v %v", b, err)
	}
	if retry, _ := q.Claim(ctx, 10, time.Minute); len(retry) != 1 || retry[0].DecisionID != "b-2" || retry[0].Attempts != 2 {
		t.Fatalf("retry claim %+v", retry)
	}
	if _, err := q.PurgeDelivered(ctx, time.Minute); err == nil {
		t.Fatal("purge below an hour must be refused")
	}
	q.now = func() time.Time { return time.Now().UTC().Add(48 * time.Hour) }
	if n, err := q.PurgeDelivered(ctx, 24*time.Hour); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
}

func TestRelay_PacesDeliveries(t *testing.T) {
	q := &fakeQueue{pending: []Pending{{DecisionID: "a", Record: sample("a")}, {DecisionID: "b", Record: sample("b")},
		{DecisionID: "c", Record: sample("c")}}}
	r := NewRelay(q, fakeSender{}, time.Second, nil)
	r.SetMaxPerSecond(20) // 50 ms apart
	start := time.Now()
	r.RunOnce(context.Background())
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Fatalf("three deliveries at 20/s took %s, want >= 100ms", took)
	}
	if len(q.delivered) != 3 {
		t.Fatalf("delivered %v", q.delivered)
	}
	r.SetMaxPerSecond(0)
	if r.gap != 0 {
		t.Fatal("0 removes the cap")
	}
}
