package domain

import (
	"errors"
	"time"
)

var (
	ErrConnectionNotFound = errors.New("bank connection not found")
	ErrStatementNotFound  = errors.New("bank statement not found")
)

const (
	StatusConnected    = "CONNECTED"
	StatusDisconnected = "DISCONNECTED"

	FormatISO20022 = "ISO20022"
	FormatSWIFT    = "SWIFT_MT940"
	FormatBAI2     = "BAI2"
)

type BankConnection struct {
	ConnectionID  string    `json:"connection_id"`
	AccountID     string    `json:"account_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	BankName      string    `json:"bank_name"`
	BIC           string    `json:"bic,omitempty"`
	SwiftBIC      string    `json:"swift_bic,omitempty"`
	AccountNumber string    `json:"account_number,omitempty"`
	IBAN          string    `json:"iban,omitempty"`
	Currency      string    `json:"currency"`
	AccountType   string    `json:"account_type,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type BankStatement struct {
	StatementID      string    `json:"statement_id"`
	ConnectionID     string    `json:"connection_id"`
	TenantID         string    `json:"tenant_id"`
	StatementFormat  string    `json:"statement_format"`
	StatementDate    time.Time `json:"statement_date"`
	OpeningBalance   float64   `json:"opening_balance"`
	ClosingBalance   float64   `json:"closing_balance"`
	TransactionCount int       `json:"transaction_count"`
	IngestedAt       time.Time `json:"ingested_at"`
}

type CreateConnectionRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	BankName      string `json:"bank_name"`
	BIC           string `json:"bic"`
	SwiftBIC      string `json:"swift_bic"`
	AccountNumber string `json:"account_number"`
	IBAN          string `json:"iban"`
	Currency      string `json:"currency"`
	AccountType   string `json:"account_type"`
}

type IngestStatementRequest struct {
	ConnectionID    string    `json:"connection_id"`
	StatementFormat string    `json:"statement_format"`
	StatementDate   time.Time `json:"statement_date"`
	OpeningBalance  float64   `json:"opening_balance"`
	ClosingBalance  float64   `json:"closing_balance"`
	RawContent      string    `json:"raw_content"`
}
