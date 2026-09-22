//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
	"zoiko.io/accounts-receivable-svc/internal/outbox"
	"zoiko.io/accounts-receivable-svc/internal/store"
)

func TestPgStore_Outbox_CreateInvoice_Atomicity_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	invoiceID := uuid.New().String()
	correlationID := uuid.New().String()
	inv := &domain.CustomerInvoice{
		InvoiceID:            invoiceID,
		TenantID:             tenantID,
		LegalEntityID:        legalEntityID,
		CustomerID:           "cust-123",
		InvoiceNumber:        "INV-2026-0001",
		Amount:               50000,
		CurrencyCode:         "USD",
		DueDate:              time.Now().UTC().Add(30 * 24 * time.Hour),
		Status:               domain.InvoiceStatusIssued,
		CreatedByPrincipalID: "preparer-1",
		CorrelationID:        correlationID,
	}

	created, err := s.CreateInvoice(ctx, inv)
	require.NoError(t, err)
	assert.True(t, created)

	// Verify outbox row committed atomically with invoice row
	var eventType, aggType, aggID, storedTenantID, storedEntityID, actorID, corrID string
	var outboxEventID uuid.UUID
	var payloadBytes []byte
	err = pool.QueryRow(ctx, `
		SELECT outbox_event_id, event_type, aggregate_type, aggregate_id, tenant_id::text, legal_entity_id::text,
		       actor_id, correlation_id, payload
		FROM outbox_events
		WHERE aggregate_id = $1
	`, invoiceID).Scan(&outboxEventID, &eventType, &aggType, &aggID, &storedTenantID, &storedEntityID, &actorID, &corrID, &payloadBytes)
	require.NoError(t, err, "expected outbox row for invoice.issued to be present")

	assert.Equal(t, "invoice.issued", eventType)
	assert.Equal(t, "CUSTOMER_INVOICE", aggType)
	assert.Equal(t, invoiceID, aggID)
	assert.Equal(t, tenantID, storedTenantID)
	assert.Equal(t, legalEntityID, storedEntityID)
	assert.Equal(t, "preparer-1", actorID)
	assert.Equal(t, correlationID, corrID)

	// Verify Variant B envelope
	var env outbox.VariantBEnvelope
	require.NoError(t, json.Unmarshal(payloadBytes, &env))
	assert.Equal(t, "evt-"+outboxEventID.String(), env.EventID)
	assert.Equal(t, "invoice.issued", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "accounts-receivable-svc", env.SourceService)
	assert.Equal(t, tenantID, env.TenantID)
	assert.Equal(t, legalEntityID, env.LegalEntityID)
	assert.Equal(t, "preparer-1", env.ActorID)
	assert.Equal(t, correlationID, env.CorrelationID)

	// Idempotent Replay: zero duplicate outbox events
	createdRetry, err := s.CreateInvoice(ctx, inv)
	require.NoError(t, err)
	assert.False(t, createdRetry, "expected replay to return created=false")

	var outboxCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", invoiceID).Scan(&outboxCount)
	require.NoError(t, err)
	assert.Equal(t, 1, outboxCount, "expected exactly 1 outbox event across idempotent retries")
}

func TestPgStore_Outbox_TransitionInvoice_Transitions_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	// 1. ISSUED -> SENT
	inv1 := &domain.CustomerInvoice{
		InvoiceID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        legalEntityID,
		CustomerID:           "cust-1",
		InvoiceNumber:        "INV-TEST-01",
		Amount:               10000,
		CurrencyCode:         "USD",
		DueDate:              time.Now().UTC().Add(30 * 24 * time.Hour),
		Status:               domain.InvoiceStatusIssued,
		CreatedByPrincipalID: "user-1",
		CorrelationID:        uuid.New().String(),
	}
	_, err := s.CreateInvoice(ctx, inv1)
	require.NoError(t, err)

	sentInv, err := s.TransitionInvoice(ctx, tenantID, inv1.InvoiceID, domain.InvoiceStatusIssued, domain.InvoiceStatusSent, "sender-1")
	require.NoError(t, err)
	assert.Equal(t, domain.InvoiceStatusSent, sentInv.Status)

	var sentCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'invoice.sent'
	`, inv1.InvoiceID).Scan(&sentCount)
	require.NoError(t, err)
	assert.Equal(t, 1, sentCount, "expected 1 invoice.sent outbox event")

	// 2. SENT -> OVERDUE
	overdueInv, err := s.TransitionInvoice(ctx, tenantID, inv1.InvoiceID, domain.InvoiceStatusSent, domain.InvoiceStatusOverdue, "marker-1")
	require.NoError(t, err)
	assert.Equal(t, domain.InvoiceStatusOverdue, overdueInv.Status)

	var overdueCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'receivable.overdue'
	`, inv1.InvoiceID).Scan(&overdueCount)
	require.NoError(t, err)
	assert.Equal(t, 1, overdueCount, "expected 1 receivable.overdue outbox event")

	// 3. OVERDUE -> PAID
	paidInv, err := s.TransitionInvoice(ctx, tenantID, inv1.InvoiceID, domain.InvoiceStatusOverdue, domain.InvoiceStatusPaid, "cashier-1")
	require.NoError(t, err)
	assert.Equal(t, domain.InvoiceStatusPaid, paidInv.Status)

	var paidCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'payment.received'
	`, inv1.InvoiceID).Scan(&paidCount)
	require.NoError(t, err)
	assert.Equal(t, 1, paidCount, "expected 1 payment.received outbox event")

	// 4. SENT -> PAID directly on a second invoice
	inv2 := &domain.CustomerInvoice{
		InvoiceID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        legalEntityID,
		CustomerID:           "cust-2",
		InvoiceNumber:        "INV-TEST-02",
		Amount:               25000,
		CurrencyCode:         "USD",
		DueDate:              time.Now().UTC().Add(30 * 24 * time.Hour),
		Status:               domain.InvoiceStatusIssued,
		CreatedByPrincipalID: "user-2",
		CorrelationID:        uuid.New().String(),
	}
	_, err = s.CreateInvoice(ctx, inv2)
	require.NoError(t, err)

	_, err = s.TransitionInvoice(ctx, tenantID, inv2.InvoiceID, domain.InvoiceStatusIssued, domain.InvoiceStatusSent, "sender-2")
	require.NoError(t, err)

	paidInv2, err := s.TransitionInvoice(ctx, tenantID, inv2.InvoiceID, domain.InvoiceStatusSent, domain.InvoiceStatusPaid, "cashier-2")
	require.NoError(t, err)
	assert.Equal(t, domain.InvoiceStatusPaid, paidInv2.Status)

	var paidCount2 int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'payment.received'
	`, inv2.InvoiceID).Scan(&paidCount2)
	require.NoError(t, err)
	assert.Equal(t, 1, paidCount2, "expected 1 payment.received outbox event for SENT -> PAID")
}

func TestPgStore_Outbox_FailedTransition_NoOutboxRow_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	inv := &domain.CustomerInvoice{
		InvoiceID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        legalEntityID,
		CustomerID:           "cust-failed",
		InvoiceNumber:        "INV-TEST-FAIL",
		Amount:               15000,
		CurrencyCode:         "USD",
		DueDate:              time.Now().UTC().Add(30 * 24 * time.Hour),
		Status:               domain.InvoiceStatusIssued,
		CreatedByPrincipalID: "user-1",
		CorrelationID:        uuid.New().String(),
	}
	_, err := s.CreateInvoice(ctx, inv)
	require.NoError(t, err)

	// Attempt invalid transition: ISSUED -> PAID directly (must fail)
	_, err = s.TransitionInvoice(ctx, tenantID, inv.InvoiceID, domain.InvoiceStatusSent, domain.InvoiceStatusPaid, "cashier-1")
	require.ErrorIs(t, err, domain.ErrInvalidTransition)

	// Verify no payment.received outbox row was inserted
	var outboxCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'payment.received'
	`, inv.InvoiceID).Scan(&outboxCount)
	require.NoError(t, err)
	assert.Equal(t, 0, outboxCount, "expected 0 outbox events on failed transition")
}

func TestPgStore_Outbox_ForcedFailure_RollbackAtomicity_RealDB(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	invoiceID := uuid.New().String()
	correlationID := uuid.New().String()
	outboxEventID := uuid.New().String()

	// 1. Begin raw transaction
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck

	// Set tenant context for RLS in this tx
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)

	// 2. Write the domain business row into customer_invoices
	const insertInvoiceSQL = `
		INSERT INTO customer_invoices (
			invoice_id, tenant_id, legal_entity_id, customer_id, invoice_number,
			amount, currency_code, due_date, status, created_by_principal_id,
			correlation_id, created_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, now()
		)
	`
	_, err = tx.Exec(ctx, insertInvoiceSQL,
		invoiceID, tenantID, legalEntityID, "cust-fail-001", "INV-FAIL-AR",
		10000.0, "USD", time.Now().UTC().Add(30*24*time.Hour), "ISSUED", "preparer-1",
		correlationID,
	)
	require.NoError(t, err)

	// 3. Write the outbox row
	err = outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: outboxEventID,
		AggregateType: "CUSTOMER_INVOICE",
		AggregateID:   invoiceID,
		EventType:     "invoice.issued",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		CorrelationID: correlationID,
		Payload:       map[string]any{"invoice_id": invoiceID},
	})
	require.NoError(t, err)

	// 4. Deliberately fail the transaction before commit
	// (Trigger duplicate primary key on outbox_events)
	_, err = tx.Exec(ctx, "INSERT INTO outbox_events (outbox_event_id) VALUES ($1)", outboxEventID)
	require.Error(t, err, "expected duplicate primary key constraint collision")

	err = tx.Rollback(ctx)
	require.NoError(t, err)

	// 5. Query fresh connection (pool) and assert NEITHER row exists
	var invoiceCount, outboxCount int
	scoped(t, pool, tenantID, func(verifyTx pgx.Tx) {
		err := verifyTx.QueryRow(ctx, "SELECT count(*) FROM customer_invoices WHERE invoice_id = $1", invoiceID).Scan(&invoiceCount)
		require.NoError(t, err)

		err = verifyTx.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", invoiceID).Scan(&outboxCount)
		require.NoError(t, err)
	})

	assert.Equal(t, 0, invoiceCount, "domain customer_invoice row must not exist after rollback")
	assert.Equal(t, 0, outboxCount, "outbox row must not exist after rollback")
}

