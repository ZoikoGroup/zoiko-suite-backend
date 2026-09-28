package accounting

import (
	"encoding/json"
	"testing"
	"time"

	"zoiko.io/contract/types"
)

func TestJournalEntry_BalancedSuccess(t *testing.T) {
	entry := JournalEntry{
		JournalEntryID:   types.MustNewV7(),
		TenantID:         types.MustNewV7(),
		LegalEntityID:    types.MustNewV7(),
		AccountingBookID: types.MustNewV7(),
		LedgerID:         types.MustNewV7(),
		FiscalPeriodID:   types.MustNewV7(),
		JournalNumber:    "JE-2026-0001",
		JournalType:      JournalTypeStandard,
		PostingDate:      time.Now().UTC(),
		Status:           JournalStatusApproved,
		Lines: []JournalLine{
			{
				JournalLineID:      types.MustNewV7(),
				LineNo:             1,
				AccountID:          types.MustNewV7(), // Cash account
				FunctionalCurrency: "EUR",
				DebitAmount:        types.MustParseMoney("1500.50"),
				CreditAmount:       types.ZeroMoney(),
			},
			{
				JournalLineID:      types.MustNewV7(),
				LineNo:             2,
				AccountID:          types.MustNewV7(), // Revenue account
				FunctionalCurrency: "EUR",
				DebitAmount:        types.ZeroMoney(),
				CreditAmount:       types.MustParseMoney("1500.50"),
			},
		},
	}

	if err := entry.ValidateBalanced(); err != nil {
		t.Fatalf("expected journal to balance, got error: %v", err)
	}
}

func TestJournalEntry_OutOfBalanceRejection(t *testing.T) {
	entry := JournalEntry{
		JournalEntryID: types.MustNewV7(),
		Lines: []JournalLine{
			{
				AccountID:   types.MustNewV7(),
				DebitAmount: types.MustParseMoney("1500.50"),
			},
			{
				AccountID:    types.MustNewV7(),
				CreditAmount: types.MustParseMoney("1500.49"), // 1 cent off
			},
		},
	}

	if err := entry.ValidateBalanced(); err == nil {
		t.Fatalf("expected out-of-balance journal to be rejected")
	}
}

func TestJournalEntry_SingleLineRejection(t *testing.T) {
	entry := JournalEntry{
		JournalEntryID: types.MustNewV7(),
		Lines: []JournalLine{
			{
				AccountID:   types.MustNewV7(),
				DebitAmount: types.MustParseMoney("100.00"),
			},
		},
	}

	if err := entry.ValidateBalanced(); err == nil {
		t.Fatalf("expected single-line journal to be rejected")
	}
}

func TestAccountingEvent_Validation(t *testing.T) {
	evt := AccountingEvent{
		AccountingEventID:    types.MustNewV7(),
		TenantID:             types.MustNewV7(),
		LegalEntityID:        types.MustNewV7(),
		SourceDomain:         "AR",
		SourceObjectTable:    "sales_invoices",
		SourceObjectID:       types.MustNewV7(),
		EventType:            "INVOICE_ISSUED",
		OccurredAt:           time.Now().UTC(),
		EffectiveDate:        time.Now().UTC(),
		AmountBasis:          json.RawMessage(`{"net_amount":"1000.00","tax_amount":"200.00","currency":"EUR"}`),
		PostingPolicyVersion: "v1.2",
		Status:               AccountingEventStatusPending,
		CreatedAt:            time.Now().UTC(),
	}

	if err := evt.Validate(); err != nil {
		t.Fatalf("accounting event validation failed: %v", err)
	}
}
