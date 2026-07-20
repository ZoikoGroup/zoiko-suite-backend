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
type CreateStatementLineRequest struct {
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	BankAccountID string    `json:"bank_account_id"`
	StatementDate time.Time `json:"statement_date"`
	Amount        float64   `json:"amount"`
	CurrencyCode  string    `json:"currency_code"`
	BankReference string    `json:"bank_reference"`
	CorrelationID string    `json:"correlation_id"`
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
type ListStatementLinesFilter struct {
	TenantID      string
	BankAccountID string
	StatementDate string
	Status        string
}

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
)
