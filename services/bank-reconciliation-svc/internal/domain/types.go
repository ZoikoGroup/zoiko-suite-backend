// Package domain defines the authoritative domain types for bank-reconciliation-svc.
//
// Per docs/architecture/03-microservices.md §10.5, this service owns
// statement matching, reconciliation state, exception queues, and
// reconciliation evidence. Matching is verified against general-ledger-svc's
// real FINALIZED journals (status, legal entity, and net amount all
// cross-checked) — a suggestion may one day be intelligence-assisted, but
// final reconciliation state always remains governed and evidential
// (the spec's critical constraint). Intelligence-assisted suggestion
// scoring is out of scope for v1 — matching here is always an explicit,
// caller-supplied journal_id, verified deterministically.
package domain

import "time"

// StatementLineStatus is the reconciliation lifecycle for one ingested bank
// statement line: UNMATCHED -> MATCHED, UNMATCHED -> EXCEPTION, and
// EXCEPTION -> MATCHED once the correct ledger entry is found. MATCHED is
// terminal — unlike purchase-request-svc's fork, an EXCEPTION here is not a
// dead end, it's a queue item that gets resolved.
type StatementLineStatus string

const (
	StatementLineStatusUnmatched StatementLineStatus = "UNMATCHED"
	StatementLineStatusMatched   StatementLineStatus = "MATCHED"
	StatementLineStatusException StatementLineStatus = "EXCEPTION"
)

// ValidStatementLineTransitions documents the state machine. Enforcement
// itself lives in the store's atomic conditional UPDATEs, not this map.
var ValidStatementLineTransitions = map[StatementLineStatus][]StatementLineStatus{
	StatementLineStatusUnmatched: {StatementLineStatusMatched, StatementLineStatusException},
	StatementLineStatusException: {StatementLineStatusMatched},
	StatementLineStatusMatched:   {},
}

// StatementLine is one ingested bank statement transaction awaiting
// reconciliation against general-ledger-svc's journal truth.
type StatementLine struct {
	StatementLineID string              `json:"statement_line_id"`
	TenantID        string              `json:"tenant_id"`
	LegalEntityID   string              `json:"legal_entity_id"`
	BankAccountID   string              `json:"bank_account_id"`
	StatementDate   time.Time           `json:"statement_date"`
	Amount          float64             `json:"amount"`
	CurrencyCode    string              `json:"currency_code"`
	BankReference   string              `json:"bank_reference"`
	Status          StatementLineStatus `json:"status"`

	// GLCashAccountCode is the general-ledger account code representing this
	// bank account — the piece of information that makes the DIRECTION of a
	// match verifiable (a debit to it is money in, a credit is money out).
	// Nil only for rows ingested before migration 000003; such a line cannot
	// be matched at all rather than being matched on magnitude alone.
	GLCashAccountCode *string `json:"gl_cash_account_code,omitempty"`

	MatchedJournalID     *string    `json:"matched_journal_id,omitempty"`
	MatchedByPrincipalID *string    `json:"matched_by_principal_id,omitempty"`
	MatchedAt            *time.Time `json:"matched_at,omitempty"`

	ExceptionReason      *string    `json:"exception_reason,omitempty"`
	FlaggedByPrincipalID *string    `json:"flagged_by_principal_id,omitempty"`
	FlaggedAt            *time.Time `json:"flagged_at,omitempty"`

	CorrelationID string    `json:"correlation_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// ── wire types ───────────────────────────────────────────────────────────────

// CreateStatementLineRequest is the input for ingesting a bank statement line.
//
// TenantID is accepted but is NOT what the row is written under — the
// verified X-Tenant-Id header is. It is kept so a caller that sends a tenant
// disagreeing with its own verified scope is refused outright (403) rather
// than silently having the header substituted, which would hide a real bug in
// the caller. See handler.CreateStatementLine.
type CreateStatementLineRequest struct {
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	BankAccountID string    `json:"bank_account_id"`
	StatementDate time.Time `json:"statement_date"`
	Amount        float64   `json:"amount"`
	CurrencyCode  string    `json:"currency_code"`
	BankReference string    `json:"bank_reference"`
	// GLCashAccountCode is required: without it the direction of any future
	// match against this line is unverifiable. See migration 000003.
	GLCashAccountCode string `json:"gl_cash_account_code"`
	CorrelationID     string `json:"correlation_id"`
}

// MatchStatementLineRequest names the general-ledger-svc journal the caller
// believes this statement line corresponds to. The service verifies this
// independently — it never trusts the claim at face value.
type MatchStatementLineRequest struct {
	JournalID string `json:"journal_id"`
}

// FlagExceptionRequest requires a reason — an exception with no stated
// reason isn't a useful queue item for whoever investigates it later.
type FlagExceptionRequest struct {
	Reason string `json:"reason"`
}

// ListStatementLinesFilter contains filters for querying statement lines.
//
// TenantID is always the caller's VERIFIED scope, never a query parameter.
// It used to be read from ?tenant_id, which made the explicit tenant filter
// the store is careful about filter by a value the caller chose.
type ListStatementLinesFilter struct {
	TenantID      string
	BankAccountID string
	StatementDate string
	Status        string
	// Limit bounds the page. The register grows by one row per bank
	// transaction, so an unbounded read is a full statement history in one
	// response — and it was the default.
	Limit int
}

// Page bounds for ListStatementLines.
const (
	DefaultListLimit = 200
	MaxListLimit     = 1000
)

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrStatementLineNotFound = errorString("statement line not found")
	ErrInvalidTransition     = errorString("invalid statement line status transition")
	ErrStoreUnavailable      = errorString("bank reconciliation store unavailable")

	ErrAuthorizationDenied             = errorString("authorization denied for this bank reconciliation action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other Phase 3 service.
	ErrIdentityMissing = errorString("caller identity missing")

	// ErrLedgerVerificationFailed means general-ledger-svc was reached and
	// answered, but the referenced journal doesn't satisfy the match
	// (not found, not FINALIZED, wrong legal entity, or amount mismatch).
	ErrLedgerVerificationFailed = errorString("ledger verification failed: referenced journal does not satisfy the match")
	// ErrLedgerServiceUnavailable means general-ledger-svc could not be
	// reached or returned an unexpected status — fail closed, never treat
	// as a pass.
	ErrLedgerServiceUnavailable = errorString("general-ledger-svc unavailable")

	// ErrStatementIncomplete means at least one line for the given bank
	// account + statement date is still UNMATCHED.
	ErrStatementIncomplete = errorString("statement has unresolved (UNMATCHED) lines")

	// ErrTenantScopeMissing is returned when a request carries no verified
	// tenant scope. Distinguished from "not found" so it answers 401 rather
	// than 404: a caller with no scope at all was being told the row does not
	// exist, which is a different — and reassuring — thing to be told.
	ErrTenantScopeMissing = errorString("caller tenant scope missing")

	// ErrTenantScopeMismatch is returned when a request body names a tenant
	// other than the caller's verified scope.
	ErrTenantScopeMismatch = errorString("tenant_id does not match the caller's verified tenant scope")

	// ErrInvalidIdentifier is returned when an identifier or date is
	// malformed (SQLSTATE 22P02 / 22007 / 22001). Without this the driver
	// error surfaced as 503 store_unavailable — reporting a database outage
	// for what is a caller mistake, and sending the console's health display
	// the wrong signal.
	ErrInvalidIdentifier = errorString("malformed identifier or date")

	// ErrCashAccountUnknown is returned when a statement line has no
	// gl_cash_account_code, so the direction of a proposed match cannot be
	// verified. Refusing is deliberate: the alternative is the magnitude-only
	// comparison that migration 000003 exists to retire, which would accept a
	// payment out as reconciling a receipt in.
	ErrCashAccountUnknown = errorString("statement line has no gl_cash_account_code, so the direction of a match cannot be verified")

	// ErrLegalEntityMismatch is returned when the legal entity a caller was
	// authorized against is not the one the statement lines actually belong
	// to. Without this check the authorization and the resource were
	// independent: a caller could be authorized against an entity it holds
	// rights over and then act on a bank account belonging to another.
	ErrLegalEntityMismatch = errorString("bank account does not belong to the legal entity the caller was authorized against")
)
