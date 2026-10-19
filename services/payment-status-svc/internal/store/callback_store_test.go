//go:build integration

package store_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/middleware"
	"zoiko.io/payment-status-svc/internal/store"
)

var (
	appOnce  sync.Once
	appPool  *pgxpool.Pool
	appStore *store.PgStore
)

// app connects as an UNPRIVILEGED role so row-level security is really enforced.
func app(t *testing.T) *store.PgStore {
	t.Helper()
	appOnce.Do(func() {
		ctx := context.Background()
		_, err := testPool.Exec(ctx, `
			DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'zoiko_app') THEN
			  CREATE ROLE zoiko_app LOGIN PASSWORD 'app' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS; END IF; END $$;
			GRANT USAGE ON SCHEMA public TO zoiko_app;
			GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO zoiko_app;
			GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zoiko_app;`)
		require.NoError(t, err)
		cfg, err := pgxpool.ParseConfig(testDSN)
		require.NoError(t, err)
		cfg.ConnConfig.User, cfg.ConnConfig.Password = "zoiko_app", "app"
		appPool, err = pgxpool.NewWithConfig(ctx, cfg)
		require.NoError(t, err)
		appStore = store.NewPgStore(appPool, zap.NewNop(), store.WithEventRegion("uk"))
	})
	require.NotNil(t, appStore)
	return appStore
}

func tctx(tenant string) context.Context { return middleware.WithTenant(context.Background(), tenant) }

func newPayment(t *testing.T, st *store.PgStore, tenant, providerRef string) *domain.PaymentExecutionState {
	t.Helper()
	p, err := st.RecordPaymentStatus(tctx(tenant), tenant, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.NewString(), ProviderRequestID: providerRef, SourceReference: "src-" + uuid.NewString(),
	}, "maker", "corr")
	require.NoError(t, err)
	return p
}

func cb(provider, event, ref string, status domain.ExecutionStatus, at *time.Time) store.CallbackInput {
	types := map[domain.ExecutionStatus]string{domain.StatusAccepted: domain.EventPaymentAccepted, domain.StatusPending: domain.EventPaymentPending,
		domain.StatusSettled: domain.EventPaymentSettled, domain.StatusRejected: domain.EventPaymentRejected}
	return store.CallbackInput{Provider: provider, Scheme: "hmac-sha256-hex", EventRef: event, PaymentRef: ref, RawStatus: string(status), Mapped: status,
		EventType: types[status], EventTime: at, BodyHash: "ab" + "cdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"[:62], MappingVersion: provider + ":v1", CorrelationID: "c"}
}

func status(t *testing.T, st *store.PgStore, tenant, id string) domain.ExecutionStatus {
	t.Helper()
	p, err := st.FindPayment(tctx(tenant), id)
	require.NoError(t, err)
	return p.Status
}

func receiptCount(t *testing.T, tenant, provider, event, outcome string) int {
	t.Helper()
	var n int
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_callback_receipts WHERE tenant_id = $1 AND provider_key = $2 AND event_ref = $3 AND outcome = $4`,
		tenant, provider, event, outcome).Scan(&n))
	return n
}

func TestCallback_AppliesByProviderRefOrOurIdAndWritesTheWholeTrail(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ref := "PRV-" + uuid.NewString()
	p := newPayment(t, st, tenant, ref)

	res, err := st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E1", ref, domain.StatusAccepted, nil))
	require.NoError(t, err)
	assert.Equal(t, store.OutcomeApplied, res.Outcome)
	assert.Equal(t, domain.StatusAccepted, res.Payment.Status)
	assert.Equal(t, "PROVIDER_CALLBACK", res.Payment.FinalitySource)
	assert.Equal(t, "simbank:v1", res.Payment.MappingVersion)

	// by our own payment id as well
	res, err = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E2", p.PaymentID, domain.StatusPending, nil))
	require.NoError(t, err)
	assert.Equal(t, store.OutcomeApplied, res.Outcome)

	// the trail: history rows, outbox events, receipts, provider key
	var hist, events int
	require.NoError(t, testPool.QueryRow(context.Background(), `SELECT count(*) FROM status_events WHERE payment_id = $1 AND provider_event_ref IN ('E1','E2')`, p.PaymentID).Scan(&hist))
	require.NoError(t, testPool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type LIKE '%payment-execution-state.%'`, tenant, p.PaymentID).Scan(&events))
	assert.Equal(t, 2, hist)
	assert.GreaterOrEqual(t, events, 3, "recorded + accepted + pending")
	assert.Equal(t, 1, receiptCount(t, tenant, "simbank", "E1", store.OutcomeApplied))
	var pk string
	require.NoError(t, testPool.QueryRow(context.Background(), `SELECT provider_key FROM payment_execution_states WHERE payment_id = $1`, p.PaymentID).Scan(&pk))
	assert.Equal(t, "simbank", pk)
}

// acceptance: duplicate callback
func TestCallback_DuplicateIsRecordedAndChangesNothing(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ref := "PRV-" + uuid.NewString()
	p := newPayment(t, st, tenant, ref)
	_, err := st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E1", ref, domain.StatusSettled, nil))
	require.NoError(t, err)
	var before int
	require.NoError(t, testPool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox WHERE aggregate_id = $1`, p.PaymentID).Scan(&before))

	for i := 0; i < 3; i++ {
		res, err := st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E1", ref, domain.StatusSettled, nil))
		require.NoError(t, err)
		assert.Equal(t, store.OutcomeDuplicate, res.Outcome)
	}
	var after int
	require.NoError(t, testPool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox WHERE aggregate_id = $1`, p.PaymentID).Scan(&after))
	assert.Equal(t, before, after, "a duplicate callback must not create another accounting event")
	assert.Equal(t, 1, receiptCount(t, tenant, "simbank", "E1", store.OutcomeApplied))
	assert.Equal(t, 3, receiptCount(t, tenant, "simbank", "E1", store.OutcomeDuplicate))
	var hist int
	require.NoError(t, testPool.QueryRow(context.Background(), `SELECT count(*) FROM status_events WHERE payment_id = $1 AND provider_event_ref = 'E1'`, p.PaymentID).Scan(&hist))
	assert.Equal(t, 1, hist)
}

func TestCallback_ConcurrentCopiesOfOneEventApplyExactlyOnce(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ref := "PRV-" + uuid.NewString()
	p := newPayment(t, st, tenant, ref)
	var applied, dup atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E-race", ref, domain.StatusSettled, nil))
			if err != nil {
				t.Errorf("%v", err)
				return
			}
			switch res.Outcome {
			case store.OutcomeApplied:
				applied.Add(1)
			case store.OutcomeDuplicate:
				dup.Add(1)
			default:
				t.Errorf("unexpected outcome %s", res.Outcome)
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, applied.Load())
	assert.EqualValues(t, 9, dup.Load())
	assert.Equal(t, domain.StatusSettled, status(t, st, tenant, p.PaymentID))
}

// acceptance: status regression
func TestCallback_NeverMovesAPaymentBackwards(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()

	// a final payment cannot be changed by a later callback
	ref := "PRV-" + uuid.NewString()
	p := newPayment(t, st, tenant, ref)
	_, _ = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E1", ref, domain.StatusSettled, nil))
	for i, s := range []domain.ExecutionStatus{domain.StatusPending, domain.StatusRejected, domain.StatusAccepted} {
		res, err := st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E-late-"+string(rune('a'+i)), ref, s, nil))
		require.NoError(t, err)
		assert.Equal(t, store.OutcomeRegressionBlocked, res.Outcome, s)
	}
	assert.Equal(t, domain.StatusSettled, status(t, st, tenant, p.PaymentID))

	// an earlier stage arriving late does not undo a later one
	ref2 := "PRV-" + uuid.NewString()
	p2 := newPayment(t, st, tenant, ref2)
	_, _ = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "F1", ref2, domain.StatusPending, nil))
	res, err := st.ApplyProviderCallback(tctx(tenant), cb("simbank", "F2", ref2, domain.StatusAccepted, nil))
	require.NoError(t, err)
	assert.Equal(t, store.OutcomeStaleIgnored, res.Outcome)
	assert.Equal(t, domain.StatusPending, status(t, st, tenant, p2.PaymentID))

	// the same state again (a different event) changes nothing
	res, _ = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "F3", ref2, domain.StatusPending, nil))
	assert.Equal(t, store.OutcomeStaleIgnored, res.Outcome)

	// an older event time loses to a newer one already applied, even if it reports a later stage
	ref3 := "PRV-" + uuid.NewString()
	p3 := newPayment(t, st, tenant, ref3)
	newer, older := time.Now().UTC().Truncate(time.Second), time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	_, _ = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "G1", ref3, domain.StatusAccepted, &newer))
	res, err = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "G2", ref3, domain.StatusPending, &older))
	require.NoError(t, err)
	assert.Equal(t, store.OutcomeStaleIgnored, res.Outcome, "an older callback never overrides a newer one")
	assert.Equal(t, domain.StatusAccepted, status(t, st, tenant, p3.PaymentID))
	res, _ = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "G3", ref3, domain.StatusPending, &newer))
	assert.Equal(t, store.OutcomeApplied, res.Outcome)

	// forward progress through every stage works
	ref4 := "PRV-" + uuid.NewString()
	newPayment(t, st, tenant, ref4)
	for i, s := range []domain.ExecutionStatus{domain.StatusAccepted, domain.StatusPending, domain.StatusSettled} {
		res, err := st.ApplyProviderCallback(tctx(tenant), cb("simbank", "H"+string(rune('1'+i)), ref4, s, nil))
		require.NoError(t, err)
		assert.Equal(t, store.OutcomeApplied, res.Outcome, s)
	}
}

func TestCallback_UnknownPaymentIsNotDecidedSoItCanApplyLater(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ref := "PRV-" + uuid.NewString()

	// the provider calls before we know the payment
	res, err := st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E1", ref, domain.StatusAccepted, nil))
	require.NoError(t, err)
	assert.Equal(t, store.OutcomeUnknownPayment, res.Outcome)
	assert.Nil(t, res.Payment)
	assert.Equal(t, 1, receiptCount(t, tenant, "simbank", "E1", store.OutcomeUnknownPayment))

	// the payment becomes known; the provider retries the SAME event and it now applies
	p := newPayment(t, st, tenant, ref)
	res, err = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E1", ref, domain.StatusAccepted, nil))
	require.NoError(t, err)
	assert.Equal(t, store.OutcomeApplied, res.Outcome)
	assert.Equal(t, domain.StatusAccepted, status(t, st, tenant, p.PaymentID))
}

func TestCallback_AmbiguousReferenceChangesNothing(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ref := "PRV-" + uuid.NewString()
	a, b := newPayment(t, st, tenant, ref), newPayment(t, st, tenant, ref)
	res, err := st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E1", ref, domain.StatusSettled, nil))
	require.NoError(t, err)
	assert.Equal(t, store.OutcomeAmbiguousPayment, res.Outcome)
	assert.Equal(t, domain.StatusPrepared, status(t, st, tenant, a.PaymentID))
	assert.Equal(t, domain.StatusPrepared, status(t, st, tenant, b.PaymentID))
}

func TestCallback_CannotCrossTenants(t *testing.T) {
	st := app(t)
	owner, other := uuid.NewString(), uuid.NewString()
	ref := "PRV-" + uuid.NewString()
	p := newPayment(t, st, owner, ref)

	// a callback arriving under another tenant's scope finds nothing of the owner's
	res, err := st.ApplyProviderCallback(tctx(other), cb("simbank", "E1", ref, domain.StatusSettled, nil))
	require.NoError(t, err)
	assert.Equal(t, store.OutcomeUnknownPayment, res.Outcome)
	assert.Equal(t, domain.StatusPrepared, status(t, st, owner, p.PaymentID), "the owner's payment is untouched")
	// and not even by our payment id
	res, _ = st.ApplyProviderCallback(tctx(other), cb("simbank", "E2", p.PaymentID, domain.StatusSettled, nil))
	assert.Equal(t, store.OutcomeUnknownPayment, res.Outcome)

	// event identity is per tenant: the same event ref in two tenants are two events
	otherRef := "PRV-" + uuid.NewString()
	op := newPayment(t, st, other, otherRef)
	r1, _ := st.ApplyProviderCallback(tctx(owner), cb("simbank", "SAME", ref, domain.StatusAccepted, nil))
	r2, _ := st.ApplyProviderCallback(tctx(other), cb("simbank", "SAME", otherRef, domain.StatusAccepted, nil))
	assert.Equal(t, store.OutcomeApplied, r1.Outcome)
	assert.Equal(t, store.OutcomeApplied, r2.Outcome)
	assert.Equal(t, domain.StatusAccepted, status(t, st, other, op.PaymentID))

	// receipts are tenant-scoped under RLS
	entity := p.LegalEntityID
	mine, err := st.ListReceipts(tctx(owner), entity, false, "", 50)
	require.NoError(t, err)
	assert.NotEmpty(t, mine)
	theirs, err := st.ListReceipts(tctx(other), entity, false, "", 50)
	require.NoError(t, err)
	assert.Empty(t, theirs, "another tenant cannot list this entity's receipts")
	none, _ := st.ListReceipts(context.Background(), entity, true, "", 50)
	assert.Empty(t, none, "no tenant scope sees nothing")
	// and cannot write a receipt for someone else, even by raw SQL
	_, err = appPool.Exec(tctx(other), `INSERT INTO provider_callback_receipts (tenant_id, provider_key, event_ref, payment_ref, scheme, reported_status, body_hash, outcome)
		VALUES ($1, 'simbank', 'forged', 'x', 'hmac-sha256-hex', 'SETTLED', repeat('a', 64), 'APPLIED')`, owner)
	assert.Error(t, err)
}

func TestCallback_ReceiptsAreAppendOnlyAndListable(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ref := "PRV-" + uuid.NewString()
	p := newPayment(t, st, tenant, ref)
	_, _ = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E1", ref, domain.StatusAccepted, nil))
	_, _ = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E1", ref, domain.StatusAccepted, nil))
	_, _ = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E-unmatched", "nope", domain.StatusAccepted, nil))

	list, err := st.ListReceipts(tctx(tenant), p.LegalEntityID, false, p.PaymentID, 50)
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, r := range list {
		assert.Equal(t, "simbank", r.Provider)
		assert.Len(t, r.BodyHash, 64)
		assert.NotNil(t, r.PaymentID)
	}
	outcomes := []string{list[0].Outcome, list[1].Outcome}
	assert.ElementsMatch(t, []string{store.OutcomeApplied, store.OutcomeDuplicate}, outcomes)

	// unmatched receipts have no entity: they are listed only when asked for
	um, err := st.ListReceipts(tctx(tenant), p.LegalEntityID, true, "", 50)
	require.NoError(t, err)
	found := false
	for _, r := range um {
		if r.EventRef == "E-unmatched" {
			found = true
			assert.Equal(t, store.OutcomeUnknownPayment, r.Outcome)
		}
	}
	assert.True(t, found)

	var id string
	require.NoError(t, testPool.QueryRow(context.Background(), `SELECT receipt_id::text FROM provider_callback_receipts WHERE tenant_id = $1 LIMIT 1`, tenant).Scan(&id))
	_, err = testPool.Exec(context.Background(), `UPDATE provider_callback_receipts SET outcome = 'APPLIED' WHERE receipt_id = $1`, id)
	assert.Error(t, err)
	_, err = testPool.Exec(context.Background(), `DELETE FROM provider_callback_receipts WHERE receipt_id = $1`, id)
	assert.Error(t, err)
}

func TestCallback_TheStateGuardIsTheBackstop(t *testing.T) {
	// even if the store's own checks were bypassed, the trigger still refuses to move a settled payment
	st := app(t)
	tenant := uuid.NewString()
	ref := "PRV-" + uuid.NewString()
	p := newPayment(t, st, tenant, ref)
	_, _ = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E1", ref, domain.StatusSettled, nil))
	_, err := testPool.Exec(context.Background(), `UPDATE payment_execution_states SET status = 'PENDING' WHERE payment_id = $1`, p.PaymentID)
	assert.Error(t, err)
}

// INT-NP-043 / INT-CTRL-034: a callback that cannot be correlated to a durable payment changes nothing, is quarantined
// for a named owner, and raises one event so an alert can route to that owner. A provider's retries do not re-raise it.
func TestCallback_UncorrelatedIsQuarantinedForANamedOwnerOnce(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ref := "PRV-" + uuid.NewString()
	quarantineEvents := func() (n int, owner string) {
		require.NoError(t, testPool.QueryRow(context.Background(),
			`SELECT count(*), COALESCE(max(convert_from(payload,'UTF8')::jsonb #>> '{data,owner}'), '') FROM eventing_outbox
			 WHERE tenant_id = $1 AND event_type = 'com.zoikosuite.banking.provider-callback.quarantined'`, tenant).Scan(&n, &owner))
		return
	}

	res, err := st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E-ORPHAN", ref, domain.StatusSettled, nil))
	require.NoError(t, err)
	require.Equal(t, store.OutcomeUnknownPayment, res.Outcome)
	n, owner := quarantineEvents()
	assert.Equal(t, 1, n)
	assert.Equal(t, store.QuarantineOwner, owner)

	// the provider retries the same orphan twice: still one quarantine event, receipts keep the evidence
	for i := 0; i < 2; i++ {
		_, err = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E-ORPHAN", ref, domain.StatusSettled, nil))
		require.NoError(t, err)
	}
	n, _ = quarantineEvents()
	assert.Equal(t, 1, n)
	assert.Equal(t, 3, receiptCount(t, tenant, "simbank", "E-ORPHAN", store.OutcomeUnknownPayment))

	// an ambiguous reference is quarantined the same way, and nothing moved
	amb := "PRV-" + uuid.NewString()
	a, b := newPayment(t, st, tenant, amb), newPayment(t, st, tenant, amb)
	res, err = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E-AMB", amb, domain.StatusSettled, nil))
	require.NoError(t, err)
	require.Equal(t, store.OutcomeAmbiguousPayment, res.Outcome)
	n, _ = quarantineEvents()
	assert.Equal(t, 2, n)
	assert.Equal(t, domain.StatusPrepared, status(t, st, tenant, a.PaymentID))
	assert.Equal(t, domain.StatusPrepared, status(t, st, tenant, b.PaymentID))

	// a callback that DOES correlate raises no quarantine
	ok := "PRV-" + uuid.NewString()
	newPayment(t, st, tenant, ok)
	_, err = st.ApplyProviderCallback(tctx(tenant), cb("simbank", "E-OK", ok, domain.StatusAccepted, nil))
	require.NoError(t, err)
	n, _ = quarantineEvents()
	assert.Equal(t, 2, n)
}
