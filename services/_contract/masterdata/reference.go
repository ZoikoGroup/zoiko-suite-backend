package masterdata

import (
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

// BookType defines the accounting basis/purpose applied to a legal entity.
type BookType string

const (
	BookTypeStatutory     BookType = "STATUTORY"
	BookTypeManagement    BookType = "MANAGEMENT"
	BookTypeTax           BookType = "TAX"
	BookTypeConsolidation BookType = "CONSOLIDATION"
	BookTypeOther         BookType = "OTHER"
)

// LedgerType defines posting container specialization.
type LedgerType string

const (
	LedgerTypePrimary     LedgerType = "PRIMARY"
	LedgerTypeAdjustment  LedgerType = "ADJUSTMENT"
	LedgerTypeTax         LedgerType = "TAX"
	LedgerTypeElimination LedgerType = "ELIMINATION"
	LedgerTypeStatistical LedgerType = "STATISTICAL"
)

// CurrencyBasis defines which currency bases are active for ledger posting.
type CurrencyBasis string

const (
	CurrencyBasisFunctional  CurrencyBasis = "FUNCTIONAL"
	CurrencyBasisTransaction CurrencyBasis = "TRANSACTION"
	CurrencyBasisReporting   CurrencyBasis = "REPORTING"
)

// AccountingBook represents an accounting basis applied to a legal entity (REF-BOOK).
// Multi-book accounting allows Statutory, Management and Tax views to diverge while sharing source documents.
type AccountingBook struct {
	AccountingBookID    types.UUID `json:"accounting_book_id"`
	TenantID            types.UUID `json:"tenant_id"`
	LegalEntityID       types.UUID `json:"legal_entity_id"`
	BookCode            string     `json:"book_code"` // Unique within legal entity
	BookType            BookType   `json:"book_type"`
	AccountingFramework string     `json:"accounting_framework"` // e.g. IFRS, US_GAAP, UK_GAAP
	FunctionalCurrency  string     `json:"functional_currency"`  // ISO 4217
	ReportingCurrency   string     `json:"reporting_currency,omitempty"`
	FiscalCalendarID    types.UUID `json:"fiscal_calendar_id"`
	ValidFrom           time.Time  `json:"valid_from"`
	ValidTo             *time.Time `json:"valid_to,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// Validate checks accounting book rules.
func (b AccountingBook) Validate() error {
	if b.AccountingBookID.IsNil() || b.TenantID.IsNil() || b.LegalEntityID.IsNil() {
		return errors.New("accounting_book requires non-nil IDs")
	}
	if b.BookCode == "" {
		return errors.New("accounting_book requires book_code")
	}
	if b.BookType == "" {
		return errors.New("accounting_book requires book_type")
	}
	if b.FunctionalCurrency == "" || len(b.FunctionalCurrency) != 3 {
		return fmt.Errorf("invalid functional currency: %q", b.FunctionalCurrency)
	}
	return nil
}

// Ledger represents a posting container under an accounting book (REF-LEDGER).
type Ledger struct {
	LedgerID            types.UUID    `json:"ledger_id"`
	TenantID            types.UUID    `json:"tenant_id"`
	AccountingBookID    types.UUID    `json:"accounting_book_id"`
	LedgerCode          string        `json:"ledger_code"` // Unique within book
	LedgerType          LedgerType    `json:"ledger_type"`
	PostingCurrencyBasis CurrencyBasis `json:"posting_currency_basis"`
	IsActive            bool          `json:"is_active"`
	ValidFrom           time.Time     `json:"valid_from"`
	ValidTo             *time.Time    `json:"valid_to,omitempty"`
	CreatedAt           time.Time     `json:"created_at"`
}

// Validate checks ledger rules.
func (l Ledger) Validate() error {
	if l.LedgerID.IsNil() || l.TenantID.IsNil() || l.AccountingBookID.IsNil() {
		return errors.New("ledger requires non-nil IDs")
	}
	if l.LedgerCode == "" {
		return errors.New("ledger requires ledger_code")
	}
	if l.LedgerType == "" {
		return errors.New("ledger requires ledger_type")
	}
	return nil
}
