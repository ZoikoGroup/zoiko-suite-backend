package accounting

import (
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

// JournalType defines the classification of a journal entry.
type JournalType string

const (
	JournalTypeStandard    JournalType = "STANDARD"
	JournalTypeAdjustment  JournalType = "ADJUSTMENT"
	JournalTypeReversal    JournalType = "REVERSAL"
	JournalTypeClosing     JournalType = "CLOSING"
	JournalTypeElimination JournalType = "ELIMINATION"
	JournalTypeAccrual     JournalType = "ACCRUAL"
)

// JournalStatus represents the posting state of a journal.
type JournalStatus string

const (
	JournalStatusDraft    JournalStatus = "DRAFT"
	JournalStatusApproved JournalStatus = "APPROVED"
	JournalStatusPosted   JournalStatus = "POSTED"
	JournalStatusReversed JournalStatus = "REVERSED"
)

// JournalEntry represents a balanced accounting header posted to one book/ledger context (ACC-JE).
// In accordance with ZS-DATA-001 Invariant D05 & D08:
// - A journal is book-aware (LegalEntity -> AccountingBook -> Ledger -> FiscalPeriod).
// - Once posted, it is immutable; corrections occur via reversing journals, never in-place edits.
type JournalEntry struct {
	JournalEntryID           types.UUID    `json:"journal_entry_id"`
	TenantID                 types.UUID    `json:"tenant_id"`
	LegalEntityID            types.UUID    `json:"legal_entity_id"`
	AccountingBookID         types.UUID    `json:"accounting_book_id"`
	LedgerID                 types.UUID    `json:"ledger_id"`
	FiscalPeriodID           types.UUID    `json:"fiscal_period_id"`
	JournalNumber            string        `json:"journal_number"` // Human sequential reference (scoped)
	JournalType              JournalType   `json:"journal_type"`
	PostingDate              time.Time     `json:"posting_date"`
	SourceAccountingEventID  *types.UUID   `json:"source_accounting_event_id,omitempty"`
	ReversalOfJournalEntryID *types.UUID   `json:"reversal_of_journal_entry_id,omitempty"`
	Status                   JournalStatus `json:"status"`
	PostedAt                 *time.Time    `json:"posted_at,omitempty"`
	PostedBy                 string        `json:"posted_by,omitempty"`
	Lines                    []JournalLine `json:"lines,omitempty"`
}

// JournalLine represents an atomic debit or credit posting line (ACC-JL).
// Debits and credits are tracked with exact NUMERIC(38,12) MoneyDecimal arithmetic (Invariant D06).
type JournalLine struct {
	JournalLineID       types.UUID         `json:"journal_line_id"`
	JournalEntryID      types.UUID         `json:"journal_entry_id"`
	LineNo              int                `json:"line_no"`
	AccountID           types.UUID         `json:"account_id"`
	TransactionCurrency string             `json:"transaction_currency"` // ISO 4217
	TransactionAmount   types.MoneyDecimal `json:"transaction_amount"`
	FunctionalCurrency  string             `json:"functional_currency"`  // ISO 4217
	FunctionalAmount    types.MoneyDecimal `json:"functional_amount"`
	DebitAmount         types.MoneyDecimal `json:"debit_amount"`  // Mutually exclusive with CreditAmount
	CreditAmount        types.MoneyDecimal `json:"credit_amount"` // Mutually exclusive with DebitAmount
	CounterpartyPartyID *types.UUID        `json:"counterparty_party_id,omitempty"`
	TaxComponentID      *types.UUID        `json:"tax_component_id,omitempty"`
	SourceLineRef       string             `json:"source_line_ref,omitempty"`
	Description         string             `json:"description,omitempty"`
}

// ValidateBalanced enforces the core ledger integrity constraint from ZS-DATA-001 §11:
// 1. Must contain at least two non-zero lines.
// 2. Each line must be either debit or credit, never both and never zero.
// 3. Total debits must exactly equal total credits in the book's functional currency.
func (j JournalEntry) ValidateBalanced() error {
	if len(j.Lines) < 2 {
		return fmt.Errorf("journal entry requires at least 2 lines; found %d", len(j.Lines))
	}

	totalDebit := types.ZeroMoney()
	totalCredit := types.ZeroMoney()

	for idx, line := range j.Lines {
		if line.AccountID.IsNil() {
			return fmt.Errorf("line %d: missing account_id", idx+1)
		}

		isDebit := !line.DebitAmount.IsZero()
		isCredit := !line.CreditAmount.IsZero()

		if !isDebit && !isCredit {
			return fmt.Errorf("line %d: line has zero debit and zero credit amount", idx+1)
		}
		if isDebit && isCredit {
			return fmt.Errorf("line %d: cannot have both debit and credit amounts populated on the same line", idx+1)
		}
		if line.DebitAmount.Sign() < 0 || line.CreditAmount.Sign() < 0 {
			return fmt.Errorf("line %d: negative debit/credit amounts are prohibited; invert line side instead", idx+1)
		}

		totalDebit = totalDebit.Add(line.DebitAmount)
		totalCredit = totalCredit.Add(line.CreditAmount)
	}

	if totalDebit.Cmp(totalCredit) != 0 {
		return fmt.Errorf("journal is out of balance: total debit (%s) != total credit (%s)",
			totalDebit.StringTrimmed(), totalCredit.StringTrimmed())
	}

	return nil
}
