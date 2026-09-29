package domain

import (
	"errors"
	"time"
)

// BNK-03 Statement Ingestion lifecycle. RECEIVED is the initial state for
// an ingested statement before its lines are validated; VALIDATING is
// in-progress; ACCEPTED/QUARANTINED/REJECTED are all terminal — see
// migration 004's own reject_terminal_statement_mutation trigger.
const (
	StatementReceived   = "RECEIVED"
	StatementValidating = "VALIDATING"
	StatementAccepted   = "ACCEPTED"
	StatementQuarantine = "QUARANTINED"
	StatementRejected   = "REJECTED"
)

func CanValidateStatement(status string) bool { return status == StatementReceived }
func CanAcceptStatement(status string) bool   { return status == StatementValidating }
func CanQuarantineStatement(status string) bool {
	return status == StatementReceived || status == StatementValidating
}
func CanReprocessQuarantinedStatement(status string) bool { return status == StatementQuarantine }

var ErrInvalidStatementTransition = errors.New("bank statement is not in a state that permits this action")

// ErrStatementBalanceMismatch is returned (never to the caller as a
// rejection — see ValidateStatement) when opening_balance + sum(included
// line amounts) != closing_balance for a statement, forcing an automatic
// QUARANTINED transition instead of allowing ACCEPT on unreconciled data.
var ErrStatementBalanceMismatch = errors.New("statement opening balance plus line amounts does not equal closing balance")

// StatementLine is one immutable evidence row within an ingested statement
// — see migration 004's reject_statement_line_mutation.
type StatementLine struct {
	LineID       string    `json:"line_id"`
	StatementID  string    `json:"statement_id"`
	TenantID     string    `json:"tenant_id"`
	LineSeq      int       `json:"line_seq"`
	PostedDate   time.Time `json:"posted_date"`
	Amount       float64   `json:"amount"`
	Currency     string    `json:"currency"`
	Description  string    `json:"description"`
	RawReference string    `json:"raw_reference"`
	BankCode     string    `json:"bank_code"`
	CreatedAt    time.Time `json:"created_at"`
}

type IngestStatementLinesRequest struct {
	ConnectionID    string            `json:"connection_id"`
	StatementFormat string            `json:"statement_format"`
	StatementDate   time.Time         `json:"statement_date"`
	ContentHash     string            `json:"content_hash"`
	SourceID        string            `json:"source_id"`
	ImportBatchID   string            `json:"import_batch_id"`
	OpeningBalance  float64           `json:"opening_balance"`
	ClosingBalance  float64           `json:"closing_balance"`
	Lines           []StatementLineIn `json:"lines"`
}

type StatementLineIn struct {
	PostedDate   time.Time `json:"posted_date"`
	Amount       float64   `json:"amount"`
	Currency     string    `json:"currency"`
	Description  string    `json:"description"`
	RawReference string    `json:"raw_reference"`
	BankCode     string    `json:"bank_code"`
}

type IngestStatementResult struct {
	Statement BankStatement   `json:"statement"`
	Lines     []StatementLine `json:"lines"`
	Created   bool            `json:"created"`
}

// BNK-04 Transaction Normalization. NORMALIZED is the steady state;
// QUARANTINED means a mapping_exceptions row is open against this line
// instead; SUPERSEDED is terminal — see migration 004's
// reject_canonical_txn_mutation.
const (
	TxnNormalized  = "NORMALIZED"
	TxnQuarantined = "QUARANTINED"
	TxnSuperseded  = "SUPERSEDED"
)

var (
	ErrInvalidTransactionTransition = errors.New("canonical transaction is not in a state that permits this action")
	ErrTransactionNotFound          = errors.New("canonical transaction not found")
)

type CanonicalTransaction struct {
	TransactionID         string     `json:"transaction_id"`
	TenantID              string     `json:"tenant_id"`
	StatementLineID       string     `json:"statement_line_id"`
	MappingVersion        int        `json:"mapping_version"`
	TransactionDate       time.Time  `json:"transaction_date"`
	Amount                float64    `json:"amount"`
	Currency              string     `json:"currency"`
	Category              string     `json:"category"`
	Counterparty          string     `json:"counterparty"`
	Status                string     `json:"status"`
	SupersededBy          string     `json:"superseded_by,omitempty"`
	CreatedByPrincipalID  string     `json:"-"`
	CreatedAt             time.Time  `json:"created_at"`
}

// NormalizeTransactionParams no longer carries a caller-supplied Category
// — see NormalizeTransactionResult. The category is resolved exclusively
// from bank_transaction_mappings, keyed by the statement line's own
// bank_code (real evidence captured at ingest), never accepted as a
// caller-supplied guess. Counterparty remains passthrough descriptive
// evidence; there is no counterparty dictionary to check it against.
type NormalizeTransactionParams struct {
	TenantID, StatementLineID, Counterparty string
	TransactionDate                         time.Time
	Amount                                  float64
	Currency                                string
	ActorPrincipalID                        string
}

// NormalizeTransactionResult is exactly one of its two fields: either the
// line's bank_code resolved against the mapping dictionary and a real
// NORMALIZED canonical transaction was created, or it didn't and the line
// was automatically routed to a mapping exception instead of guessing.
type NormalizeTransactionResult struct {
	Transaction          *CanonicalTransaction `json:"transaction,omitempty"`
	QuarantinedException *MappingException     `json:"quarantined_exception,omitempty"`
}

var ErrStatementLineNotFound = errors.New("statement line not found")

// TransactionMapping is one tenant-scoped bank_code -> category dictionary
// entry (migration 005).
type TransactionMapping struct {
	MappingID            string    `json:"mapping_id"`
	TenantID              string    `json:"tenant_id"`
	BankCode               string    `json:"bank_code"`
	Category               string    `json:"category"`
	CreatedByPrincipalID   string    `json:"-"`
	CreatedAt              time.Time `json:"created_at"`
}

type CreateTransactionMappingParams struct {
	TenantID, BankCode, Category, ActorPrincipalID string
}

var ErrMappingAlreadyExists = errors.New("a mapping for this bank_code already exists for this tenant")

// ReNormalizeTransactionParams supersedes an existing NORMALIZED canonical
// row with a corrected one, preserving the prior version rather than
// editing it in place.
type ReNormalizeTransactionParams struct {
	TenantID, PriorTransactionID, Category, Counterparty string
	TransactionDate                                       time.Time
	Amount                                                 float64
	Currency                                               string
	ActorPrincipalID                                       string
}

type QuarantineTransactionParams struct {
	TenantID, StatementLineID, Reason, ActorPrincipalID string
}

// ApproveMappingExceptionParams is BNK-04's maker-checker resolution: the
// principal approving must differ from the one who raised the exception
// (see the app-layer self-approval check in the handler, mirroring every
// other maker-checker flow in this codebase).
type ApproveMappingExceptionParams struct {
	TenantID, ExceptionID, Category, Counterparty string
	TransactionDate                                time.Time
	Amount                                          float64
	Currency                                        string
	ApproverPrincipalID                             string
}

var (
	ErrMappingExceptionNotFound     = errors.New("mapping exception not found")
	ErrMappingExceptionNotOpen      = errors.New("mapping exception is not OPEN")
	ErrMappingExceptionSelfApproval = errors.New("the principal who raised a mapping exception cannot also approve it")
)

type MappingException struct {
	ExceptionID            string     `json:"exception_id"`
	TenantID               string     `json:"tenant_id"`
	StatementLineID        string     `json:"statement_line_id"`
	Reason                 string     `json:"reason"`
	Status                 string     `json:"status"`
	RaisedByPrincipalID    string     `json:"raised_by_principal_id"`
	ResolvedTransactionID  string     `json:"resolved_transaction_id,omitempty"`
	ResolvedByPrincipalID  string     `json:"resolved_by_principal_id,omitempty"`
	ResolvedAt             *time.Time `json:"resolved_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
}
