package banking

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"zoiko.io/contract/types"
)

// ConnectionStatus represents the live state of a bank connection.
type ConnectionStatus string

const (
	ConnectionStatusActive       ConnectionStatus = "ACTIVE"
	ConnectionStatusDisconnected ConnectionStatus = "DISCONNECTED"
	ConnectionStatusConsentNeed  ConnectionStatus = "CONSENT_REQUIRED"
)

// MatchStatus represents the reconciliation state of a bank transaction.
type MatchStatus string

const (
	MatchStatusUnmatched MatchStatus = "UNMATCHED"
	MatchStatusMatched   MatchStatus = "MATCHED"
	MatchStatusException MatchStatus = "EXCEPTION"
	MatchStatusResolved  MatchStatus = "RESOLVED"
)

// BankConnection represents an authenticated institutional connection to a financial account (BNK-CONN).
// In accordance with ZS-DATA-001 Invariant D14 (Privacy & Security):
// Raw account numbers and IBANs are stored via vault references/encrypted columns;
// display values must be masked (e.g. "****1234").
type BankConnection struct {
	BankConnectionID     types.UUID       `json:"bank_connection_id"`
	TenantID             types.UUID       `json:"tenant_id"`
	LegalEntityID        types.UUID       `json:"legal_entity_id"`
	InstitutionID        string           `json:"institution_id"`
	AccountNumberVaultRef string          `json:"account_number_vault_ref"`
	MaskedAccountNumber  string           `json:"masked_account_number"`
	IBANVaultRef         string           `json:"iban_vault_ref,omitempty"`
	MaskedIBAN           string           `json:"masked_iban,omitempty"`
	CurrencyCode         string           `json:"currency_code"` // ISO 4217
	Status               ConnectionStatus `json:"status"`
	CreatedAt            time.Time        `json:"created_at"`
}

// MaskSensitiveIdentifier masks all digits except the last 4 characters.
func MaskSensitiveIdentifier(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) <= 4 {
		return "****"
	}
	visible := raw[len(raw)-4:]
	return strings.Repeat("*", len(raw)-4) + visible
}

// BankStatement represents an official periodic bank statement (BNK-STMT).
type BankStatement struct {
	BankStatementID  types.UUID         `json:"bank_statement_id"`
	TenantID         types.UUID         `json:"tenant_id"`
	BankConnectionID types.UUID         `json:"bank_connection_id"`
	StatementNumber  string             `json:"statement_number"`
	StatementDate    time.Time          `json:"statement_date"`
	OpeningBalance   types.MoneyDecimal `json:"opening_balance"`
	ClosingBalance   types.MoneyDecimal `json:"closing_balance"`
	CurrencyCode     string             `json:"currency_code"`
	Transactions     []BankTransaction  `json:"transactions,omitempty"`
	CreatedAt        time.Time          `json:"created_at"`
}

// ValidateArithmetic verifies Invariant D15:
// Opening Balance + Sum(Transaction Amounts) must exactly equal Closing Balance.
func (s BankStatement) ValidateArithmetic() error {
	netMovement := types.ZeroMoney()
	for _, tx := range s.Transactions {
		netMovement = netMovement.Add(tx.Amount)
	}

	computedClosing := s.OpeningBalance.Add(netMovement)
	if computedClosing.Cmp(s.ClosingBalance) != 0 {
		return fmt.Errorf("bank statement arithmetic mismatch: opening (%s) + movements (%s) = %s, but statement closing is %s",
			s.OpeningBalance.StringTrimmed(),
			netMovement.StringTrimmed(),
			computedClosing.StringTrimmed(),
			s.ClosingBalance.StringTrimmed(),
		)
	}
	return nil
}

// BankTransaction represents an atomic debit or credit movement on a bank account (BNK-TXN).
type BankTransaction struct {
	BankTransactionID types.UUID         `json:"bank_transaction_id"`
	TenantID          types.UUID         `json:"tenant_id"`
	BankStatementID   types.UUID         `json:"bank_statement_id"`
	TransactionDate   time.Time          `json:"transaction_date"`
	ValueDate         time.Time          `json:"value_date"`
	Amount            types.MoneyDecimal `json:"amount"` // Signed: positive = credit/inflow, negative = debit/outflow
	CurrencyCode      string             `json:"currency_code"`
	BankReference     string             `json:"bank_reference"`
	CounterpartyName  string             `json:"counterparty_name,omitempty"`
	MatchStatus       MatchStatus        `json:"match_status"`
}

// BankReconciliationMatch links an authoritative bank transaction to a finalized GL journal or payment (BNK-MATCH).
type BankReconciliationMatch struct {
	MatchID           types.UUID         `json:"match_id"`
	TenantID          types.UUID         `json:"tenant_id"`
	BankTransactionID types.UUID         `json:"bank_transaction_id"`
	MatchedObjectType string             `json:"matched_object_type"` // e.g. "journal_entry", "cash_receipt", "payment_execution"
	MatchedObjectID   types.UUID         `json:"matched_object_id"`
	MatchedAmount     types.MoneyDecimal `json:"matched_amount"`
	DifferenceAmount  types.MoneyDecimal `json:"difference_amount"`
	MatchedAt         time.Time          `json:"matched_at"`
	MatchedBy         string             `json:"matched_by"` // "RULE_ENGINE" or user principal
}

// Validate checks match properties.
func (m BankReconciliationMatch) Validate() error {
	if m.MatchID.IsNil() || m.TenantID.IsNil() || m.BankTransactionID.IsNil() || m.MatchedObjectID.IsNil() {
		return errors.New("bank reconciliation match requires non-nil IDs")
	}
	if m.MatchedAmount.IsZero() {
		return errors.New("matched amount cannot be zero")
	}
	return nil
}
