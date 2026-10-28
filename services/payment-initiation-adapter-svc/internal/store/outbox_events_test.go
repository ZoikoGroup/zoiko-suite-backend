//go:build integration

package store_test

import (
	"context"
	"strings"
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
		Amount: "100.00", Currency: "USD", ExecutionDate: time.Now().Add(24 * time.Hour),
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
		Amount: "75.00", Currency: "USD", ExecutionDate: time.Now().Add(24 * time.Hour),
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
		Amount: "75.00", Currency: "USD", ExecutionDate: time.Now().Add(24 * time.Hour),
		PayerAccountVerified: true, IdempotencyKey: "corr-replay-2",
	}, "maker-1", "corr-replay-2")
	require.ErrorIs(t, err, domain.ErrUnresolvedAttemptExists)
	require.NotNil(t, second)
	require.Equal(t, first.AttemptID, second.AttemptID)

	gotAfterRetry := outboxEventCount(t, ctx, tenantID, first.AttemptID, "payment-initiation-attempt.prepared")
	assert.Equal(t, 1, gotAfterRetry, "a retry returning ErrUnresolvedAttemptExists must not re-enqueue PAYMENT_INITIATION_PREPARED")
}

// TestPgStore_LegacyStub_OutcomeEventsWithCorrelation proves the legacy stub
// path (no provider_key, no INT-07 engine) publishes the same outcome events
// the engine path does, atomically with the state change and carrying the
// caller's correlation id. Before this, only the engine's FinishSend wrote
// the outbox; a stub submission was invisible to every downstream consumer.
func TestPgStore_LegacyStub_OutcomeEventsWithCorrelation(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	prepare := func(key string) *domain.PaymentInitiationAttempt {
		a, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
			LegalEntityID: uuid.New().String(), PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
			Amount: "15.00", Currency: "USD", ExecutionDate: time.Now().Add(24 * time.Hour),
			PayerAccountVerified: true, IdempotencyKey: key,
		}, "maker-1", "corr-"+key)
		require.NoError(t, err)
		return a
	}
	assertEvent := func(a *domain.PaymentInitiationAttempt, suffix, correlationID string) {
		t.Helper()
		assert.Equal(t, 1, outboxEventCount(t, ctx, tenantID, a.AttemptID, suffix))
		var payload string
		require.NoError(t, testPool.QueryRow(ctx, `
			SELECT convert_from(payload, 'UTF8') FROM eventing_outbox
			WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type LIKE $3 LIMIT 1`,
			tenantID, a.AttemptID, "%"+suffix).Scan(&payload))
		assert.True(t, strings.Contains(payload, correlationID),
			"event %s must carry correlation id %s, payload: %s", suffix, correlationID, payload)
	}

	submitted := prepare("legacy-submitted")
	_, err := testStore.MarkSubmitted(ctx, submitted.AttemptID, "prv-1", "resp-1", "maker-1", "corr-legacy-submitted")
	require.NoError(t, err)
	assertEvent(submitted, "payment-initiation-attempt.submitted", "corr-legacy-submitted")

	pending := prepare("legacy-ambiguous")
	_, err = testStore.MarkPendingUnknown(ctx, pending.AttemptID, "maker-1", "corr-legacy-ambiguous")
	require.NoError(t, err)
	assertEvent(pending, "payment-initiation-attempt.ambiguous", "corr-legacy-ambiguous")

	rejected := prepare("legacy-rejected")
	_, err = testStore.MarkRejected(ctx, rejected.AttemptID, "profile cannot carry it", "maker-1", "corr-legacy-rejected")
	require.NoError(t, err)
	assertEvent(rejected, "payment-initiation-attempt.rejected", "corr-legacy-rejected")

	quarantined := prepare("legacy-quarantined")
	_, err = testStore.QuarantineAttempt(ctx, quarantined.AttemptID, domain.QuarantineRequest{Reason: "operator review"}, "maker-1", "corr-legacy-quarantined")
	require.NoError(t, err)
	assertEvent(quarantined, "payment-initiation-attempt.quarantined", "corr-legacy-quarantined")
}