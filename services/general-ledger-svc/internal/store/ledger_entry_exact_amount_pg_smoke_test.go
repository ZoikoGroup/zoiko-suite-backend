package store_test

// SPEC_DEVIATIONS A15, "Float amounts": LedgerEntry.DebitAmount/CreditAmount
// are exact decimal strings now, not float64. ledger_entries.debit_amount is
// NUMERIC(18,2), whose range includes magnitudes above 2^53 minor units
// (cents) — e.g. $90,071,992,547,409.93 is 9,007,199,254,740,993 cents, one
// more than 2^53, which float64 cannot represent exactly. A row at that
// magnitude is inserted directly (bypassing CreateJournal, whose
// domain.JournalLine input is still float64 and so cannot carry such a
// value in losslessly in the first place — a separate, still-open gap this
// closure does not claim to fix) to isolate and prove QueryLedger's own
// read path: the NUMERIC column scans into a Go string exactly, with no
// float64 ever in between. Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

func TestPgStore_QueryLedger_AmountBeyond2to53Cents_RoundTripsExact_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	ctx := context.Background()
	tenantID := uuid.New().String()
	entity := uuid.New().String()
	journalID := uuid.New().String()
	lineID := uuid.New().String()

	const exactDebit = "90071992547409.93" // 9,007,199,254,740,993 cents == 2^53 + 1
	const exactCredit = "0.00"

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin insert tx: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		t.Fatalf("installing tenant scope failed: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (
			ledger_entry_id, tenant_id, legal_entity_id, book_id, fiscal_period,
			journal_id, journal_line_id, line_number, account_code,
			debit_amount, credit_amount, currency_code,
			transaction_date, posting_date, correlation_id, created_at
		) VALUES ($1,$2,$3,'','2026-07',$4,$5,1,'1000',$6::numeric,$7::numeric,'GBP','2026-07-10','2026-07-10',$8,$9)
	`, uuid.New().String(), tenantID, entity, journalID, lineID, exactDebit, exactCredit, uuid.New().String(), time.Now().UTC()); err != nil {
		t.Fatalf("direct insert of the boundary-magnitude entry failed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit insert tx: %v", err)
	}

	tenantCtx := svcmiddleware.WithTenant(ctx, tenantID)
	entries, _, err := s.QueryLedger(tenantCtx, tenantID, domain.QueryLedgerFilter{LegalEntityID: entity}, 10)
	if err != nil {
		t.Fatalf("QueryLedger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one entry, got %d", len(entries))
	}
	if entries[0].DebitAmount != exactDebit {
		t.Fatalf("expected DebitAmount to round-trip exactly as %q (float64 cannot represent this magnitude exactly), got %q",
			exactDebit, entries[0].DebitAmount)
	}
	if entries[0].CreditAmount != exactCredit {
		t.Fatalf("expected CreditAmount to round-trip exactly as %q, got %q", exactCredit, entries[0].CreditAmount)
	}
}
