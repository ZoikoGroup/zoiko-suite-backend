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

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

// storedEvent is one eventing_outbox row with its rendered envelope decoded.
type storedEvent struct {
	EventID, EventType, TenantID, Region, PayloadHash string
	Body                                              map[string]json.RawMessage
}

func (e storedEvent) str(t *testing.T, key string) string {
	t.Helper()
	var v string
	require.NoError(t, json.Unmarshal(e.Body[key], &v), "member %q", key)
	return v
}

func loadEvent(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, aggregateID, fact string) storedEvent {
	t.Helper()
	var e storedEvent
	var payload []byte
	err := pool.QueryRow(context.Background(), `
		SELECT event_id, event_type, tenant_id, region, payload_hash, payload
		  FROM eventing_outbox
		 WHERE aggregate_id = $1 AND event_type = $2`,
		aggregateID, "com.zoikosuite.accounting.journal."+fact).
		Scan(&e.EventID, &e.EventType, &e.TenantID, &e.Region, &e.PayloadHash, &payload)
	require.NoError(t, err, "expected an outbox row for journal.%s", fact)
	require.NoError(t, json.Unmarshal(payload, &e.Body))
	return e
}

func countEvents(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, aggregateID, fact string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM eventing_outbox WHERE aggregate_id = $1 AND event_type = $2`,
		aggregateID, "com.zoikosuite.accounting.journal."+fact).Scan(&n))
	return n
}

func TestPgStore_Outbox_CreateJournal_Atomicity_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))

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

	// The event committed with the journal, carrying the canonical envelope
	// (ZS-EVENT-001 §4) and the legacy names existing consumers parse.
	e := loadEvent(t, pool, journalID, "created")
	assert.Equal(t, tenantID, e.TenantID)
	assert.Equal(t, "uk", e.Region)
	assert.Equal(t, e.EventID, e.str(t, "id"), "row event_id and envelope id must be the same identity")
	assert.Equal(t, "com.zoikosuite.accounting.journal.created", e.str(t, "type"))
	assert.Equal(t, "urn:zoikosuite:service:general-ledger-svc", e.str(t, "source"))
	assert.Equal(t, "urn:zoikosuite:journal:"+journalID, e.str(t, "subject"))
	assert.Equal(t, legalEntityID, e.str(t, "legalentityid"))
	assert.Equal(t, correlationID, e.str(t, "correlationid"))
	assert.Equal(t, "preparer-1", e.str(t, "actorid"))
	assert.Equal(t, "uk", e.str(t, "residencyregion"))
	assert.Equal(t, "confidential", e.str(t, "classification"))
	assert.Equal(t, e.PayloadHash, e.str(t, "payloadhash"))
	assert.Equal(t, envelope.PayloadHash(e.Body["data"]), e.PayloadHash, "payloadhash must verify against the stored data bytes")
	assert.Equal(t, "journal.created", e.str(t, "event_type"), "legacy event_type kept for existing consumers")
	assert.Equal(t, e.EventID, e.str(t, "event_id"))

	// Idempotent Replay: zero duplicate outbox events
	_, createdRetry, err := s.CreateJournal(ctx, h, lines)
	require.NoError(t, err)
	assert.False(t, createdRetry, "expected replay to return created=false")

	assert.Equal(t, 1, countEvents(t, pool, journalID, "created"), "expected exactly 1 outbox event across idempotent retries")
}

func TestPgStore_Outbox_TransitionJournal_ValidatedAndPosted_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))

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

	assert.Equal(t, 1, countEvents(t, pool, journalID, "validated"))
	assert.Equal(t, "validator-1", loadEvent(t, pool, journalID, "validated").str(t, "actorid"))

	// Transition VALIDATED -> FINALIZED
	err = s.TransitionJournal(ctx, tenantID, journalID, domain.JournalStatusValidated, domain.JournalStatusFinalized, "poster-1")
	require.NoError(t, err)

	// Verify both ledger entries and journal.posted outbox row committed atomically
	assert.Equal(t, 1, countEvents(t, pool, journalID, "posted"))

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
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))

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

	// journal.reversed is a fact about the ORIGINAL journal's aggregate.
	rev := loadEvent(t, pool, originalJournalID, "reversed")
	var payloadData map[string]any
	require.NoError(t, json.Unmarshal(rev.Body["data"], &payloadData))
	assert.Equal(t, originalJournalID, payloadData["journal_id"])
	assert.Equal(t, reversingJournalID, payloadData["reversing_journal_id"])
	assert.Equal(t, "reverser-1", rev.str(t, "actorid"))

	// Critical check: DO NOT create a separate journal.posted event for the reversing journal
	assert.Equal(t, 0, countEvents(t, pool, reversingJournalID, "posted"),
		"reversing journal must NOT emit a separate journal.posted event")
}

func TestPgStore_Outbox_ForcedFailure_RollbackAtomicity_RealDB(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	journalID := uuid.New().String()
	correlationID := uuid.New().String()

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

	// 3. Write the outbox row through the shared library, in the same tx
	env, err := envelope.New(envelope.Spec{
		Type:            "com.zoikosuite.accounting.journal.created",
		Service:         "general-ledger-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   "journal",
		AggregateID:     journalID,
		CorrelationID:   correlationID,
		ResidencyRegion: "uk",
		Classification:  envelope.Confidential,
		Data:            map[string]any{"journal_id": journalID},
	})
	require.NoError(t, err)
	require.NoError(t, outbox.Enqueue(ctx, tx, env))

	// 4. Deliberately fail the transaction before commit: enqueueing the same
	// envelope again collides on event_id.
	require.Error(t, outbox.Enqueue(ctx, tx, env), "expected duplicate event_id collision")

	err = tx.Rollback(ctx)
	require.NoError(t, err)

	// 5. Query fresh connection (pool) and assert NEITHER row exists
	var journalCount, outboxCount int
	scoped(t, pool, tenantID, func(verifyTx pgx.Tx) {
		err := verifyTx.QueryRow(ctx, "SELECT count(*) FROM journal_headers WHERE journal_id = $1", journalID).Scan(&journalCount)
		require.NoError(t, err)

		err = verifyTx.QueryRow(ctx, "SELECT count(*) FROM eventing_outbox WHERE aggregate_id = $1", journalID).Scan(&outboxCount)
		require.NoError(t, err)
	})

	assert.Equal(t, 0, journalCount, "domain journal_headers row must not exist after rollback")
	assert.Equal(t, 0, outboxCount, "outbox row must not exist after rollback")
}

