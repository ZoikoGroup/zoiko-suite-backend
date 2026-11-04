package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	svcenvelope "zoiko.io/carta-svc/internal/envelope"
	"zoiko.io/carta-svc/internal/outbox"
	"zoiko.io/carta-svc/internal/store"
	"zoiko.io/carta-svc/internal/testdb"
)

type fakePub struct {
	mu   sync.Mutex
	sent map[string][]byte // outbox_event_id -> payload
	fail bool
}

func (f *fakePub) PublishOutbox(_ context.Context, id, _ string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return context.DeadlineExceeded
	}
	if f.sent == nil {
		f.sent = map[string][]byte{}
	}
	f.sent[id] = payload
	return nil
}

// outboxRows counts outbox rows for one aggregate, as the owner (no RLS).
func outboxRows(t *testing.T, pools *testdb.Pools, aggregateID string) int {
	t.Helper()
	var n int
	if err := pools.Admin.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, aggregateID).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// Saving an assessment writes its assessment.created event in the same
// transaction, carrying the platform envelope, actor and schema_version.
func TestOutbox_AssessmentWritesEventInSameTransaction(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	tenant := newTenant()
	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{TenantID: tenant, CorrelationID: "corr-1"})

	asm := evaluate("le-1", "subject-1", 20)
	asm.EvaluatedBy = "alice"
	if err := store.NewPgStore(app).SaveAssessment(ctx, tenant, asm); err != nil {
		t.Fatalf("save: %v", err)
	}
	var eventType, actor, corr string
	var payload []byte
	if err := pools.Admin.QueryRow(context.Background(), `
		SELECT event_type, actor_id, correlation_id, payload FROM outbox_events WHERE aggregate_id = $1`, asm.ID).
		Scan(&eventType, &actor, &corr, &payload); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if eventType != store.EventAssessmentCreated || actor != "alice" || corr != "corr-1" {
		t.Fatalf("outbox row = (%s, %s, %s)", eventType, actor, corr)
	}
	var env struct {
		SchemaVersion string `json:"schema_version"`
		TenantID      string `json:"tenant_id"`
		Payload       struct {
			ID          string  `json:"id"`
			TrustScore  float64 `json:"trust_score"`
			EvaluatedBy string  `json:"evaluated_by"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.SchemaVersion != "1.0" || env.TenantID != tenant || env.Payload.ID != asm.ID ||
		env.Payload.EvaluatedBy != "alice" || env.Payload.TrustScore != asm.TrustScore {
		t.Fatalf("envelope = %+v", env)
	}
	got, err := store.NewPgStore(app).GetAssessmentByID(ctx, tenant, asm.ID)
	if err != nil || got.EvaluatedBy != "alice" {
		t.Fatalf("evaluated_by not persisted: %+v, %v", got, err)
	}
}

// When the outbox insert fails, the assessment is rolled back with it:
// neither row exists. The failure is induced with a NOT VALID check
// constraint that rejects one marker legal entity only.
func TestOutbox_FailedEventRollsBackAssessment(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	tenant := newTenant()
	marker := "le-reject-" + uuid.NewString()[:8]
	cname := pgx.Identifier{"test_reject_" + strings.ReplaceAll(marker[10:], "-", "")}.Sanitize()
	if _, err := pools.Admin.Exec(context.Background(),
		`ALTER TABLE outbox_events ADD CONSTRAINT `+cname+` CHECK (legal_entity_id IS DISTINCT FROM '`+marker+`') NOT VALID`); err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pools.Admin.Exec(context.Background(), `ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS `+cname)
	})

	asm := evaluate(marker, "subject-1", 20)
	if err := store.NewPgStore(app).SaveAssessment(context.Background(), tenant, asm); err == nil {
		t.Fatal("save must fail when the outbox row cannot be written")
	}
	var n int
	if err := pools.Admin.QueryRow(context.Background(),
		`SELECT count(*) FROM carta_assessments WHERE legal_entity_id = $1`, marker).Scan(&n); err != nil {
		t.Fatalf("count assessments: %v", err)
	}
	var m int
	if err := pools.Admin.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE legal_entity_id = $1`, marker).Scan(&m); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n != 0 || m != 0 {
		t.Fatalf("after rollback: assessments=%d outbox=%d, want 0 and 0", n, m)
	}
}

// The relay, running as the ordinary app role, publishes the row (crossing
// tenants via app.outbox_relay) and marks it published; a failed publish is
// recorded and retried, never lost.
func TestOutbox_RelayPublishesAndMarks(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	tenant := newTenant()
	asm := evaluate("le-1", "subject-1", 20)
	if err := store.NewPgStore(app).SaveAssessment(context.Background(), tenant, asm); err != nil {
		t.Fatalf("save: %v", err)
	}
	var outboxID string
	if err := pools.Admin.QueryRow(context.Background(),
		`SELECT outbox_event_id::text FROM outbox_events WHERE aggregate_id = $1`, asm.ID).Scan(&outboxID); err != nil {
		t.Fatalf("outbox row: %v", err)
	}

	failing := &fakePub{fail: true}
	if _, err := outbox.NewRelay(app, failing, time.Second, 1000, zap.NewNop()).RelayOnce(context.Background()); err != nil {
		t.Fatalf("relay (failing broker): %v", err)
	}
	var attempts int
	var published *time.Time
	if err := pools.Admin.QueryRow(context.Background(),
		`SELECT publish_attempts, published_at FROM outbox_events WHERE outbox_event_id = $1`, outboxID).
		Scan(&attempts, &published); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if attempts < 1 || published != nil {
		t.Fatalf("after a failed publish: attempts=%d published=%v, want >=1 and nil", attempts, published)
	}

	pub := &fakePub{}
	relay := outbox.NewRelay(app, pub, time.Second, 1000, zap.NewNop())
	for i := 0; i < 20 && pub.sent[outboxID] == nil; i++ {
		if _, err := relay.RelayOnce(context.Background()); err != nil {
			t.Fatalf("relay: %v", err)
		}
	}
	if pub.sent[outboxID] == nil {
		t.Fatal("relay never published the assessment.created row")
	}
	if err := pools.Admin.QueryRow(context.Background(),
		`SELECT published_at FROM outbox_events WHERE outbox_event_id = $1`, outboxID).Scan(&published); err != nil || published == nil {
		t.Fatalf("row not marked published: %v %v", published, err)
	}
}

// Under FORCE RLS an app-role session with no tenant and no relay flag sees
// no outbox row, and a request-path insert for another tenant is refused.
func TestOutbox_RLS(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	tenant := newTenant()
	asm := evaluate("le-1", "subject-1", 20)
	if err := store.NewPgStore(app).SaveAssessment(context.Background(), tenant, asm); err != nil {
		t.Fatalf("save: %v", err)
	}
	if n := outboxRows(t, pools, asm.ID); n != 1 {
		t.Fatalf("owner sees %d rows, want 1", n)
	}
	var visible int
	if err := app.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&visible); err != nil {
		t.Fatalf("count: %v", err)
	}
	if visible != 0 {
		t.Fatalf("unscoped app session sees %d outbox rows, want 0", visible)
	}
	err := asTenant(t, app, newTenant(), func(tx pgx.Tx) error {
		_, err := outbox.Insert(context.Background(), tx, store.SourceService, outbox.Event{
			AggregateType: "x", AggregateID: uuid.NewString(), EventType: "forged", TenantID: tenant, Payload: map[string]string{},
		})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("cross-tenant outbox insert must be refused by RLS, got %v", err)
	}
}
