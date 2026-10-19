package domain

import (
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"
)

// Match/pair status values — ACC-11's own state model (verbatim from spec):
// "Open → AwaitingCounterparty → Matched / Mismatched / Disputed →
// Resolved/Closed." MatchStatusUnmatched is kept as the literal string
// "UNMATCHED" for backward compatibility with rows and callers written
// before this lifecycle existed — it IS the spec's "Open" state, just
// under its pre-existing name.
const (
	MatchStatusUnmatched            = "UNMATCHED" // spec's "Open"
	MatchStatusAwaitingCounterparty = "AWAITING_COUNTERPARTY"
	MatchStatusMatched              = "MATCHED"
	MatchStatusMismatch             = "MISMATCH"
	MatchStatusDisputed             = "DISPUTED"
	MatchStatusResolved             = "RESOLVED"

	// MatchStatusClosed is the terminal state of a MATCHED pair, e.g. once
	// its period has been consolidated. Nothing leaves it.
	MatchStatusClosed = "CLOSED"
)

// Resolution outcomes: what ResolveMismatch decided about a disputed pair.
// A consumer deciding whether a RESOLVED pair may be eliminated reads this;
// a NULL outcome (pairs resolved before migration 000006) means unknown.
const (
	// ResolutionAccepted: the difference was accepted as it stands.
	ResolutionAccepted = "ACCEPTED"
	// ResolutionCorrected: one side was corrected so the pair now agrees.
	ResolutionCorrected = "CORRECTED"
	// ResolutionWrittenOff: the difference was written off.
	ResolutionWrittenOff = "WRITTEN_OFF"
)

// ValidResolutionOutcome reports whether o is one of the resolution outcomes.
func ValidResolutionOutcome(o string) bool {
	return o == ResolutionAccepted || o == ResolutionCorrected || o == ResolutionWrittenOff
}

// MatchableStatuses are the only statuses MatchIntercompany may move a pair
// out of. MISMATCH is included so a caller can correct a prior mismatch by
// re-running the match; MATCHED, DISPUTED, RESOLVED and CLOSED have left the
// part of the state model matching belongs to. The store enforces this in the UPDATE
// itself, so a dispute that lands between the handler's read and its write
// is never overwritten.
var MatchableStatuses = []string{MatchStatusUnmatched, MatchStatusAwaitingCounterparty, MatchStatusMismatch}

type IntercompanyEntry struct {
	IntercompanyEntryID string  `json:"intercompany_entry_id"`
	TenantID            string  `json:"tenant_id"`
	SourceLegalEntityID string  `json:"source_legal_entity_id"`
	TargetLegalEntityID string  `json:"target_legal_entity_id"`
	SourceJournalID     string  `json:"source_journal_id"`
	TargetJournalID     *string `json:"target_journal_id,omitempty"`

	// Amount/CurrencyCode are the originating (source) entity's side of the
	// pair. CounterpartyAmount/CounterpartyCurrencyCode are what the
	// counterparty (target) entity is expected to book, and what
	// MatchIntercompany compares the target journal against. They differ
	// only for a cross-currency pair; for a same-currency pair they are
	// equal (CreateIntercompanyPair refuses anything else).
	Amount                   Decimal `json:"amount"`
	CurrencyCode             string  `json:"currency_code"`
	CounterpartyAmount       Decimal `json:"counterparty_amount"`
	CounterpartyCurrencyCode string  `json:"counterparty_currency_code"`

	// FiscalPeriod is the period the pair belongs to, in general-ledger-svc's
	// own fiscal_period form (e.g. "2026-07"). Nil for pairs created before
	// migration 000005, and for callers that do not send one yet.
	FiscalPeriod *string `json:"fiscal_period,omitempty"`

	// Dimensions are the pair's reporting dimensions (cost centre, project,
	// ...), the same flat string map general-ledger-svc carries on journal
	// lines. Never nil once read from the store.
	Dimensions Dimensions `json:"dimensions"`

	MatchStatus    string  `json:"match_status"`
	MismatchReason *string `json:"mismatch_reason,omitempty"`

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

	// ResolutionOutcome is ResolveMismatch's decision (ResolutionAccepted,
	// ResolutionCorrected or ResolutionWrittenOff). Nil on a pair that is
	// not RESOLVED, and on pairs resolved before it was recorded.
	ResolutionOutcome *string `json:"resolution_outcome,omitempty"`

	// ClosedAt/ClosedByPrincipalID/CloseReason record ClosePair, the
	// terminal close of a MATCHED pair.
	ClosedAt            *time.Time `json:"closed_at,omitempty"`
	ClosedByPrincipalID *string    `json:"closed_by_principal_id,omitempty"`
	CloseReason         *string    `json:"close_reason,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateEntryRequest struct {
	SourceLegalEntityID string  `json:"source_legal_entity_id"`
	TargetLegalEntityID string  `json:"target_legal_entity_id"`
	SourceJournalID     string  `json:"source_journal_id"`
	Amount              Decimal `json:"amount"`
	CurrencyCode        string  `json:"currency_code"`

	// CounterpartyCurrencyCode defaults to CurrencyCode. CounterpartyAmount
	// defaults to Amount only when the two currencies are the same: a
	// cross-currency pair must state the counterparty amount, because this
	// service has no FX rate to derive it from and will not guess one.
	CounterpartyAmount       *Decimal   `json:"counterparty_amount,omitempty"`
	CounterpartyCurrencyCode string     `json:"counterparty_currency_code,omitempty"`
	FiscalPeriod             string     `json:"fiscal_period,omitempty"`
	Dimensions               Dimensions `json:"dimensions,omitempty"`
}

// Dimensions is a flat map of reporting dimension name to value.
type Dimensions map[string]string

const (
	maxDimensions        = 32
	maxDimensionKeyLen   = 64
	maxDimensionValueLen = 256
	maxFiscalPeriodLen   = 20
)

// Validate bounds the map so a pair cannot carry an unbounded blob.
func (d Dimensions) Validate() error {
	if len(d) > maxDimensions {
		return fmt.Errorf("%w: at most %d dimensions", ErrInvalidDimensions, maxDimensions)
	}
	for k, v := range d {
		if !printable(k, maxDimensionKeyLen) || k == "" {
			return fmt.Errorf("%w: dimension name %q must be 1-%d printable characters", ErrInvalidDimensions, k, maxDimensionKeyLen)
		}
		if !printable(v, maxDimensionValueLen) {
			return fmt.Errorf("%w: value of %q must be at most %d printable characters", ErrInvalidDimensions, k, maxDimensionValueLen)
		}
	}
	return nil
}

// ValidFiscalPeriod applies general-ledger-svc's own rule for a
// fiscal_period reference: 1-20 characters, no control characters. There is
// no fiscal calendar service to check it against.
func ValidFiscalPeriod(p string) bool {
	return p != "" && printable(p, maxFiscalPeriodLen)
}

func printable(s string, maxLen int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxLen {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type MatchEntryRequest struct {
	TargetJournalID string `json:"target_journal_id"`
}

type MatchEntryResponse struct {
	IntercompanyEntryID string  `json:"intercompany_entry_id"`
	MatchStatus         string  `json:"match_status"`
	MismatchReason      *string `json:"mismatch_reason,omitempty"`
	// FailedRule is the MatchRule* code of a MISMATCH.
	FailedRule *string `json:"failed_rule,omitempty"`
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
// Outcome is required: a resolution that does not say whether the mismatch
// was accepted, corrected or written off cannot tell consolidation whether
// the pair may be eliminated.
type ResolveMismatchRequest struct {
	ResolutionNote string `json:"resolution_note"`
	Outcome        string `json:"outcome"`
}

// ClosePairRequest is the input to ClosePair, the terminal close of a
// MATCHED pair.
type ClosePairRequest struct {
	Reason string `json:"reason"`
}

// Match rules: which check a MatchIntercompany attempt failed, in the order
// the match applies them.
const (
	MatchRuleCounterpartyJournalMissing = "COUNTERPARTY_JOURNAL_MISSING"
	MatchRuleTargetEntityMismatch       = "TARGET_ENTITY_MISMATCH"
	MatchRuleTargetJournalNotFinalized  = "TARGET_JOURNAL_NOT_FINALIZED"
	MatchRuleCurrencyUnverifiable       = "CURRENCY_UNVERIFIABLE"
	MatchRuleCurrencyMismatch           = "CURRENCY_MISMATCH"
	MatchRuleAmountMismatch             = "AMOUNT_MISMATCH"
	MatchRuleGroupRelationshipLost      = "GROUP_RELATIONSHIP_LOST"

	// MatchRuleLegacyUnstructured marks an attempt backfilled by migration
	// 000007 from a pre-existing mismatch_reason sentence, which never said
	// which rule failed.
	MatchRuleLegacyUnstructured = "LEGACY_UNSTRUCTURED"
)

// MatchAttempt is one applied MatchIntercompany attempt, the structured
// ExplainMismatch evidence (migration 000007). Append-only: a later attempt
// never overwrites an earlier one.
type MatchAttempt struct {
	AttemptID           int64  `json:"attempt_id"`
	IntercompanyEntryID string `json:"intercompany_entry_id"`

	// Outcome is MatchStatusMatched or MatchStatusMismatch. FailedRule and
	// Message are set exactly when it is a mismatch; Message is the sentence
	// the pair's mismatch_reason reports.
	Outcome    string  `json:"outcome"`
	FailedRule *string `json:"failed_rule,omitempty"`
	Message    *string `json:"message,omitempty"`

	Source MatchAttemptSide `json:"source"`
	Target MatchAttemptSide `json:"target"`

	ExaminedJournalIDs []string `json:"examined_journal_ids"`
	Tolerance          Decimal  `json:"tolerance"`

	// FiscalPeriod is the pair's period at the time of the attempt.
	FiscalPeriod  *string   `json:"fiscal_period,omitempty"`
	AttemptedBy   string    `json:"attempted_by"`
	CorrelationID *string   `json:"correlation_id,omitempty"`
	AttemptedAt   time.Time `json:"attempted_at"`
}

// MatchAttemptSide is what one side of the pair expected and what
// general-ledger-svc reported for the journal examined on that side. The
// Observed fields are nil when the side was not examined (the source side
// never is yet, SPEC_DEVIATIONS A6) or the journal was not found.
type MatchAttemptSide struct {
	JournalID             *string  `json:"journal_id,omitempty"`
	ExpectedAmount        Decimal  `json:"expected_amount"`
	ExpectedCurrency      string   `json:"expected_currency"`
	ObservedAmount        *Decimal `json:"observed_amount,omitempty"`
	ObservedCurrency      *string  `json:"observed_currency,omitempty"`
	ObservedLegalEntityID *string  `json:"observed_legal_entity_id,omitempty"`
	ObservedStatus        *string  `json:"observed_status,omitempty"`
	ObservedFiscalPeriod  *string  `json:"observed_fiscal_period,omitempty"`
}

// MismatchExplanation is ExplainMismatch's answer: the pair's current
// status and reason, and every match attempt, oldest first.
type MismatchExplanation struct {
	IntercompanyEntryID string         `json:"intercompany_entry_id"`
	MatchStatus         string         `json:"match_status"`
	MismatchReason      *string        `json:"mismatch_reason,omitempty"`
	Attempts            []MatchAttempt `json:"attempts"`
}

// PairTransition is one row of a pair's append-only state history.
// FromState is nil on the first row: the pair's creation, or (actor
// "migration:000006") the state it was already in when history began.
type PairTransition struct {
	TransitionID        int64     `json:"transition_id"`
	IntercompanyEntryID string    `json:"intercompany_entry_id"`
	FromState           *string   `json:"from_state"`
	ToState             string    `json:"to_state"`
	FiscalPeriod        *string   `json:"fiscal_period,omitempty"`
	Reason              *string   `json:"reason,omitempty"`
	ResolutionOutcome   *string   `json:"resolution_outcome,omitempty"`
	Actor               string    `json:"actor"`
	CorrelationID       *string   `json:"correlation_id,omitempty"`
	OccurredAt          time.Time `json:"occurred_at"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrEntryNotFound       = errorString("intercompany entry not found")
	ErrInvalidAmount       = errorString("intercompany entry amount must be greater than zero")
	ErrAmountPrecision     = errorString("intercompany amounts must have at most 4 decimal places and 14 integer digits")
	ErrInvalidCurrency     = errorString("currency codes must be three-letter ISO 4217 codes naming a real currency")
	ErrInvalidFiscalPeriod = errorString("fiscal_period must be 1-20 characters with no control characters")
	ErrInvalidDimensions   = errorString("invalid dimensions")

	// ErrCounterpartyAmountRequired: a cross-currency pair must state the
	// counterparty amount; this service has no FX rate to derive it from.
	ErrCounterpartyAmountRequired = errorString("counterparty_amount is required when counterparty_currency_code differs from currency_code")

	// ErrCounterpartyAmountDiffers: in a same-currency pair both sides book
	// the same amount. Accepting a different expected counterparty amount
	// would let a caller decide what the target journal has to match.
	ErrCounterpartyAmountDiffers = errorString("counterparty_amount must equal amount when both sides are in the same currency")
	ErrSameEntityForbidden       = errorString("source and target legal entities must be different")
	ErrEntryAlreadyMatched       = errorString("intercompany entry is already matched")
	ErrGLServiceUnavailable      = errorString("general-ledger-svc unavailable")
	ErrAmountMismatch            = errorString("journal amount or entity discrepancy detected")
	ErrAuthorizationDenied       = errorString("authorization denied for intercompany accounting action")
	ErrAuthzServiceUnavailable   = errorString("authorization-svc unavailable")
	ErrIdentityMissing           = errorString("caller identity missing")
	ErrStoreUnavailable          = errorString("intercompany store unavailable")

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

	// ErrResolutionOutcomeInvalid is ResolveMismatch's outcome guard.
	ErrResolutionOutcomeInvalid = errorString("outcome is required to resolve a disputed intercompany pair: ACCEPTED, CORRECTED or WRITTEN_OFF")

	// ErrCloseReasonRequired is ClosePair's guard: a terminal close with no
	// recorded reason is not evidence of anything.
	ErrCloseReasonRequired = errorString("reason is required to close an intercompany pair")

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
