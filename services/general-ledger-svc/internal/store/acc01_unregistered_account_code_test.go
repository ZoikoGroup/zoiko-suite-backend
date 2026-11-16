//go:build integration

package store_test

// SPEC_DEVIATIONS A9: ACC-04 deliberately lets an account code the chart has
// never heard of post (the bootstrap gap). Enforcing the opposite would be a
// cross-service flag-day decision this file does not make. This is the
// narrower, non-breaking step: the fact that an unregistered code was posted
// is now observable (enriches the existing journal.created outbox event) so
// real usage can be measured before anyone decides to enforce.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

func TestPgStore_CreateJournal_UnregisteredAccountCode_StillPostsAndIsObservable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	// "9999" is never registered via CreateAccount in this tenant.
	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		FiscalPeriod: "2026-11", Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: domain.NewDate(2026, 11, 7),
		PostingDate: domain.NewDate(2026, 11, 7), CurrencyCode: "GBP",
		CreatedByPrincipalID: "preparer-1", CorrelationID: "unregistered-code-smoke-1",
		ApprovalStatus: domain.ApprovalStatusDraft,
	}
	lines := []domain.JournalLine{
		{AccountCode: "9999", DebitAmount: 100},
		{AccountCode: "9999", CreditAmount: 100},
	}
	if _, created, err := s.CreateJournal(ctx, h, lines); err != nil || !created {
		t.Fatalf("CreateJournal must still succeed for an unregistered account code (the bootstrap gap is deliberate): created=%v err=%v", created, err)
	}

	var payload string
	if err := pool.QueryRow(ctx,
		`SELECT convert_from(payload, 'UTF8') FROM eventing_outbox WHERE event_type LIKE '%journal.created%' AND aggregate_id = $1`,
		h.JournalID,
	).Scan(&payload); err != nil {
		t.Fatalf("query outbox payload: %v", err)
	}
	// One line debits and one credits the same unregistered code — the
	// enrichment names it once, not once per line. (The envelope mirrors its
	// payload under both "data" and "payload" keys, so this checks the
	// array's own shape rather than a raw substring count across the whole
	// envelope.)
	if !strings.Contains(payload, `"unregistered_account_codes":["9999"]`) {
		t.Fatalf("journal.created outbox payload does not name the unregistered account code exactly once: %s", payload)
	}
}

func TestPgStore_CreateJournal_AllAccountsRegistered_NoUnregisteredCodeField(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	cash := newTestAccount(tenantID, "1000", "Cash")
	if err := s.CreateAccount(ctx, cash); err != nil {
		t.Fatalf("CreateAccount(cash): %v", err)
	}
	sales := newTestAccount(tenantID, "4000", "Sales")
	if err := s.CreateAccount(ctx, sales); err != nil {
		t.Fatalf("CreateAccount(sales): %v", err)
	}

	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		FiscalPeriod: "2026-11", Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: domain.NewDate(2026, 11, 7),
		PostingDate: domain.NewDate(2026, 11, 7), CurrencyCode: "GBP",
		CreatedByPrincipalID: "preparer-1", CorrelationID: "registered-code-smoke-1",
		ApprovalStatus: domain.ApprovalStatusDraft,
	}
	lines := []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 100},
		{AccountCode: "4000", CreditAmount: 100},
	}
	if _, created, err := s.CreateJournal(ctx, h, lines); err != nil || !created {
		t.Fatalf("CreateJournal: created=%v err=%v", created, err)
	}

	var payload string
	if err := pool.QueryRow(ctx,
		`SELECT convert_from(payload, 'UTF8') FROM eventing_outbox WHERE event_type LIKE '%journal.created%' AND aggregate_id = $1`,
		h.JournalID,
	).Scan(&payload); err != nil {
		t.Fatalf("query outbox payload: %v", err)
	}
	if strings.Contains(payload, "unregistered_account_codes") {
		t.Fatalf("journal.created outbox payload must not carry unregistered_account_codes when every line's account is registered: %s", payload)
	}
}
