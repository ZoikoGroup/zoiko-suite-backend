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

// A6: the amount goes into NUMERIC(18,2) as decimal text and comes back out
// as decimal text, on the attempt, on a later read and on the prepared
// event. 9999999999999999.99 is the column's largest value and has no
// float64: through a float it would have been stored as 10000000000000000.
func TestPgStore_AmountRoundTripsExactly(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	const largest = "9999999999999999.99"

	a, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: largest, Currency: "USD", ExecutionDate: time.Now().Add(24 * time.Hour),
		PayerAccountVerified: true, IdempotencyKey: "amount-exact-" + tenantID,
	}, "maker-1", "corr-amount-exact")
	require.NoError(t, err)
	assert.Equal(t, domain.Decimal(largest), a.Amount)

	var stored string
	require.NoError(t, testPool.QueryRow(ctx, `SELECT amount::text FROM payment_initiation_attempts WHERE attempt_id = $1`, a.AttemptID).Scan(&stored))
	assert.Equal(t, largest, stored)

	read, err := testStore.FindAttempt(ctx, a.AttemptID)
	require.NoError(t, err)
	assert.Equal(t, domain.Decimal(largest), read.Amount)

	// A whole amount reads back in the column's form.
	b, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: uuid.New().String(), PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: "500", Currency: "USD", ExecutionDate: time.Now().Add(24 * time.Hour),
		PayerAccountVerified: true, IdempotencyKey: "amount-whole-" + tenantID,
	}, "maker-1", "corr-amount-whole")
	require.NoError(t, err)
	assert.Equal(t, domain.Decimal("500.00"), b.Amount)

	var payload string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT convert_from(payload, 'UTF8') FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type LIKE '%payment-initiation-attempt.prepared'`,
		tenantID, a.AttemptID).Scan(&payload))
	assert.Contains(t, payload, `"Amount":`+largest)
}

// The store holds the same line as the handler: an amount the column would
// round is refused, and no row or event is written.
func TestPgStore_AmountTheColumnWouldRoundIsRefused(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	for amount, want := range map[domain.Decimal]error{
		"10.005":            domain.ErrAmountPrecision,
		"10000000000000000": domain.ErrAmountPrecision,
		"0":                 domain.ErrInvalidAmount,
		"":                  domain.ErrInvalidAmount,
	} {
		_, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
			LegalEntityID: uuid.New().String(), PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
			Amount: amount, Currency: "USD", ExecutionDate: time.Now().Add(24 * time.Hour),
			PayerAccountVerified: true, IdempotencyKey: "amount-refused-" + uuid.NewString(),
		}, "maker-1", "corr-amount-refused")
		assert.ErrorIs(t, err, want, "amount %q", amount)
	}
	var rows, events int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT COUNT(*) FROM payment_initiation_attempts WHERE tenant_id = $1`, tenantID).Scan(&rows))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT COUNT(*) FROM eventing_outbox WHERE tenant_id = $1`, tenantID).Scan(&events))
	assert.Zero(t, rows)
	assert.Zero(t, events)
}
