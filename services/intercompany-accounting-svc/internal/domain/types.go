// Package domain defines the authoritative domain types for
// intercompany-accounting-svc.
//
// Per docs/architecture/03-microservices.md §10.6, this service owns
// governed intercompany entries, matching logic, and balancing integrity.
// Critical constraint: intercompany activity must never be collapsed into
// single-entity truth — every entry always keeps SourceLegalEntityID and
// TargetLegalEntityID as two distinct fields; nothing here ever nets them
// into one signed position.
//
// Unlike bank-reconciliation-svc (where a human explicitly chooses to flag
// an exception), a mismatch here is always a system-detected OUTCOME of an
// attempted match, never a separate human action: LinkTargetJournal is the
// only mutating action past creation, and whether the result is MATCHED or
// MISMATCHED is decided by cross-checking both journals against
// general-ledger-svc, not by which endpoint was called.
package domain

import "time"

type MatchStatus string

const (
	MatchStatusPending    MatchStatus = "PENDING"
	MatchStatusMatched    MatchStatus = "MATCHED"
	MatchStatusMismatched MatchStatus = "MISMATCHED"
)

// ValidMatchTransitions documents the state machine. Enforcement itself
// lives in the store's atomic conditional UPDATEs, not this map.
var ValidMatchTransitions = map[MatchStatus][]MatchStatus{
	MatchStatusPending:    {MatchStatusMatched, MatchStatusMismatched},
	MatchStatusMismatched: {MatchStatusMatched},
	MatchStatusMatched:    {},
}

// IntercompanyEntry is one governed transaction between two legal entities
// within the same tenant, awaiting (or having completed) balancing against
// general-ledger-svc's journal truth on both sides.
type IntercompanyEntry struct {
	IntercompanyEntryID string `json:"intercompany_entry_id"`
	TenantID            string `json:"tenant_id"`
	SourceLegalEntityID string `json:"source_legal_entity_id"`
	TargetLegalEntityID string `json:"target_legal_entity_id"`

	SourceJournalEntryID string  `json:"source_journal_entry_id"`
	TargetJournalEntryID *string `json:"target_journal_entry_id,omitempty"`

	Amount       float64     `json:"amount"`
	CurrencyCode string      `json:"currency_code"`
	Description  string      `json:"description"`
	MatchStatus  MatchStatus `json:"match_status"`

	MismatchReason *string `json:"mismatch_reason,omitempty"`

	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	MatchedByPrincipalID *string    `json:"matched_by_principal_id,omitempty"`
	CorrelationID        string     `json:"correlation_id"`
	CreatedAt            time.Time  `json:"created_at"`
	MatchedAt            *time.Time `json:"matched_at,omitempty"`
	MismatchedAt         *time.Time `json:"mismatched_at,omitempty"`
}

// ── wire types ───────────────────────────────────────────────────────────────

// CreateIntercompanyEntryRequest is the input for registering a new
// intercompany entry. Only the source side is known at creation time — the
// target side is supplied later via LinkTargetJournalRequest, once the
// counterparty entity has posted its mirroring journal.
type CreateIntercompanyEntryRequest struct {
	TenantID             string  `json:"tenant_id"`
	SourceLegalEntityID  string  `json:"source_legal_entity_id"`
	TargetLegalEntityID  string  `json:"target_legal_entity_id"`
	SourceJournalEntryID string  `json:"source_journal_entry_id"`
	Amount               float64 `json:"amount"`
	CurrencyCode         string  `json:"currency_code"`
	Description          string  `json:"description"`
	CorrelationID        string  `json:"correlation_id"`
}

// LinkTargetJournalRequest names the general-ledger-svc journal the caller
// believes mirrors this entry on the target entity's side. The service
// verifies this independently against both journals — it never trusts the
// claim at face value, and a failed verification is not an error, it's a
// MISMATCHED outcome.
type LinkTargetJournalRequest struct {
	TargetJournalEntryID string `json:"target_journal_entry_id"`
}

// ListEntriesFilter contains filters for querying intercompany entries.
type ListEntriesFilter struct {
	TenantID    string
	MatchStatus string
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrEntryNotFound     = errorString("intercompany entry not found")
	ErrInvalidTransition = errorString("invalid intercompany entry status transition")
	ErrStoreUnavailable  = errorString("intercompany accounting store unavailable")

	ErrAuthorizationDenied             = errorString("authorization denied for this intercompany action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as every other Phase 3 service.
	ErrIdentityMissing = errorString("caller identity missing")

	// ErrLedgerServiceUnavailable means general-ledger-svc could not be
	// reached or returned an unexpected status for either journal — fail
	// closed, never treat as a pass or as a mismatch.
	ErrLedgerServiceUnavailable = errorString("general-ledger-svc unavailable")
)
