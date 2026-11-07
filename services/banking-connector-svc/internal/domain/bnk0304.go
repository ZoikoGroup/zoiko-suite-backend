package domain

import (
	"errors"
	"time"
)

// BNK-03 Statement Ingestion lifecycle. RECEIVED is the initial state for
// an ingested statement before its lines are validated; VALIDATING is
// in-progress; ACCEPTED and REJECTED are terminal. QUARANTINED is terminal
// for ordinary writes but may be reset to RECEIVED by the explicit
// ReprocessQuarantinedStatement path — see migration 008's narrow exception
// to migration 004's reject_terminal_statement_mutation trigger.
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
	TransactionID        string    `json:"transaction_id"`
	TenantID             string    `json:"tenant_id"`
	StatementLineID      string    `json:"statement_line_id"`
	MappingVersion       int       `json:"mapping_version"`
	TransactionDate      time.Time `json:"transaction_date"`
	Amount               float64   `json:"amount"`
	Currency             string    `json:"currency"`
	Category             string    `json:"category"`
	Counterparty         string    `json:"counterparty"`
	Status               string    `json:"status"`
	SupersededBy         string    `json:"superseded_by,omitempty"`
	CreatedByPrincipalID string    `json:"-"`
	CreatedAt            time.Time `json:"created_at"`
}

// NormalizeTransactionParams no longer carries a required caller-supplied
// Category — see NormalizeTransactionResult. The category is resolved
// exclusively from bank_transaction_mappings, keyed by the statement line's
// own bank_code (real evidence captured at ingest), never accepted as a
// caller-supplied guess. Counterparty remains passthrough descriptive
// evidence; there is no counterparty dictionary to check it against.
//
// TransactionDate, Amount and Currency are optional assertions: when the
// caller supplies one it must exactly match the immutable statement line,
// and a mismatch is rejected with ErrTransactionEvidenceMismatch. The
// canonical row is always built from the statement line's raw values, never
// from the request. Omitting them is the normal path.
type NormalizeTransactionParams struct {
	TenantID, StatementLineID, Counterparty string
	TransactionDate                         *time.Time
	Amount                                  *float64
	Currency                                string
	ActorPrincipalID                        string
	CorrelationID                           string
}

// NormalizeTransactionResult is exactly one of its two fields: either the
// line's bank_code resolved against the mapping dictionary and a real
// NORMALIZED canonical transaction was created, or it didn't and the line
// was automatically routed to a mapping exception instead of guessing.
type NormalizeTransactionResult struct {
	Transaction          *CanonicalTransaction `json:"transaction,omitempty"`
	QuarantinedException *MappingException     `json:"quarantined_exception,omitempty"`
}

var (
	ErrStatementLineNotFound = errors.New("statement line not found")
	// ErrTransactionEvidenceMismatch is returned when a caller asserts a
	// booking date, amount or currency that contradicts the immutable raw
	// statement line. The raw line always wins; a mismatch is refused so a
	// client cannot believe it wrote a fact the service did not store.
	ErrTransactionEvidenceMismatch = errors.New("caller-supplied transaction evidence does not match the statement line")
)

// TransactionMapping is one tenant-scoped bank_code -> category dictionary
// entry (migration 005).
type TransactionMapping struct {
	MappingID            string    `json:"mapping_id"`
	TenantID             string    `json:"tenant_id"`
	BankCode             string    `json:"bank_code"`
	Category             string    `json:"category"`
	CreatedByPrincipalID string    `json:"-"`
	CreatedAt            time.Time `json:"created_at"`
}

type CreateTransactionMappingParams struct {
	TenantID, BankCode, Category, ActorPrincipalID string
}

var ErrMappingAlreadyExists = errors.New("a mapping for this bank_code already exists for this tenant")

// ReNormalizeTransactionParams supersedes an existing NORMALIZED canonical
// row with a corrected one, preserving the prior version rather than
// editing it in place. Category is the deliberate correction; date, amount
// and currency remain raw statement-line evidence and cannot be changed by
// a re-normalization. TransactionDate/Amount/Currency are optional
// assertions, checked exactly like NormalizeTransactionParams.
type ReNormalizeTransactionParams struct {
	TenantID, PriorTransactionID, Category, Counterparty string
	TransactionDate                                      *time.Time
	Amount                                               *float64
	Currency                                             string
	ActorPrincipalID                                     string
	CorrelationID                                        string
}

type QuarantineTransactionParams struct {
	TenantID, StatementLineID, Reason, ActorPrincipalID string
	CorrelationID                                       string
}

// ApproveMappingExceptionParams is BNK-04's maker-checker resolution: the
// principal approving must differ from the one who raised the exception
// (see the app-layer self-approval check in the handler, mirroring every
// other maker-checker flow in this codebase). Category and Counterparty are
// the approver's decision; date, amount and currency come from the raw
// statement line. The date/amount/currency fields are optional assertions,
// checked exactly like NormalizeTransactionParams.
type ApproveMappingExceptionParams struct {
	TenantID, ExceptionID, Category, Counterparty string
	TransactionDate                               *time.Time
	Amount                                        *float64
	Currency                                      string
	ApproverPrincipalID                           string
	CorrelationID                                 string
}

var (
	ErrMappingExceptionNotFound     = errors.New("mapping exception not found")
	ErrMappingExceptionNotOpen      = errors.New("mapping exception is not OPEN")
	ErrMappingExceptionSelfApproval = errors.New("the principal who raised a mapping exception cannot also approve it")
)

type MappingException struct {
	ExceptionID           string     `json:"exception_id"`
	TenantID              string     `json:"tenant_id"`
	StatementLineID       string     `json:"statement_line_id"`
	Reason                string     `json:"reason"`
	Status                string     `json:"status"`
	RaisedByPrincipalID   string     `json:"raised_by_principal_id"`
	ResolvedTransactionID string     `json:"resolved_transaction_id,omitempty"`
	ResolvedByPrincipalID string     `json:"resolved_by_principal_id,omitempty"`
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
}

// ListTransactionsQuery is BNK-04's ListTransactions filter (spec table 23).
// It returns only NORMALIZED canonical transactions: a SUPERSEDED version has
// been replaced by a correction and a QUARANTINED one is not current truth,
// so neither may reach a reconciling caller as a live fact. Tenant scope is
// not a field: row-level security on every joined table scopes the read to
// the request's tenant (app.tenant_id).
type ListTransactionsQuery struct {
	LegalEntityID string // required; authorization is per legal entity
	BankAccountID string // optional; the connection's treasury-svc BNK-01 account
	Currency      string // optional
	// From / ToExclusive bound transaction_date in UTC: From is the first
	// instant included, ToExclusive the first instant after the window. nil
	// leaves that end open.
	From, ToExclusive *time.Time
	// AfterTransactionID is the keyset cursor: rows with a transaction_id
	// after it, in byte order.
	AfterTransactionID string
	Limit              int
}

// CanonicalTransactionListItem is one ListTransactions row. It is a separate
// type from CanonicalTransaction on purpose: Amount is the exact
// NUMERIC(18,4) value as a decimal string with four places, never a float64
// (ZS-DATA-001 D06), and BankAccountID / ConnectionID say which account the
// transaction belongs to without a second lookup.
type CanonicalTransactionListItem struct {
	TransactionID string `json:"transaction_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ConnectionID  string `json:"connection_id"`
	// BankAccountID is null for a connection created before BNK-02 recorded
	// one (bank_connections.bank_account_id = ''); such a transaction is
	// never returned when a bank_account_id filter is given.
	BankAccountID   *string   `json:"bank_account_id"`
	StatementLineID string    `json:"statement_line_id"`
	MappingVersion  int       `json:"mapping_version"`
	TransactionDate time.Time `json:"transaction_date"`
	Amount          string    `json:"amount"`
	Currency        string    `json:"currency"`
	Category        string    `json:"category"`
	Counterparty    string    `json:"counterparty"`
	Status          string    `json:"status"`
}

// CanonicalTransactionPage is one keyset page of ListTransactions. A non-empty
// NextCursor means more rows exist and the page must not be read as the whole
// result; an empty one means this page ends the result.
type CanonicalTransactionPage struct {
	Transactions []CanonicalTransactionListItem `json:"transactions"`
	NextCursor   string                         `json:"next_cursor,omitempty"`
}

// RawLineage is GetRawLineage's response (table 23's "Queries/retrieval",
// table 25's "Raw transaction hash/ID, canonical version, mapping/version").
// It traces one canonical transaction back through its immutable statement
// line to the statement and connection it arrived on. There is no
// per-transaction raw hash column in this schema (migration 004):
// StatementContentHash is the whole statement file's hash
// (bank_statements.content_hash), not a per-line one — RawReference is the
// only per-line raw evidence this service actually stores, and this type
// never pretends otherwise.
type RawLineage struct {
	TransactionID   string    `json:"transaction_id"`
	TenantID        string    `json:"tenant_id"`
	Status          string    `json:"status"`
	MappingVersion  int       `json:"mapping_version"`
	SupersededBy    string    `json:"superseded_by,omitempty"`
	StatementLineID string    `json:"statement_line_id"`
	LineSeq         int       `json:"line_seq"`
	RawReference    string    `json:"raw_reference"`
	BankCode        string    `json:"bank_code"`
	PostedDate      time.Time `json:"posted_date"`
	StatementID     string    `json:"statement_id"`
	// StatementContentHash is the statement-level hash recorded at BNK-03
	// ingestion, not a per-transaction one — see the type comment.
	StatementContentHash string  `json:"statement_content_hash"`
	StatementSourceID    string  `json:"statement_source_id"`
	ConnectionID         string  `json:"connection_id"`
	LegalEntityID        string  `json:"legal_entity_id"`
	BankAccountID        *string `json:"bank_account_id,omitempty"`
}

// NormalizationVersion is GetNormalizationVersion's response. Caveat carries
// A19 (SPEC_DEVIATIONS.md) into the response itself, not just a code
// comment: mapping_version is honestly exposed as the per-line
// re-normalization counter it actually is, so a caller cannot mistake it
// for a mapping-dictionary, parser or schema version.
type NormalizationVersion struct {
	TransactionID  string `json:"transaction_id"`
	TenantID       string `json:"tenant_id"`
	LegalEntityID  string `json:"legal_entity_id"`
	Status         string `json:"status"`
	MappingVersion int    `json:"mapping_version"`
	Caveat         string `json:"caveat"`
}

// MappingVersionCaveat is NormalizationVersion.Caveat's fixed text.
const MappingVersionCaveat = "mapping_version is an integer incremented on each re-normalization of this statement line (1 at first normalization or exception approval, prior+1 at each re-normalize); it is not an identifier or version of the bank_transaction_mappings dictionary entry, parser or schema that produced this row, and cannot be used to reproduce which mapping rule was applied (SPEC_DEVIATIONS.md A19)."

// ListMappingExceptionsQuery is ListNormalizationExceptions' filter. Status
// narrows to one lifecycle value (OPEN/APPROVED/REJECTED); empty means any.
// mapping_exceptions has no legal_entity_id column — it is a platform-scoped
// control queue, the same posture ApproveMappingException and
// CreateTransactionMapping already use — so there is no legal-entity filter.
type ListMappingExceptionsQuery struct {
	Status           string
	AfterExceptionID string
	Limit            int
}

// MappingExceptionPage is one keyset page of ListNormalizationExceptions.
type MappingExceptionPage struct {
	Exceptions []MappingException `json:"exceptions"`
	NextCursor string             `json:"next_cursor,omitempty"`
}
