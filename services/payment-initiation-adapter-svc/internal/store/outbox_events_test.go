//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/middleware"
)

// outboxEventCount counts eventing_outbox rows for one aggregate and a
// canonical event_type suffix, scoped to the given tenant.
func outboxEventCount(t *testing.T, ctx context.Context, tenantID, aggregateID, canonicalTypeSuffix string) int {
	t.Helper()
	var count int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type LIKE $3`,
		tenantID, aggregateID, "%"+canonicalTypeSuffix).Scan(&count))
	return count
}

// TestPgStore_EventingOutboxTableExists verifies the migration created the table.
func TestPgStore_EventingOutboxTableExists(t *testing.T) {
	ctx := context.Background()
	var exists bool
	err := testPool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables 
			WHERE table_name = 'eventing_outbox'
		)
	`).Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists, "eventing_outbox table should exist after migration 000005")
}

// TestPgStore_PrepareAttempt_EnqueuesPreparedEvent proves PrepareAttempt
// enqueues payment-initiation-attempt.prepared atomically with the insert.
func TestPgStore_PrepareAttempt_EnqueuesPreparedEvent(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	attempt, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: legalEntityID, PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 100.00, Currency: "USD", ExecutionDate: time.Now().Add(24 * time.Hour),
		PayerAccountVerified: true, IdempotencyKey: "corr-outbox-1",
	}, "maker-1", "corr-outbox-1")
	require.NoError(t, err)
	require.Equal(t, domain.StatusPrepared, attempt.Status)

	got := outboxEventCount(t, ctx, tenantID, attempt.AttemptID, "payment-initiation-attempt.prepared")
	assert.Equal(t, 1, got, "expected exactly 1 payment-initiation-attempt.prepared outbox row for this attempt")
}

// TestPgStore_PrepareAttempt_IdempotentReplay_NoReEnqueue proves the
// idempotent-replay branch of PrepareAttempt (ErrUnresolvedAttemptExists)
// does not re-enqueue — a retry with the same source_reference returns the
// existing attempt and must not publish a second event.
func TestPgStore_PrepareAttempt_IdempotentReplay_NoReEnqueue(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()
	sourceRef := "instruction-" + uuid.New().String()

	first, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: legalEntityID, SourceReference: sourceRef,
		PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 75.00, Currency: "USD", ExecutionDate: time.Now().Add(24 * time.Hour),
		PayerAccountVerified: true, IdempotencyKey: "corr-replay-1",
	}, "maker-1", "corr-replay-1")
	require.NoError(t, err)

	gotAfterFirst := outboxEventCount(t, ctx, tenantID, first.AttemptID, "payment-initiation-attempt.prepared")
	assert.Equal(t, 1, gotAfterFirst, "expected exactly 1 outbox row after first prepare")

	// Retry with a BRAND-NEW idempotency_key but SAME source_reference —
	// this is the exact Invariant #16 retry shape that returns the
	// existing attempt (ErrUnresolvedAttemptExists).
	second, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: legalEntityID, SourceReference: sourceRef,
		PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: 75.00, Currency: "USD", ExecutionDate: time.Now().Add(24 * time.Hour),
		PayerAccountVerified: true, IdempotencyKey: "corr-replay-2",
	}, "maker-1", "corr-replay-2")
	require.ErrorIs(t, err, domain.ErrUnresolvedAttemptExists)
	require.NotNil(t, second)
	require.Equal(t, first.AttemptID, second.AttemptID)

	gotAfterRetry := outboxEventCount(t, ctx, tenantID, first.AttemptID, "payment-initiation-attempt.prepared")
	assert.Equal(t, 1, gotAfterRetry, "a retry returning ErrUnresolvedAttemptExists must not re-enqueue PAYMENT_INITIATION_PREPARED")
}