package outbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/privacy-consent-svc/internal/testdb"
)

// These tests run against a REAL PostgreSQL with the service's real
// migrations, connected as a restricted role (see package testdb) so that
// FORCE ROW LEVEL SECURITY on event_outbox is genuinely in force. Kafka is
// the only fake: broker behavior is what is being simulated, not something
// under test. They skip without TEST_DATABASE_URL (and FAIL, rather than
// skip, when REQUIRE_DB_TESTS=1).

type fakeWriter struct {
	mu      sync.Mutex
	msgs    []kafka.Message
	err     error
	onWrite func() // runs inside WriteMessages, before it returns
}

func (w *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	w.mu.Lock()
	fn := w.onWrite
	err := w.err
	w.mu.Unlock()
	if fn != nil {
		fn()
	}
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.msgs = append(w.msgs, msgs...)
	w.mu.Unlock()
	return nil
}

func (w *fakeWriter) delivered() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.msgs)
}

func (w *fakeWriter) setErr(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

// enqueueAs writes one event the way production does: through the app role,
// inside a transaction scoped to the tenant (the policy's WITH CHECK admits
// the row only if tenant_id matches app.tenant_id).
func enqueueAs(t *testing.T, db *testdb.DB, tenant, eventType, correlation string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.App.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	payload, err := BuildPayload(eventType, "entity-1", tenant, "principal-1", correlation, map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := Enqueue(ctx, tx, Record{
		EventType: eventType, TenantID: tenant, EntityID: "entity-1",
		ActorID: "principal-1", CorrelationID: correlation, Payload: payload,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var id string
	if err := db.Admin.QueryRow(ctx,
		`SELECT outbox_event_id FROM event_outbox WHERE correlation_id = $1`, correlation).Scan(&id); err != nil {
		t.Fatalf("read back event id: %v", err)
	}
	return id
}

type outboxRow struct {
	published bool
	lastError *string
	attempts  int
	nextAt    time.Time
}

func rowOf(t *testing.T, db *testdb.DB, id string) outboxRow {
	t.Helper()
	var r outboxRow
	var pub *time.Time
	if err := db.Admin.QueryRow(context.Background(),
		`SELECT published_at, last_error, attempts, next_attempt_at FROM event_outbox WHERE outbox_event_id = $1`, id,
	).Scan(&pub, &r.lastError, &r.attempts, &r.nextAt); err != nil {
		t.Fatalf("read row %s: %v", id, err)
	}
	r.published = pub != nil
	return r
}

func makeDue(t *testing.T, db *testdb.DB, id string) {
	t.Helper()
	if _, err := db.Admin.Exec(context.Background(),
		`UPDATE event_outbox SET next_attempt_at = NOW() - interval '1 second' WHERE outbox_event_id = $1`, id); err != nil {
		t.Fatalf("make due: %v", err)
	}
}

func newRelay(db *testdb.DB, w MessageWriter) *Relay {
	return NewRelay(db.App, w, zap.NewNop())
}

const tenantA = "11111111-1111-1111-1111-111111111111"
const tenantB = "22222222-2222-2222-2222-222222222222"

// The control experiment: it proves the mechanism of the original bug under
// real RLS. With no capability, an UPDATE against event_outbox from the app
// role matches ZERO rows and raises NO error — which is exactly what the old
// bare pool.Exec bookkeeping did. With the capability it matches one.
func TestRLS_BareUpdateIsSilentlyInvisible_WithCapabilityItPersists(t *testing.T) {
	db := testdb.Setup(t)
	ctx := context.Background()
	id := enqueueAs(t, db, tenantA, "privacy.consent.changed", "corr-control")

	tag, err := db.App.Exec(ctx, `UPDATE event_outbox SET published_at = NOW() WHERE outbox_event_id = $1`, id)
	if err != nil {
		t.Fatalf("a bare UPDATE should not error — it should silently match nothing: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("RLS NOT IN FORCE: a bare app-role UPDATE (no tenant, no relay capability) matched %d rows", tag.RowsAffected())
	}
	if rowOf(t, db, id).published {
		t.Fatalf("the bare UPDATE must not have published the row")
	}

	tx, _ := db.App.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)")
	tag, err = tx.Exec(ctx, `UPDATE event_outbox SET published_at = NOW() WHERE outbox_event_id = $1`, id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("with app.outbox_relay the UPDATE must match exactly 1 row, got %d (err=%v)", tag.RowsAffected(), err)
	}
}

func TestRelay_SuccessfulPublish_MarksPublished_NeverRepublishes(t *testing.T) {
	db := testdb.Setup(t)
	ctx := context.Background()
	id := enqueueAs(t, db, tenantA, "privacy.consent.changed", "corr-success")

	w := &fakeWriter{}
	r := newRelay(db, w)

	n, err := r.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if n != 1 || w.delivered() != 1 {
		t.Fatalf("expected exactly 1 event claimed and delivered, got claimed=%d delivered=%d", n, w.delivered())
	}

	row := rowOf(t, db, id)
	if !row.published {
		t.Fatalf("BOOKKEEPING BUG: the broker acknowledged the event but published_at is still NULL — it would be re-sent forever")
	}
	if row.lastError != nil {
		t.Fatalf("a delivered event must carry no last_error, got %q", *row.lastError)
	}
	if r.BookkeepingFailures() != 0 {
		t.Fatalf("expected 0 bookkeeping failures, got %d", r.BookkeepingFailures())
	}

	// Even with the backoff long elapsed, a delivered event must never be
	// claimed again. Before the fix it was, every cycle, indefinitely.
	for i := 0; i < 3; i++ {
		makeDue(t, db, id)
		if _, err := r.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce #%d: %v", i+2, err)
		}
	}
	if w.delivered() != 1 {
		t.Fatalf("REPUBLISH BUG: a delivered event was sent %d times", w.delivered())
	}
}

func TestRelay_FailedPublish_PersistsLastError_StaysRetryable_ThenRecovers(t *testing.T) {
	db := testdb.Setup(t)
	ctx := context.Background()
	id := enqueueAs(t, db, tenantA, "privacy.consent.changed", "corr-fail")

	w := &fakeWriter{}
	w.setErr(errors.New("broker down: leader not available"))
	r := newRelay(db, w)

	if _, err := r.DrainOnce(ctx); err != nil {
		t.Fatalf("a Kafka failure with working bookkeeping must not surface as an error: %v", err)
	}
	row := rowOf(t, db, id)
	if row.published {
		t.Fatalf("an event the broker rejected must NOT be marked published")
	}
	if row.lastError == nil || *row.lastError != "broker down: leader not available" {
		t.Fatalf("recordFailure did not persist last_error under RLS, got %v", row.lastError)
	}
	if row.attempts != 1 {
		t.Fatalf("expected attempts=1 after one failed delivery, got %d", row.attempts)
	}
	if !row.nextAt.After(time.Now()) {
		t.Fatalf("expected next_attempt_at pushed into the future (backoff), got %v", row.nextAt)
	}
	if r.BookkeepingFailures() != 0 {
		t.Fatalf("expected 0 bookkeeping failures, got %d", r.BookkeepingFailures())
	}

	// Inside the backoff window: not claimed, not hammered.
	before := w.delivered()
	if n, _ := r.DrainOnce(ctx); n != 0 {
		t.Fatalf("an event inside its backoff window must not be claimed, got %d", n)
	}
	if w.delivered() != before {
		t.Fatalf("writer was called during the backoff window")
	}

	// Broker recovers; backoff elapses: it is retried and delivered.
	w.setErr(nil)
	makeDue(t, db, id)
	if n, err := r.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("retry: claimed=%d err=%v", n, err)
	}
	row = rowOf(t, db, id)
	if !row.published {
		t.Fatalf("retry succeeded at the broker but the event was not marked published")
	}
	if row.lastError != nil {
		t.Fatalf("a delivered event's stale last_error must be cleared, got %q", *row.lastError)
	}
	if row.attempts != 2 {
		t.Fatalf("expected attempts=2 (one failure, one success), got %d", row.attempts)
	}
	if w.delivered() != 1 {
		t.Fatalf("expected exactly one successful delivery, got %d", w.delivered())
	}
}

func TestRelay_MarkPublished_RecordFailure_Direct(t *testing.T) {
	db := testdb.Setup(t)
	ctx := context.Background()
	r := newRelay(db, &fakeWriter{})

	idFail := enqueueAs(t, db, tenantA, "privacy.consent.changed", "corr-direct-fail")
	if err := r.recordFailure(ctx, idFail, errors.New("boom")); err != nil {
		t.Fatalf("recordFailure on a live row: %v", err)
	}
	if row := rowOf(t, db, idFail); row.lastError == nil || *row.lastError != "boom" || row.published {
		t.Fatalf("recordFailure did not persist last_error and leave the row retryable: %+v", row)
	}

	idPub := enqueueAs(t, db, tenantA, "privacy.consent.changed", "corr-direct-pub")
	if err := r.markPublished(ctx, idPub); err != nil {
		t.Fatalf("markPublished on a live row: %v", err)
	}
	if !rowOf(t, db, idPub).published {
		t.Fatalf("markPublished did not persist under FORCE RLS")
	}

	// Silent no-ops are the bug: a missing row, and an already-published row,
	// must each be an error rather than a quiet zero-row UPDATE.
	if err := r.markPublished(ctx, "evt-does-not-exist"); err == nil {
		t.Fatalf("markPublished on a nonexistent row must error, it silently succeeded")
	}
	if err := r.markPublished(ctx, idPub); err == nil {
		t.Fatalf("markPublished on an already-published row must error (0 rows updated)")
	}
	if err := r.recordFailure(ctx, idPub, errors.New("late")); err == nil {
		t.Fatalf("recordFailure on an already-published row must error, and must not overwrite its state")
	}
	if rowOf(t, db, idPub).lastError != nil {
		t.Fatalf("recordFailure must never write last_error onto a delivered event")
	}
}

// If bookkeeping cannot persist, the relay must say so — the old code
// discarded the result and logged nothing the caller could act on.
func TestRelay_BookkeepingFailure_IsSurfaced_NotSilent(t *testing.T) {
	db := testdb.Setup(t)
	ctx := context.Background()
	id := enqueueAs(t, db, tenantA, "privacy.consent.changed", "corr-bk")

	w := &fakeWriter{}
	// Simulate a competing relay winning the race: while this relay is
	// inside the broker call, the row is marked published elsewhere, so this
	// relay's own markPublished matches nothing.
	w.onWrite = func() {
		_, _ = db.Admin.Exec(ctx, `UPDATE event_outbox SET published_at = NOW() WHERE outbox_event_id = $1`, id)
	}
	r := newRelay(db, w)

	_, err := r.DrainOnce(ctx)
	if err == nil {
		t.Fatalf("a bookkeeping UPDATE that matched 0 rows must surface as an error from DrainOnce")
	}
	if r.BookkeepingFailures() != 1 {
		t.Fatalf("expected BookkeepingFailures()=1, got %d", r.BookkeepingFailures())
	}
}

func TestRelay_NilPool_ReturnsError_DoesNotPanic(t *testing.T) {
	r := NewRelay(nil, &fakeWriter{}, zap.NewNop())
	if _, err := r.DrainOnce(context.Background()); !errors.Is(err, ErrNoPool) {
		t.Fatalf("expected ErrNoPool from a pool-less relay, got %v", err)
	}

	// Run's loop must also survive it (it used to dereference the nil pool
	// and take the whole process down).
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx, 10*time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after its context ended")
	}
}

// "Do not weaken RLS": the relay capability must unlock event_outbox and
// nothing else. Under it, another tenant's consent evidence stays invisible.
func TestRelayCapability_DoesNotWidenAccessToOtherTables(t *testing.T) {
	db := testdb.Setup(t)
	ctx := context.Background()

	for _, tenant := range []string{tenantA, tenantB} {
		if _, err := db.Admin.Exec(ctx, `
			INSERT INTO consent_receipts (tenant_id, subject_ref, purpose_id, action, capture_channel, actor_principal_id)
			VALUES ($1, 'subject-x', 'purpose-x', 'GRANTED', 'WEB', 'p')`, tenant); err != nil {
			t.Fatalf("seed consent receipt: %v", err)
		}
	}

	tx, err := db.App.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM consent_receipts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("TENANT ISOLATION BROKEN: with app.outbox_relay set, tenant A's session saw %d consent receipts (want exactly its own 1)", n)
	}
	var ok bool
	_ = pgx.ErrNoRows
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM consent_receipts WHERE tenant_id = $1)`, tenantB).Scan(&ok); err != nil || !ok {
		t.Fatalf("tenant B's receipt must be invisible under the relay capability (err=%v)", err)
	}
}

// Two relays draining at once must deliver each event exactly once — the
// claim (SKIP LOCKED + pushing next_attempt_at forward) is what guarantees it.
func TestRelay_ConcurrentRelays_DeliverEachEventOnce(t *testing.T) {
	db := testdb.Setup(t)
	ctx := context.Background()
	const total = 12
	for i := 0; i < total; i++ {
		enqueueAs(t, db, tenantA, "privacy.consent.changed", "corr-conc-"+string(rune('a'+i)))
	}

	w := &fakeWriter{}
	r1, r2 := newRelay(db, w), newRelay(db, w)
	var wg sync.WaitGroup
	for _, r := range []*Relay{r1, r2} {
		wg.Add(1)
		go func(r *Relay) {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				_, _ = r.DrainOnce(ctx)
			}
		}(r)
	}
	wg.Wait()

	if w.delivered() != total {
		t.Fatalf("expected each of %d events delivered exactly once across two relays, got %d deliveries", total, w.delivered())
	}
	var unpublished int
	if err := db.Admin.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE published_at IS NULL`).Scan(&unpublished); err != nil || unpublished != 0 {
		t.Fatalf("expected every event marked published, %d are not (err=%v)", unpublished, err)
	}
}
