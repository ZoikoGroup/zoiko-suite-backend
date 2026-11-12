//go:build integration

// Outbox relay claims (J-15) and parked-event recovery (J-16), against a real Postgres.
package registryit_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// recordingWriter stands in for Kafka: it remembers every key it was given.
type recordingWriter struct {
	mu   sync.Mutex
	keys []string
	fail bool
}

func (w *recordingWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail {
		return context.DeadlineExceeded
	}
	for _, m := range msgs {
		w.keys = append(w.keys, string(m.Key))
	}
	return nil
}

func (w *recordingWriter) withPrefix(prefix string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, k := range w.keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

func addOutboxEvents(t *testing.T, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		require.NoError(t, st.AddEventToOutbox(ctx, pool, "j15.test", "t", prefix+uuid.NewString()[:8], []byte(`{"event_id":"`+uuid.NewString()+`"}`)))
	}
}

func pendingWithPrefix(t *testing.T, prefix string) int {
	t.Helper()
	return countRows(t, `SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND partition_key LIKE $1`, prefix+"%")
}

// Two relays (two replicas) over one outbox: every event is published exactly once.
func TestJ15_TwoRelaysPublishEachEventOnce(t *testing.T) {
	prefix := "j15-" + uuid.NewString()[:8] + "-"
	addOutboxEvents(t, prefix, 40)
	w1, w2 := &recordingWriter{}, &recordingWriter{}
	r1 := events.NewOutboxWorker(zap.NewNop(), st, w1, "t", nil)
	r2 := events.NewOutboxWorker(zap.NewNop(), st, w2, "t", nil)
	for round := 0; round < 20 && pendingWithPrefix(t, prefix) > 0; round++ {
		var wg sync.WaitGroup
		for _, r := range []*events.OutboxWorker{r1, r2} {
			wg.Add(1)
			go func(r *events.OutboxWorker) { defer wg.Done(); r.RelayOnce() }(r)
		}
		wg.Wait()
	}
	require.Equal(t, 0, pendingWithPrefix(t, prefix))
	got := append(w1.withPrefix(prefix), w2.withPrefix(prefix)...)
	require.Len(t, got, 40, "40 events, 40 publishes")
	seen := map[string]bool{}
	for _, k := range got {
		require.False(t, seen[k], "event %s published twice", k)
		seen[k] = true
	}
}

// The claim is FOR UPDATE SKIP LOCKED: a second transaction never receives a
// row the first one holds.
func TestJ15_ClaimsAreDisjoint(t *testing.T) {
	prefix := "j15c-" + uuid.NewString()[:8] + "-"
	addOutboxEvents(t, prefix, 6)
	c1, u1 := st.BeginUnit(ctx, "")
	defer u1.Rollback(c1)
	first, err := st.ClaimOutboxEvents(c1, 1000)
	require.NoError(t, err)
	held := map[string]bool{}
	for _, e := range first {
		held[e.OutboxID] = true
	}
	c2, u2 := st.BeginUnit(ctx, "")
	defer u2.Rollback(c2)
	second, err := st.ClaimOutboxEvents(c2, 1000)
	require.NoError(t, err)
	for _, e := range second {
		require.False(t, held[e.OutboxID], "row %s was claimed by both", e.OutboxID)
	}
	ok, err := st.TryLockOutboxRelay(c1)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = st.TryLockOutboxRelay(c2)
	require.NoError(t, err)
	require.False(t, ok, "one relay at a time")
}

func outboxAdminEnv(auth *recordingAuthz) http.Handler {
	r := newRouter()
	h := handler.New(st, auth, &spyPub{}, "platform-scope", zap.NewNop()).WithRegistry(st, &spyPub{}).WithOutboxRecovery(st)
	handler.RegisterRoutes(r, h)
	return r
}

func TestJ16_AParkedEventIsListedRequeuedAndPublished(t *testing.T) {
	prefix := "j16-" + uuid.NewString()[:8] + "-"
	addOutboxEvents(t, prefix, 1)
	var id string
	require.NoError(t, pool.QueryRow(ctx, `UPDATE event_outbox SET publish_attempts=$2, last_error='broker refused: message too large'
		WHERE partition_key LIKE $1 RETURNING outbox_id::text`, prefix+"%", store.MaxOutboxAttempts).Scan(&id))

	// Parked: the relay no longer picks it up.
	w := &recordingWriter{}
	events.NewOutboxWorker(zap.NewNop(), st, w, "t", nil).RelayOnce()
	require.Empty(t, w.withPrefix(prefix))

	auth := &recordingAuthz{}
	e := &env{srv: outboxAdminEnv(auth), auth: auth}
	code, page := e.do("GET", "/v1/admin/outbox/parked?page_size=200", "release-mgr", nil)
	require.Equal(t, 200, code, "%v", page)
	found := false
	for _, it := range page["items"].([]any) {
		if it.(map[string]any)["outbox_id"] == id {
			found = true
			require.Equal(t, "broker refused: message too large", it.(map[string]any)["last_error"])
		}
	}
	require.True(t, found, "the parked event is listed")
	require.True(t, auth.pairs["jurisdiction_outbox_event.view"])

	code, body := e.do("POST", "/v1/admin/outbox/"+id+"/requeue", "release-mgr", map[string]any{})
	require.Equal(t, 400, code, "%v", body)
	require.Equal(t, "missing_field", body["code"])

	code, body = e.do("POST", "/v1/admin/outbox/"+id+"/requeue", "release-mgr",
		map[string]any{"reason": "broker max.message.bytes raised", "evidence_ref": "INC-4242"})
	require.Equal(t, 200, code, "%v", body)
	require.Equal(t, float64(store.MaxOutboxAttempts), body["attempts_before"])
	require.Equal(t, "release-mgr", body["requested_by"])
	require.Equal(t, 0, countRows(t, `SELECT publish_attempts FROM event_outbox WHERE outbox_id=$1`, id))

	code, body = e.do("POST", "/v1/admin/outbox/"+id+"/requeue", "release-mgr", map[string]any{"reason": "again"})
	require.Equal(t, 409, code, "%v", body)
	require.Equal(t, "outbox_event_not_parked", body["code"])
	code, _ = e.do("POST", "/v1/admin/outbox/"+uuid.NewString()+"/requeue", "release-mgr", map[string]any{"reason": "x"})
	require.Equal(t, 404, code)

	// Requeued: the next relay publishes it.
	events.NewOutboxWorker(zap.NewNop(), st, w, "t", nil).RelayOnce()
	require.Len(t, w.withPrefix(prefix), 1)

	// The requeue log is evidence: append-only.
	_, err := pool.Exec(ctx, `UPDATE outbox_requeues SET reason='edited' WHERE outbox_id=$1`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM outbox_requeues WHERE outbox_id=$1`, id)
	require.Error(t, err)
}

func TestJ16_RequeueIsAuthorized(t *testing.T) {
	auth := &recordingAuthz{deny: "jurisdiction_outbox_event.requeue"}
	e := &env{srv: outboxAdminEnv(auth), auth: auth}
	code, body := e.do("POST", "/v1/admin/outbox/"+uuid.NewString()+"/requeue", "intruder", map[string]any{"reason": "x"})
	require.Equal(t, 403, code, "%v", body)
}

// R4: 000033 reverts and re-applies on a database that has used it. Run in one
// transaction (DDL is transactional) so the suite's schema is untouched.
func TestMigration000033_DownThenUp(t *testing.T) {
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	down, up := readMigration(t, "000033_outbox_requeues.down.sql"), readMigration(t, "000033_outbox_requeues.up.sql")

	// The down refuses while a requeue is recorded (it is evidence).
	_, err = tx.Exec(ctx, `ALTER TABLE outbox_requeues DISABLE TRIGGER USER; DELETE FROM outbox_requeues;
		ALTER TABLE outbox_requeues ENABLE TRIGGER USER;
		INSERT INTO event_outbox (outbox_id, event_type, event_payload, topic) VALUES ('00000000-0000-4000-8000-000000000033', 'x', '{}', 't');
		INSERT INTO outbox_requeues (outbox_id, event_type, attempts_before, reason, requested_by)
		VALUES ('00000000-0000-4000-8000-000000000033', 'x', 10, 'probe', 'test')`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SAVEPOINT d`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, down)
	require.ErrorContains(t, err, "000033 down refused")
	_, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT d; ALTER TABLE outbox_requeues DISABLE TRIGGER USER; DELETE FROM outbox_requeues; ALTER TABLE outbox_requeues ENABLE TRIGGER USER`)
	require.NoError(t, err)

	_, err = tx.Exec(ctx, down)
	require.NoError(t, err, "down")
	var n int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name='outbox_requeues'`).Scan(&n))
	require.Equal(t, 0, n)
	_, err = tx.Exec(ctx, up)
	require.NoError(t, err, "up again")
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name='outbox_requeues'`).Scan(&n))
	require.Equal(t, 1, n)
}
