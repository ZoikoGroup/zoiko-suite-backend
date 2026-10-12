package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/domain"
	"zoiko.io/purchase-request-svc/internal/idempotency"
	svcmiddleware "zoiko.io/purchase-request-svc/internal/middleware"
	"zoiko.io/purchase-request-svc/internal/outbox"
	"zoiko.io/purchase-request-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies every migration from a
// clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set.
//
// It DROPs this service's tables, so the database name must contain "test".
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, strings.ToLower(strings.TrimPrefix(u.Path, "/")), "test",
		"refusing to DROP tables in a database not recognisably disposable")

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	for _, tbl := range []string{"idempotency_keys", "outbox_events", "purchase_request_history", "purchase_request_lines", "purchase_requests"} {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl+` CASCADE`)
	}
	_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS guard_purchase_request() CASCADE`)
	_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS guard_purchase_request_line() CASCADE`)
	_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS reject_pr_history_mutation() CASCADE`)

	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")
	migrations, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, migrations)
	sort.Strings(migrations)
	for _, m := range migrations {
		sql, err := os.ReadFile(m)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(sql))
		require.NoError(t, err, "migration %s", filepath.Base(m))
	}
	return pool
}

func newReq(tenantID string) *domain.PurchaseRequest {
	return &domain.PurchaseRequest{
		RequestID:              uuid.NewString(),
		TenantID:               tenantID,
		LegalEntityID:          uuid.NewString(),
		RequestedByPrincipalID: "requester",
		Description:            "40 laptops",
		CurrencyCode:           "EUR",
		BusinessPurpose:        "refresh",
		AttachmentRefs:         []string{"doc-1"},
		CorrelationID:          "corr-" + uuid.NewString(),
		Lines: []domain.RequestLine{
			{ItemRef: "LAP-1", Description: "laptop", Category: "IT", Quantity: 40, Amount: 48000, CurrencyCode: "EUR", AttachmentRefs: []string{}},
			{ItemRef: "BAG-1", Description: "bag", Category: "IT", Quantity: 40, Amount: 1000, CurrencyCode: "EUR", AttachmentRefs: []string{}},
		},
	}
}

type fixture struct {
	pool *pgxpool.Pool
	s    *store.PgStore
	ctx  context.Context
	tid  string
}

// setup returns the store under test. Migrations and the verification queries
// run as TEST_DATABASE_URL's role (the table owner). When
// TEST_APP_DATABASE_URL names an ordinary NOSUPERUSER NOBYPASSRLS role the
// STORE runs as that role instead — the role the service has in production
// once DB_USER names one — so the row-level-security policies actually bind it.
func setup(t *testing.T) fixture {
	pool := openTestPool(t)
	tid := uuid.NewString()
	storePool := pool
	if appDSN := os.Getenv("TEST_APP_DATABASE_URL"); appDSN != "" {
		app, err := pgxpool.New(context.Background(), appDSN)
		require.NoError(t, err)
		t.Cleanup(app.Close)
		storePool = app
	}
	return fixture{pool: pool, s: store.New(storePool, zap.NewNop()), ctx: svcmiddleware.WithTenant(context.Background(), tid), tid: tid}
}

func (f fixture) create(t *testing.T) *domain.PurchaseRequest {
	r := newReq(f.tid)
	created, err := f.s.CreateRequest(f.ctx, r)
	require.NoError(t, err)
	require.True(t, created)
	return r
}

func (f fixture) move(t *testing.T, r *domain.PurchaseRequest, from []domain.RequestStatus, to domain.RequestStatus, actor string) (*domain.PurchaseRequest, error) {
	v := r.Version
	return f.s.Apply(f.ctx, store.Transition{
		TenantID: f.tid, RequestID: r.RequestID, ExpectedVersion: &v, From: from, To: to,
		Action: string(to), Meta: store.Meta{Actor: actor, CorrelationID: "c1", Reason: "why"},
		Events: []store.EventSpec{{Type: "Evt" + string(to)}},
	})
}

func (f fixture) submitApprove(t *testing.T, r *domain.PurchaseRequest) *domain.PurchaseRequest {
	p, err := f.move(t, r, []domain.RequestStatus{domain.RequestStatusDraft}, domain.RequestStatusPending, "requester")
	require.NoError(t, err)
	a, err := f.move(t, p, []domain.RequestStatus{domain.RequestStatusPending}, domain.RequestStatusApproved, "approver")
	require.NoError(t, err)
	return a
}

func (f fixture) outboxTypes(t *testing.T, id string) []string {
	rows, err := f.pool.Query(context.Background(), `SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, outbox_event_id`, id)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func TestCreate_WritesLinesHistoryAndEventsWithAlias(t *testing.T) {
	f := setup(t)
	r := f.create(t)

	got, err := f.s.GetRequest(f.ctx, r.RequestID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.RequestStatusDraft, got.Status)
	assert.Equal(t, 1, got.Version)
	assert.Equal(t, 49000.0, got.Amount, "header amount is the sum of the lines")
	require.Len(t, got.Lines, 2)
	assert.Equal(t, "LAP-1", got.Lines[0].ItemRef)
	assert.Equal(t, []string{"doc-1"}, got.AttachmentRefs)
	assert.ElementsMatch(t, []string{"PurchaseRequisitionCreated", "purchase.request.created"}, f.outboxTypes(t, r.RequestID))

	h, err := f.s.GetHistory(f.ctx, r.RequestID)
	require.NoError(t, err)
	require.Len(t, h, 1)
	assert.Equal(t, "CREATED", h[0].Action)
}

func TestCreate_RetriedCorrelationID_OneRowOneEventSet(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	retry := newReq(f.tid)
	retry.CorrelationID = r.CorrelationID
	created, err := f.s.CreateRequest(f.ctx, retry)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, r.RequestID, retry.RequestID, "resolves to the original")
	assert.Len(t, retry.Lines, 2)
	assert.Len(t, f.outboxTypes(t, r.RequestID), 2, "no second event set")
}

func TestTenantIsolation(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	other := svcmiddleware.WithTenant(context.Background(), uuid.NewString())

	got, err := f.s.GetRequest(other, r.RequestID)
	require.NoError(t, err)
	assert.Nil(t, got)

	_, err = f.s.Apply(other, store.Transition{TenantID: uuid.NewString(), RequestID: r.RequestID,
		From: []domain.RequestStatus{domain.RequestStatusDraft}, To: domain.RequestStatusPending})
	assert.ErrorIs(t, err, domain.ErrRequestNotFound)

	list, err := f.s.ListRequests(f.ctx, domain.ListRequestsFilter{TenantID: f.tid})
	require.NoError(t, err)
	assert.Len(t, list, 1)
	list, err = f.s.ListRequests(other, domain.ListRequestsFilter{TenantID: uuid.NewString()})
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestMalformedUUID_ReadsAsAbsent(t *testing.T) {
	f := setup(t)
	got, err := f.s.GetRequest(f.ctx, "not-a-uuid")
	require.NoError(t, err)
	assert.Nil(t, got)
	_, err = f.s.Apply(f.ctx, store.Transition{TenantID: f.tid, RequestID: "not-a-uuid",
		From: []domain.RequestStatus{domain.RequestStatusDraft}, To: domain.RequestStatusPending})
	assert.ErrorIs(t, err, domain.ErrRequestNotFound)
}

func TestLifecycle_VersionsHistoryAndEvents(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	a := f.submitApprove(t, r)
	assert.Equal(t, domain.RequestStatusApproved, a.Status)
	assert.Equal(t, 3, a.Version)
	require.NotNil(t, a.ApprovedByPrincipalID)
	assert.Equal(t, "approver", *a.ApprovedByPrincipalID)

	h, err := f.s.GetHistory(f.ctx, r.RequestID)
	require.NoError(t, err)
	assert.Len(t, h, 3)
	assert.Equal(t, []string{"Evt" + "PENDING_APPROVAL", "Evt" + "APPROVED"}, f.outboxTypes(t, r.RequestID)[2:])
}

func TestStaleVersion_Refused(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	bad := 7
	_, err := f.s.Apply(f.ctx, store.Transition{TenantID: f.tid, RequestID: r.RequestID, ExpectedVersion: &bad,
		From: []domain.RequestStatus{domain.RequestStatusDraft}, To: domain.RequestStatusPending})
	assert.ErrorIs(t, err, domain.ErrStaleVersion)
}

// A failing guard (e.g. SoD) rolls back everything: no state change, no history
// row, no outbox event.
func TestGuardFailure_RollsBackHistoryAndOutbox(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	p, err := f.move(t, r, []domain.RequestStatus{domain.RequestStatusDraft}, domain.RequestStatusPending, "requester")
	require.NoError(t, err)
	before := len(f.outboxTypes(t, r.RequestID))

	_, err = f.s.Apply(f.ctx, store.Transition{TenantID: f.tid, RequestID: r.RequestID,
		From: []domain.RequestStatus{domain.RequestStatusPending}, To: domain.RequestStatusApproved,
		Meta:   store.Meta{Actor: "requester"},
		Guard:  func(*domain.PurchaseRequest) error { return domain.ErrSelfApprovalNotAllowed },
		Events: []store.EventSpec{{Type: "PurchaseRequisitionApproved"}}})
	assert.ErrorIs(t, err, domain.ErrSelfApprovalNotAllowed)

	got, _ := f.s.GetRequest(f.ctx, r.RequestID)
	assert.Equal(t, domain.RequestStatusPending, got.Status)
	assert.Equal(t, p.Version, got.Version)
	assert.Len(t, f.outboxTypes(t, r.RequestID), before)
	h, _ := f.s.GetHistory(f.ctx, r.RequestID)
	assert.Len(t, h, 2)
}

// §20 #2: an approved requisition cannot change before conversion.
func TestApprovedRequisition_CannotChangeBeforeConversion(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	a := f.submitApprove(t, r)
	bg := context.Background()

	// Raw SQL — the DB itself refuses, whatever the application does.
	_, err := f.pool.Exec(bg, `UPDATE purchase_requests SET amount = 1 WHERE request_id = $1`, a.RequestID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot change")
	_, err = f.pool.Exec(bg, `UPDATE purchase_request_lines SET amount = 1 WHERE request_id = $1`, a.RequestID)
	require.Error(t, err)
	_, err = f.pool.Exec(bg, `DELETE FROM purchase_request_lines WHERE request_id = $1`, a.RequestID)
	require.Error(t, err)
	_, err = f.pool.Exec(bg, `DELETE FROM purchase_requests WHERE request_id = $1`, a.RequestID)
	require.Error(t, err)

	got, _ := f.s.GetRequest(f.ctx, r.RequestID)
	assert.Equal(t, 49000.0, got.Amount)
	assert.Len(t, got.Lines, 2)
}

func TestAmendApproved_InvalidatesApprovalAndReplacesLines(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	a := f.submitApprove(t, r)

	upd, invalidated, err := f.s.AmendRequest(f.ctx, store.Amendment{
		TenantID: f.tid, RequestID: a.RequestID, Meta: store.Meta{Actor: "amender", Reason: "price change"},
		Apply: func(w *domain.PurchaseRequest) error {
			w.Lines = []domain.RequestLine{{ItemRef: "LAP-2", Category: "IT", Quantity: 10, Amount: 100, CurrencyCode: "EUR", AttachmentRefs: []string{}}}
			return nil
		},
	})
	require.NoError(t, err)
	assert.True(t, invalidated)
	assert.Equal(t, domain.RequestStatusDraft, upd.Status)
	assert.Equal(t, 100.0, upd.Amount)
	assert.Nil(t, upd.ApprovedByPrincipalID)
	assert.Equal(t, domain.BudgetNotChecked, upd.BudgetDecision)
	assert.Equal(t, 1, upd.ApprovalInvalidatedCount)
	assert.Equal(t, a.Version+1, upd.Version)
	require.Len(t, upd.Lines, 1)
	assert.Contains(t, f.outboxTypes(t, r.RequestID), "PurchaseRequisitionApprovalInvalidated")
	assert.Contains(t, f.outboxTypes(t, r.RequestID), "PurchaseRequisitionAmended")

	// Not approvable again without going through submit.
	_, err = f.move(t, upd, []domain.RequestStatus{domain.RequestStatusPending}, domain.RequestStatusApproved, "approver")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition)
}

// §5 #4: a cancelled requisition cannot be converted.
func TestCancelledRequisition_CannotConvert(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	a := f.submitApprove(t, r)
	c, err := f.move(t, a, []domain.RequestStatus{domain.RequestStatusApproved}, domain.RequestStatusCancelled, "requester")
	require.NoError(t, err)

	_, err = f.s.Apply(f.ctx, store.Transition{TenantID: f.tid, RequestID: c.RequestID,
		From: []domain.RequestStatus{domain.RequestStatusApproved}, To: domain.RequestStatusConverted, PurchaseOrderID: uuid.NewString()})
	assert.ErrorIs(t, err, domain.ErrInvalidTransition)

	// And the database refuses even a raw attempt.
	_, err = f.pool.Exec(context.Background(), `UPDATE purchase_requests SET status='CONVERTED', converted_purchase_order_id=$2 WHERE request_id=$1`, c.RequestID, uuid.NewString())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "can no longer change")
}

func TestConvert_RecordsPOLinkOnce(t *testing.T) {
	f := setup(t)
	a := f.submitApprove(t, f.create(t))
	po := uuid.NewString()
	v := a.Version
	c, err := f.s.Apply(f.ctx, store.Transition{TenantID: f.tid, RequestID: a.RequestID, ExpectedVersion: &v,
		From: []domain.RequestStatus{domain.RequestStatusApproved}, To: domain.RequestStatusConverted, PurchaseOrderID: po,
		Action: "CONVERTED", Meta: store.Meta{Actor: "buyer"}, Events: []store.EventSpec{{Type: "PurchaseRequisitionConverted"}}})
	require.NoError(t, err)
	require.NotNil(t, c.ConvertedPurchaseOrderID)
	assert.Equal(t, po, *c.ConvertedPurchaseOrderID)
	assert.Contains(t, f.outboxTypes(t, a.RequestID), "PurchaseRequisitionConverted")

	// A PO is claimed by one requisition only.
	b := f.submitApprove(t, f.create(t))
	_, err = f.s.Apply(f.ctx, store.Transition{TenantID: f.tid, RequestID: b.RequestID,
		From: []domain.RequestStatus{domain.RequestStatusApproved}, To: domain.RequestStatusConverted, PurchaseOrderID: po})
	assert.Error(t, err)

	// A converted requisition is frozen.
	_, err = f.move(t, c, []domain.RequestStatus{domain.RequestStatusApproved}, domain.RequestStatusCancelled, "x")
	assert.ErrorIs(t, err, domain.ErrInvalidTransition)
}

func TestExpireDue(t *testing.T) {
	f := setup(t)
	r := newReq(f.tid)
	past := time.Now().Add(-time.Hour)
	r.ExpiresAt = &past
	_, err := f.s.CreateRequest(f.ctx, r)
	require.NoError(t, err)
	// DRAFT never expires; once submitted it does.
	n, err := f.s.ExpireDue(context.Background(), time.Now())
	require.NoError(t, err)
	assert.Zero(t, n)
	_, err = f.move(t, r, []domain.RequestStatus{domain.RequestStatusDraft}, domain.RequestStatusPending, "requester")
	require.NoError(t, err)
	n, err = f.s.ExpireDue(context.Background(), time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got, _ := f.s.GetRequest(f.ctx, r.RequestID)
	assert.Equal(t, domain.RequestStatusExpired, got.Status)
	assert.Contains(t, f.outboxTypes(t, r.RequestID), "PurchaseRequisitionExpired")
}

func TestHistoryIsAppendOnly(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	_, err := f.pool.Exec(context.Background(), `UPDATE purchase_request_history SET actor='x' WHERE request_id=$1`, r.RequestID)
	require.Error(t, err)
	_, err = f.pool.Exec(context.Background(), `DELETE FROM purchase_request_history WHERE request_id=$1`, r.RequestID)
	require.Error(t, err)
}

type fakePub struct {
	fail bool
	got  []string
}

func (p *fakePub) PublishOutbox(_ context.Context, id, _ string, _ []byte) error {
	if p.fail {
		return errors.New("broker down")
	}
	p.got = append(p.got, id)
	return nil
}

func TestOutboxRelay_DeliversAndRetries(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	pub := &fakePub{fail: true}
	relay := outbox.NewRelay(f.pool, pub, time.Second, 50, zap.NewNop())

	assert.Zero(t, relay.RelayOnce(context.Background()), "broker down: nothing delivered")
	var attempts, unpublished int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT max(publish_attempts), count(*) FROM outbox_events WHERE aggregate_id=$1 AND published_at IS NULL`, r.RequestID).Scan(&attempts, &unpublished))
	assert.Equal(t, 1, attempts)
	assert.Equal(t, 2, unpublished, "events are kept for retry, never lost")

	pub.fail = false
	assert.Equal(t, 2, relay.RelayOnce(context.Background()))
	assert.Zero(t, relay.RelayOnce(context.Background()), "published rows are not re-sent")
}

func TestOutbox_DedupeKeyIsAtMostOnce(t *testing.T) {
	f := setup(t)
	r := f.create(t)
	tx, err := f.pool.Begin(context.Background())
	require.NoError(t, err)
	defer tx.Rollback(context.Background()) //nolint:errcheck
	ev := outbox.Event{AggregateType: "x", AggregateID: r.RequestID, EventType: "Once", TenantID: f.tid, LegalEntityID: r.LegalEntityID, DedupeKey: "k", Payload: map[string]string{}}
	ins, err := outbox.Insert(context.Background(), tx, ev)
	require.NoError(t, err)
	assert.True(t, ins)
	ins, err = outbox.Insert(context.Background(), tx, ev)
	require.NoError(t, err, "a duplicate is a no-op, not an aborted transaction")
	assert.False(t, ins)
}

func TestIdempotencyStore(t *testing.T) {
	f := setup(t)
	st := idempotency.NewPgStore(f.pool)
	ctx := context.Background()

	o, _, err := st.Begin(ctx, f.tid, "k1", "h1", "POST", "/p")
	require.NoError(t, err)
	assert.Equal(t, idempotency.Proceed, o)

	o, _, err = st.Begin(ctx, f.tid, "k1", "h1", "POST", "/p")
	require.NoError(t, err)
	assert.Equal(t, idempotency.InFlight, o)

	require.NoError(t, st.Complete(ctx, f.tid, "k1", 201, []byte(`{"ok":true}`)))
	o, rec, err := st.Begin(ctx, f.tid, "k1", "h1", "POST", "/p")
	require.NoError(t, err)
	assert.Equal(t, idempotency.Replay, o)
	assert.Equal(t, 201, rec.StatusCode)
	assert.JSONEq(t, `{"ok":true}`, string(rec.Body))

	o, _, err = st.Begin(ctx, f.tid, "k1", "OTHER", "POST", "/p")
	require.NoError(t, err)
	assert.Equal(t, idempotency.Mismatch, o)

	// Another tenant's identical key is independent.
	o, _, err = st.Begin(ctx, uuid.NewString(), "k1", "h1", "POST", "/p")
	require.NoError(t, err)
	assert.Equal(t, idempotency.Proceed, o)

	// Abandon releases an unfinished key for retry, but never a completed one.
	_, _, _ = st.Begin(ctx, f.tid, "k2", "h", "POST", "/p")
	require.NoError(t, st.Abandon(ctx, f.tid, "k2"))
	o, _, _ = st.Begin(ctx, f.tid, "k2", "h", "POST", "/p")
	assert.Equal(t, idempotency.Proceed, o)
	require.NoError(t, st.Abandon(ctx, f.tid, "k1"))
	o, _, _ = st.Begin(ctx, f.tid, "k1", "h1", "POST", "/p")
	assert.Equal(t, idempotency.Replay, o)
}
