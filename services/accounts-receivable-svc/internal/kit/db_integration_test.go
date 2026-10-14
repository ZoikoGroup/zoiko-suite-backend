//go:build integration

package kit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/kit/kittest"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	var stop func()
	pool, stop = kittest.Start("ar_kit_test", 17300)
	code := m.Run()
	stop()
	os.Exit(code)
}

func caller(tenant string) kit.Caller {
	return kit.Caller{TenantID: tenant, PrincipalID: "p1", CorrelationID: "corr", LegalEntityID: uuid.NewString()}
}

func TestIdempotency_ReplayReuseAndRelease(t *testing.T) {
	db := kit.NewDB(pool)
	tenant := uuid.NewString()
	var calls int32
	h := db.IdemWrap("Op", true, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		kit.WriteJSON(w, http.StatusCreated, map[string]string{"id": "abc"})
	})
	do := func(key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		req.Header.Set("X-Tenant-Id", tenant)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	require.Equal(t, http.StatusBadRequest, do("", `{}`).Code, "key is mandatory on commands")

	first := do("k1", `{"a":1}`)
	require.Equal(t, http.StatusCreated, first.Code)
	replay := do("k1", `{"a":1}`)
	require.Equal(t, http.StatusCreated, replay.Code)
	require.Equal(t, "true", replay.Header().Get("Idempotent-Replay"))
	require.EqualValues(t, 1, atomic.LoadInt32(&calls), "the handler must run once")

	reuse := do("k1", `{"a":2}`)
	require.Equal(t, http.StatusUnprocessableEntity, reuse.Code)
	require.Contains(t, reuse.Body.String(), kit.CodeIdempotencyReused)

	// Another tenant may reuse the same key string.
	other := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":1}`))
	other.Header.Set("X-Tenant-Id", uuid.NewString())
	other.Header.Set("Idempotency-Key", "k1")
	rec := httptest.NewRecorder()
	h(rec, other)
	require.Equal(t, http.StatusCreated, rec.Code)
	require.EqualValues(t, 2, atomic.LoadInt32(&calls))
}

func TestIdempotency_FailedCommandReleasesClaim(t *testing.T) {
	db := kit.NewDB(pool)
	tenant := uuid.NewString()
	var calls int32
	h := db.IdemWrap("Op", true, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			kit.WriteError(w, http.StatusUnprocessableEntity, kit.CodeTaxDeterminationMissing, "")
			return
		}
		kit.WriteJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	})
	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`))
		req.Header.Set("X-Tenant-Id", tenant)
		req.Header.Set("Idempotency-Key", "kf")
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	require.Equal(t, http.StatusUnprocessableEntity, send())
	require.Equal(t, http.StatusOK, send(), "a failed command must be retryable with the same key")
}

func TestAudit_IsAppendOnly(t *testing.T) {
	db := kit.NewDB(pool)
	c := caller(uuid.NewString())
	ctx := context.Background()
	id := uuid.NewString()
	require.NoError(t, db.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: "T", AggregateID: id, Command: "Create",
			After: map[string]string{"k": "v"}, ReasonCode: "R"})
	}))
	err := db.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE ar_audit_events SET command = 'Edited' WHERE aggregate_id = $1`, id)
		return e
	})
	require.Error(t, err, "audit rows must not be updatable")
	err = db.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `DELETE FROM ar_audit_events WHERE aggregate_id = $1`, id)
		return e
	})
	require.Error(t, err, "audit rows must not be deletable")
}

type fakeLedger struct {
	answers []kit.PostingOutcome
	calls   int32
}

func (f *fakeLedger) Post(ctx context.Context, tenantID, principalID string, payload []byte) (kit.PostingOutcome, error) {
	i := int(atomic.AddInt32(&f.calls, 1)) - 1
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	return f.answers[i], nil
}

type recordingListener struct{ got []kit.PostingOutcome }

func (l *recordingListener) OnPostingOutcome(ctx context.Context, q kit.QueuedPosting, o kit.PostingOutcome) error {
	l.got = append(l.got, o)
	return nil
}

func TestPostingQueue_DurableIdempotentAndRetried(t *testing.T) {
	db := kit.NewDB(pool)
	c := caller(uuid.NewString())
	ctx := context.Background()
	srcID := uuid.NewString()
	ev := kit.PostingEvent{
		SourceEventID: srcID, FiscalPeriod: "2026-09", Description: "AR invoice", TransactionCurrency: "USD",
		DocumentDate: "2026-09-01", PostingDate: "2026-09-01",
		Lines: []kit.PostingLine{
			{MappingKey: "AR_RECEIVABLE_STANDARD", Debit: kit.MustAmount("120")},
			{MappingKey: "AR_REVENUE_STANDARD", Credit: kit.MustAmount("120")},
		},
	}
	// Queued twice (a replayed command): still ONE obligation.
	for i := 0; i < 2; i++ {
		require.NoError(t, db.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
			return kit.QueuePosting(ctx, tx, c, kit.SourceInvoice, srcID, ev)
		}))
	}
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM ar_accounting_posting_requests WHERE source_event_id=$1`, srcID).Scan(&n))
	require.Equal(t, 1, n)

	// A rolled-back command leaves no obligation behind.
	rolled := uuid.NewString()
	ev2 := ev
	ev2.SourceEventID = rolled
	_ = db.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		require.NoError(t, kit.QueuePosting(ctx, tx, c, kit.SourceInvoice, rolled, ev2))
		return http.ErrAbortHandler
	})
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM ar_accounting_posting_requests WHERE source_event_id=$1`, rolled).Scan(&n))
	require.Equal(t, 0, n)

	// Locked period (412 → PENDING) is retried; then POSTED.
	led := &fakeLedger{answers: []kit.PostingOutcome{
		{Status: kit.PostingPending, Error: "period locked"},
		{Status: kit.PostingPosted, ExecutionID: uuid.NewString(), JournalID: "J-1"},
	}}
	d := kit.NewPostingDispatcher(db, led, "", zap.NewNop())
	l := &recordingListener{}
	d.Register(kit.SourceInvoice, l)

	require.Equal(t, 1, d.RunOnce(ctx))
	status, _, lastErr, found, err := db.PostingStatusFor(ctx, c.TenantID, srcID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, kit.PostingPending, status, "a transient failure must stay PENDING, not fail the document")
	require.Contains(t, lastErr, "period locked")
	require.Empty(t, l.got, "the owner is told only of final outcomes")

	// Backoff has pushed next_attempt_at out; pull it in to retry now.
	_, err = pool.Exec(ctx, `UPDATE ar_accounting_posting_requests SET next_attempt_at = now() WHERE source_event_id=$1`, srcID)
	require.NoError(t, err)
	require.Equal(t, 1, d.RunOnce(ctx))
	status, journal, _, _, err := db.PostingStatusFor(ctx, c.TenantID, srcID)
	require.NoError(t, err)
	require.Equal(t, kit.PostingPosted, status)
	require.Equal(t, "J-1", journal)
	require.Len(t, l.got, 1)
	require.Equal(t, 0, d.RunOnce(ctx), "nothing left to deliver")
}

func TestPostingQueue_RejectsUnbalancedEvent(t *testing.T) {
	db := kit.NewDB(pool)
	c := caller(uuid.NewString())
	ctx := context.Background()
	err := db.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		return kit.QueuePosting(ctx, tx, c, kit.SourceInvoice, "x", kit.PostingEvent{SourceEventID: "u",
			Lines: []kit.PostingLine{{MappingKey: "A", Debit: kit.MustAmount("5")}, {MappingKey: "B", Credit: kit.MustAmount("4")}}})
	})
	require.Error(t, err)
}
