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

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/outbox"
	"zoiko.io/general-ledger-svc/internal/store"
)

func TestPgStore_Outbox_CreateJournal_Atomicity_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	journalID := uuid.New().String()
	correlationID := uuid.New().String()
	transactionDate := domain.NewDate(2026, time.August, 1)
	h := &domain.JournalHeader{
		JournalID:            journalID,
		TenantID:             tenantID,
		LegalEntityID:        legalEntityID,
		FiscalPeriod:         "2026-08",
		Status:               domain.JournalStatusPending,
		TransactionDate:      transactionDate,
		PostingDate:          transactionDate,
		CreatedByPrincipalID: "preparer-1",
		CorrelationID:        correlationID,
		ApprovalStatus:       domain.ApprovalStatusDraft,
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 500, CreditAmount: 0},
		{AccountCode: "2000", DebitAmount: 0, CreditAmount: 500},
	}

	resultLines, created, err := s.CreateJournal(ctx, h, lines)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Len(t, resultLines, 2)

	// Verify outbox row committed atomically with the domain state
	var eventType, aggType, aggID, storedTenantID, storedEntityID string
	var payloadBytes []byte
	err = pool.QueryRow(ctx, `
		SELECT event_type, aggregate_type, aggregate_id, tenant_id::text, legal_entity_id::text, payload
		FROM outbox_events
		WHERE aggregate_id = $1
	`, journalID).Scan(&eventType, &aggType, &aggID, &storedTenantID, &storedEntityID, &payloadBytes)
	require.NoError(t, err, "expected outbox row for journal.created to be present")

	assert.Equal(t, "journal.created", eventType)
	assert.Equal(t, "JOURNAL", aggType)
	assert.Equal(t, journalID, aggID)
	assert.Equal(t, tenantID, storedTenantID)
	assert.Equal(t, legalEntityID, storedEntityID)

	var env outbox.VariantAEnvelope
	require.NoError(t, json.Unmarshal(payloadBytes, &env))
	assert.Equal(t, "journal.created", env.EventType)
	assert.Equal(t, "general-ledger-svc", env.SourceService)
	assert.Equal(t, correlationID, env.CorrelationID)

	// Idempotent Replay: zero duplicate outbox events
	_, createdRetry, err := s.CreateJournal(ctx, h, lines)
	require.NoError(t, err)
	assert.False(t, createdRetry, "expected replay to return created=false")

	var outboxCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", journalID).Scan(&outboxCount)
	require.NoError(t, err)
	assert.Equal(t, 1, outboxCount, "expected exactly 1 outbox event across idempotent retries")
}

func TestPgStore_Outbox_TransitionJournal_ValidatedAndPosted_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	journalID := uuid.New().String()
	transactionDate := domain.NewDate(2026, time.August, 1)
	h := &domain.JournalHeader{
		JournalID:            journalID,
		TenantID:             tenantID,
		LegalEntityID:        legalEntityID,
		FiscalPeriod:         "2026-08",
		Status:               domain.JournalStatusPending,
		TransactionDate:      transactionDate,
		PostingDate:          transactionDate,
		CreatedByPrincipalID: "preparer-1",
		CorrelationID:        uuid.New().String(),
		ApprovalStatus:       domain.ApprovalStatusDraft,
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 250, CreditAmount: 0},
		{AccountCode: "2000", DebitAmount: 0, CreditAmount: 250},
	}

	_, _, err := s.CreateJournal(ctx, h, lines)
	require.NoError(t, err)

	// Transition PENDING -> VALIDATED
	err = s.TransitionJournal(ctx, tenantID, journalID, domain.JournalStatusPending, domain.JournalStatusValidated, "validator-1")
	require.NoError(t, err)

	var validatedCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'journal.validated'
	`, journalID).Scan(&validatedCount)
	require.NoError(t, err)
	assert.Equal(t, 1, validatedCount)

	// Transition VALIDATED -> FINALIZED
	err = s.TransitionJournal(ctx, tenantID, journalID, domain.JournalStatusValidated, domain.JournalStatusFinalized, "poster-1")
	require.NoError(t, err)

	// Verify both ledger entries and journal.posted outbox row committed atomically
	var postedCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'journal.posted'
	`, journalID).Scan(&postedCount)
	require.NoError(t, err)
	assert.Equal(t, 1, postedCount)

	var ledgerEntriesCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_entries
		WHERE journal_id = $1
	`, journalID).Scan(&ledgerEntriesCount)
	require.NoError(t, err)
	assert.Equal(t, 2, ledgerEntriesCount, "expected 2 posted ledger entries")
}

func TestPgStore_Outbox_ReverseJournal_Atomicity_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	originalJournalID := uuid.New().String()
	transactionDate := domain.NewDate(2026, time.August, 1)
	h := &domain.JournalHeader{
		JournalID:            originalJournalID,
		TenantID:             tenantID,
		LegalEntityID:        legalEntityID,
		FiscalPeriod:         "2026-08",
		Status:               domain.JournalStatusPending,
		TransactionDate:      transactionDate,
		PostingDate:          transactionDate,
		CreatedByPrincipalID: "preparer-1",
		CorrelationID:        uuid.New().String(),
		ApprovalStatus:       domain.ApprovalStatusPosted,
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 300, CreditAmount: 0},
		{AccountCode: "2000", DebitAmount: 0, CreditAmount: 300},
	}
	_, _, err := s.CreateJournal(ctx, h, lines)
	require.NoError(t, err)
	require.NoError(t, s.TransitionJournal(ctx, tenantID, originalJournalID, domain.JournalStatusPending, domain.JournalStatusValidated, "p1"))
	require.NoError(t, s.TransitionJournal(ctx, tenantID, originalJournalID, domain.JournalStatusValidated, domain.JournalStatusFinalized, "p1"))

	// Reverse the finalized journal
	reversingJournalID := uuid.New().String()
	reversingHeader := &domain.JournalHeader{
		JournalID:            reversingJournalID,
		TenantID:             tenantID,
		LegalEntityID:        legalEntityID,
		FiscalPeriod:         "2026-08",
		Status:               domain.JournalStatusFinalized,
		TransactionDate:      transactionDate,
		PostingDate:          transactionDate,
		ApprovalStatus:       domain.ApprovalStatusPosted,
		ReversalOfJournalID:  &originalJournalID,
		Description:          "Reversal",
		CreatedByPrincipalID: "reverser-1",
		CorrelationID:        uuid.New().String(),
	}
	reversingLines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 0, CreditAmount: 300},
		{AccountCode: "2000", DebitAmount: 300, CreditAmount: 0},
	}

	_, created, err := s.ReverseJournal(ctx, tenantID, originalJournalID, reversingHeader, reversingLines, "reverser-1")
	require.NoError(t, err)
	assert.True(t, created)

	// Verify original journal status is now REVERSED
	var originalStatus string
	err = pool.QueryRow(ctx, "SELECT status FROM journal_headers WHERE journal_id = $1", originalJournalID).Scan(&originalStatus)
	require.NoError(t, err)
	assert.Equal(t, string(domain.JournalStatusReversed), originalStatus)

	// Verify outbox event for journal.reversed exists with aggregate_id = originalJournalID
	var reversedPayloadBytes []byte
	err = pool.QueryRow(ctx, `
		SELECT payload FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'journal.reversed'
	`, originalJournalID).Scan(&reversedPayloadBytes)
	require.NoError(t, err)

	var env outbox.VariantAEnvelope
	require.NoError(t, json.Unmarshal(reversedPayloadBytes, &env))
	assert.Equal(t, "journal.reversed", env.EventType)

	var payloadData map[string]any
	require.NoError(t, json.Unmarshal(env.Payload, &payloadData))
	assert.Equal(t, originalJournalID, payloadData["journal_id"])
	assert.Equal(t, reversingJournalID, payloadData["reversing_journal_id"])

	// Critical check: DO NOT create a separate journal.posted event for the reversing journal
	var reversingPostedCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'journal.posted'
	`, reversingJournalID).Scan(&reversingPostedCount)
	require.NoError(t, err)
	assert.Equal(t, 0, reversingPostedCount, "reversing journal must NOT emit a separate journal.posted event")
}

func TestPgStore_Outbox_ForcedFailure_RollbackAtomicity_RealDB(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	journalID := uuid.New().String()
	correlationID := uuid.New().String()
	outboxEventID := uuid.New().String()

	// 1. Begin raw transaction
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck

	// Set tenant context for RLS in this tx
	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)

	// 2. Write the domain business row into journal_headers
	const insertJournalSQL = `
		INSERT INTO journal_headers (
			journal_id, tenant_id, legal_entity_id, fiscal_period, status,
			description, created_by_principal_id, correlation_id,
			journal_type, transaction_date, posting_date, currency_code, approval_status,
			created_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8,
			$9, $10, $11, $12, $13,
			now()
		)
	`
	transactionDate := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	_, err = tx.Exec(ctx, insertJournalSQL,
		journalID, tenantID, legalEntityID, "2026-08", "PENDING",
		"Rollback atomicity test", "preparer-1", correlationID,
		"STANDARD", transactionDate, transactionDate, "USD", "DRAFT",
	)
	require.NoError(t, err)

	// 3. Write the outbox row
	err = outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: outboxEventID,
		AggregateType: "JOURNAL",
		AggregateID:   journalID,
		EventType:     "journal.proposed",
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		CorrelationID: correlationID,
		Payload:       map[string]any{"journal_id": journalID},
	})
	require.NoError(t, err)

	// 4. Deliberately fail the transaction before commit
	// (Trigger duplicate primary key on outbox_events)
	_, err = tx.Exec(ctx, "INSERT INTO outbox_events (outbox_event_id) VALUES ($1)", outboxEventID)
	require.Error(t, err, "expected duplicate primary key constraint collision")

	err = tx.Rollback(ctx)
	require.NoError(t, err)

	// 5. Query fresh connection (pool) and assert NEITHER row exists
	var journalCount, outboxCount int
	scoped(t, pool, tenantID, func(verifyTx pgx.Tx) {
		err := verifyTx.QueryRow(ctx, "SELECT count(*) FROM journal_headers WHERE journal_id = $1", journalID).Scan(&journalCount)
		require.NoError(t, err)

		err = verifyTx.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", journalID).Scan(&outboxCount)
		require.NoError(t, err)
	})

	assert.Equal(t, 0, journalCount, "domain journal_headers row must not exist after rollback")
	assert.Equal(t, 0, outboxCount, "outbox row must not exist after rollback")
}

