//go:build integration

package store_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payment-status-svc/internal/domain"
)

func recordCount(t *testing.T, tenant, sourceReference string) int {
	t.Helper()
	var count int
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM payment_execution_states WHERE tenant_id = $1 AND source_reference = $2`,
		tenant, sourceReference).Scan(&count))
	return count
}

// TestPgStore_RecordPaymentStatus_ConcurrentSameSourceReference_OneRow proves
// the BNK-07 A5 invariant under a real race: every concurrent caller for one
// BNK-06 source_reference must resolve to the same execution state, not create
// one row each. Without migration 000007's partial unique index and the
// unique-violation recovery in RecordPaymentStatus, N lost-response retries
// would become N payment rows, each able to receive its own callbacks and emit
// its own PaymentSettled.
func TestPgStore_RecordPaymentStatus_ConcurrentSameSourceReference_OneRow(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	sourceReference := "src-race-" + uuid.NewString()
	entity := uuid.NewString()
	ctx := tctx(tenant)

	type result struct {
		payment *domain.PaymentExecutionState
		err     error
	}
	const callers = 8
	start := make(chan struct{})
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p, err := st.RecordPaymentStatus(ctx, tenant, domain.RecordPaymentStatusRequest{
				LegalEntityID:   entity,
				SourceReference: sourceReference,
			}, "maker", "corr-race")
			results <- result{payment: p, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winner *domain.PaymentExecutionState
	for res := range results {
		require.NoError(t, res.err)
		require.NotNil(t, res.payment)
		if winner == nil {
			winner = res.payment
			continue
		}
		assert.Equal(t, winner.PaymentID, res.payment.PaymentID,
			"every concurrent caller must resolve to the same execution state")
		assert.Equal(t, winner.Status, res.payment.Status)
		assert.Equal(t, winner.SourceReference, res.payment.SourceReference)
	}
	require.NotNil(t, winner)

	assert.Equal(t, 1, recordCount(t, tenant, sourceReference),
		"exactly one execution state may exist for the unresolved source_reference")

	// A duplicate must not become a second side effect either: only the
	// winner's transaction records PAYMENT_PREPARED and enqueues the event.
	var statusEvents int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM status_events WHERE payment_id = $1 AND event_type = 'PAYMENT_PREPARED'`,
		winner.PaymentID).Scan(&statusEvents))
	assert.Equal(t, 1, statusEvents)

	var outboxEvents int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2`,
		tenant, winner.PaymentID).Scan(&outboxEvents))
	assert.Equal(t, 1, outboxEvents)
}

// TestPgStore_RecordPaymentStatus_SequentialRetryReturnsTheOriginalRow covers
// the pre-insert lookup path (the common lost-201 retry, with no race).
func TestPgStore_RecordPaymentStatus_SequentialRetryReturnsTheOriginalRow(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	sourceReference := "src-retry-" + uuid.NewString()
	ctx := tctx(tenant)
	req := domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.NewString(), ProviderRequestID: "pr-" + uuid.NewString(), SourceReference: sourceReference,
	}

	first, err := st.RecordPaymentStatus(ctx, tenant, req, "maker", "corr-1")
	require.NoError(t, err)
	second, err := st.RecordPaymentStatus(ctx, tenant, req, "maker", "corr-1")
	require.NoError(t, err)

	assert.Equal(t, first.PaymentID, second.PaymentID)
	assert.Equal(t, first.Status, second.Status)
	assert.Equal(t, 1, recordCount(t, tenant, sourceReference))
}

// TestPgStore_RecordPaymentStatus_EmptySourceReferenceIsNotDeduped proves the
// index's empty-source-reference exclusion: callers that omit the optional
// field must not collide with every other such caller.
func TestPgStore_RecordPaymentStatus_EmptySourceReferenceIsNotDeduped(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	ctx := tctx(tenant)

	first, err := st.RecordPaymentStatus(ctx, tenant, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.NewString(),
	}, "maker", "corr-1")
	require.NoError(t, err)
	second, err := st.RecordPaymentStatus(ctx, tenant, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.NewString(),
	}, "maker", "corr-2")
	require.NoError(t, err)

	assert.NotEqual(t, first.PaymentID, second.PaymentID)
}

// TestPgStore_RecordPaymentStatus_ResolvedSourceReferenceMayBeReused pins the
// deliberate scope of the index: it covers only the not-yet-resolved states.
// Once the previous execution state is final, a genuine new instruction may
// carry the same source_reference.
func TestPgStore_RecordPaymentStatus_ResolvedSourceReferenceMayBeReused(t *testing.T) {
	st := app(t)
	tenant := uuid.NewString()
	sourceReference := "src-reuse-" + uuid.NewString()
	ctx := tctx(tenant)

	first, err := st.RecordPaymentStatus(ctx, tenant, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.NewString(), SourceReference: sourceReference,
	}, "maker", "corr-1")
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `UPDATE payment_execution_states SET status = 'REJECTED' WHERE payment_id = $1`, first.PaymentID)
	require.NoError(t, err)

	second, err := st.RecordPaymentStatus(ctx, tenant, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.NewString(), SourceReference: sourceReference,
	}, "maker", "corr-2")
	require.NoError(t, err)

	assert.NotEqual(t, first.PaymentID, second.PaymentID)
	assert.Equal(t, 2, recordCount(t, tenant, sourceReference))
}
