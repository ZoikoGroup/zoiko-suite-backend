package domain

import "time"

// Match/pair status values — ACC-11's own state model (verbatim from spec):
// "Open → AwaitingCounterparty → Matched / Mismatched / Disputed →
// Resolved/Closed." MatchStatusUnmatched is kept as the literal string
// "UNMATCHED" for backward compatibility with rows and callers written
// before this lifecycle existed — it IS the spec's "Open" state, just
// under its pre-existing name.
const (
	MatchStatusUnmatched           = "UNMATCHED" // spec's "Open"
	MatchStatusAwaitingCounterparty = "AWAITING_COUNTERPARTY"
	MatchStatusMatched             = "MATCHED"
	MatchStatusMismatch            = "MISMATCH"
	MatchStatusDisputed            = "DISPUTED"
	MatchStatusResolved            = "RESOLVED"
)

type IntercompanyEntry struct {
	IntercompanyEntryID string    `json:"intercompany_entry_id"`
	TenantID            string    `json:"tenant_id"`
	SourceLegalEntityID string    `json:"source_legal_entity_id"`
	TargetLegalEntityID string    `json:"target_legal_entity_id"`
	SourceJournalID     string    `json:"source_journal_id"`
	TargetJournalID     *string   `json:"target_journal_id,omitempty"`
	Amount              float64   `json:"amount"`
	CurrencyCode        string    `json:"currency_code"`
	MatchStatus         string    `json:"match_status"`
	MismatchReason      *string   `json:"mismatch_reason,omitempty"`

	// AcknowledgedAt/AcknowledgedByPrincipalID record ACC-11's own
	// "AwaitingCounterparty" milestone — the counterparty side confirming
	// it has seen the pair. Nil until AcknowledgeCounterparty is called.
	AcknowledgedAt            *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedByPrincipalID *string    `json:"acknowledged_by_principal_id,omitempty"`

	// DisputedAt/DisputedByPrincipalID/DisputeReason record DisputeIntercompany
	// — a human flagging a MISMATCH for investigation rather than accepting it.
	DisputedAt            *time.Time `json:"disputed_at,omitempty"`
	DisputedByPrincipalID *string    `json:"disputed_by_principal_id,omitempty"`
	DisputeReason         *string    `json:"dispute_reason,omitempty"`

	// ResolvedAt/ResolvedByPrincipalID/ResolutionNote record ResolveMismatch
	// — the spec's own terminal "Resolved" outcome for a disputed pair.
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
	ResolvedByPrincipalID *string    `json:"resolved_by_principal_id,omitempty"`
	ResolutionNote        *string    `json:"resolution_note,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateEntryRequest struct {
	SourceLegalEntityID string  `json:"source_legal_entity_id"`
	TargetLegalEntityID string  `json:"target_legal_entity_id"`
	SourceJournalID     string  `json:"source_journal_id"`
	Amount              float64 `json:"amount"`
	CurrencyCode        string  `json:"currency_code"`
}

type MatchEntryRequest struct {
	TargetJournalID string `json:"target_journal_id"`
}

type MatchEntryResponse struct {
	IntercompanyEntryID string  `json:"intercompany_entry_id"`
	MatchStatus         string  `json:"match_status"`
	MismatchReason      *string `json:"mismatch_reason,omitempty"`
}

// AcknowledgeCounterpartyRequest is ACC-11's own AcknowledgeCounterparty
// command input — no body fields are required; the caller identity and
// entry id carry everything needed.
type AcknowledgeCounterpartyRequest struct{}

// DisputeIntercompanyRequest is ACC-11's own DisputeIntercompany command
// input — the spec's own negative-path table presumes a real reason is
// always given for flagging a mismatch, the same posture as ACC-03's own
// RejectJournal reason requirement.
type DisputeIntercompanyRequest struct {
	Reason string `json:"reason"`
}

// ResolveMismatchRequest is ACC-11's own ResolveMismatch command input.
type ResolveMismatchRequest struct {
	ResolutionNote string `json:"resolution_note"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrEntryNotFound           = errorString("intercompany entry not found")
	ErrInvalidAmount           = errorString("intercompany entry amount must be greater than zero")
	ErrSameEntityForbidden     = errorString("source and target legal entities must be different")
	ErrEntryAlreadyMatched     = errorString("intercompany entry is already matched")
	ErrGLServiceUnavailable    = errorString("general-ledger-svc unavailable")
	ErrAmountMismatch          = errorString("journal amount or entity discrepancy detected")
	ErrAuthorizationDenied     = errorString("authorization denied for intercompany accounting action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("intercompany store unavailable")

	// ── ACC-11 lifecycle ─────────────────────────────────────────────────

	// ErrInvalidPairTransition covers every ACC-11 lifecycle command called
	// against a pair not in the one status it requires.
	ErrInvalidPairTransition = errorString("intercompany pair is not in a status that allows this action")

	// ErrDisputeReasonRequired is DisputeIntercompany's own guard, same
	// posture as ACC-03's RejectJournal reason requirement.
	ErrDisputeReasonRequired = errorString("reason is required to dispute an intercompany mismatch")

	// ErrResolutionNoteRequired is ResolveMismatch's own guard — a
	// resolution with no recorded rationale is not evidence of anything.
	ErrResolutionNoteRequired = errorString("resolution_note is required to resolve a disputed intercompany pair")

	// ErrCounterpartyJournalMissing is the spec's own negative path "One
	// side missing": MatchIntercompany named a target_journal_id that
	// general-ledger-svc has no record of at all. Resolved to MISMATCH,
	// not a 503 — a missing counterparty journal is a real accounting
	// fact worth recording, not a transient outage.
	ErrCounterpartyJournalMissing = errorString("counterparty journal does not exist in general-ledger-svc")

	// ErrEntityRegistryUnavailable is returned when tenant-entity-registry-svc
	// cannot be reached to check group relationship state.
	ErrEntityRegistryUnavailable = errorString("tenant-entity-registry-svc unavailable")

	// ErrGroupRelationshipLost is the spec's own negative path, "Entity
	// loses group relationship mid-period": the source and target legal
	// entities no longer share an open group relationship as of the match
	// attempt, per tenant-entity-registry-svc's own EntityHierarchy record.
	ErrGroupRelationshipLost = errorString("source and target legal entities no longer share an open group relationship")
)
