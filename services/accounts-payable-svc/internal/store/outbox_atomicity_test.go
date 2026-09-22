package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-payable-svc/internal/outbox"
)

func TestPgStore_Outbox_ForcedFailure_RollbackAtomicity_RealDB(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	invoiceID := uuid.New().String()
	correlationID := uuid.New().String()
	outboxEventID := uuid.New().String()

	// 1. Begin a real transaction
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck

	// Set tenant context for RLS in this tx
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)

	invoiceDate := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)

	// 2. Write the domain business row into vendor_invoices
	const insertInvoiceSQL = `
		INSERT INTO vendor_invoices (
			invoice_id, tenant_id, legal_entity_id, vendor_id, invoice_number,
			amount, currency_code, due_date, status, created_by_principal_id,
			correlation_id, invoice_date, supply_date, net_amount, tax_amount,
			created_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15,
			now()
		)
	`
	_, err = tx.Exec(ctx, insertInvoiceSQL,
		invoiceID, tenantID, legalEntityID, "vendor-1", "INV-FAIL-001",
		5000.0, "USD", time.Now().UTC().Add(30*24*time.Hour), "DRAFT", "preparer-1",
		correlationID, invoiceDate, invoiceDate, 5000.0, 0.0,
	)
	require.NoError(t, err)

	// 3. Write the outbox row
	err = outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: outboxEventID,
		AggregateType: "VENDOR_INVOICE",
		AggregateID:   invoiceID,
		EventType:     "vendor_invoice.captured",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		CorrelationID: correlationID,
		Payload:       map[string]any{"invoice_id": invoiceID},
	})
	require.NoError(t, err)

	// 4. Deliberately fail the transaction before commit
	// (Trigger a primary key constraint collision on outbox_events, forcing a failure)
	_, err = tx.Exec(ctx, "INSERT INTO outbox_events (outbox_event_id) VALUES ($1)", outboxEventID)
	require.Error(t, err, "expected duplicate primary key constraint error")

	// Explicitly roll back the transaction
	err = tx.Rollback(ctx)
	require.NoError(t, err)

	// 5. Query fresh connection (pool) and assert NEITHER row exists
	var invoiceCount, outboxCount int
	scoped(t, pool, tenantID, func(verifyTx pgx.Tx) {
		err := verifyTx.QueryRow(ctx, "SELECT count(*) FROM vendor_invoices WHERE invoice_id = $1", invoiceID).Scan(&invoiceCount)
		require.NoError(t, err)

		err = verifyTx.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", invoiceID).Scan(&outboxCount)
		require.NoError(t, err)
	})

	assert.Equal(t, 0, invoiceCount, "domain vendor_invoice row must not exist after rollback")
	assert.Equal(t, 0, outboxCount, "outbox row must not exist after rollback")
}
