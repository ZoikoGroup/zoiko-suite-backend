package domain

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// AuthoritativeRefKind names the owning domain's record an EXECUTED
// suggestion points at (A5). ZS-CONTROL-001 §21/§24: remediation references
// the owning domain's command — "journal/reversal/adjustment/credit/debit note
// IDs" — and the control never embeds the correction as free text.
type AuthoritativeRefKind string

// AuthoritativeRefGLJournal is a general-ledger-svc journal. A write-off or a
// timing adjustment of a bank reconciling item is a posting to the entity's
// own books, and general-ledger-svc is the only service that posts them. It
// returns one identifier for every journal type it posts — ADJUSTMENT,
// ACCRUAL, REVERSAL, STANDARD — its UUID journal_id. No service that owns a
// bank line issues an "adjustment_id": bank-reconciliation-svc has no
// write-off or adjustment command, and consolidation-svc's adjustments are
// consolidation-only and never touch an entity's books.
const AuthoritativeRefGLJournal AuthoritativeRefKind = "GL_JOURNAL"

// AuthoritativeRef is the structured reference recorded when a suggestion is
// marked EXECUTED: the record in the owning domain that carried the action
// out. The caller asserts it. It is checked for shape, not looked up in
// general-ledger-svc (B6).
type AuthoritativeRef struct {
	Kind      AuthoritativeRefKind `json:"kind"`
	JournalID string               `json:"journal_id"`
}

var (
	ErrInvalidAuthoritativeRef  = errors.New("invalid authoritative_ref")
	ErrAuthoritativeRefRequired = errors.New("authoritative_ref is required")
)

// ValidateAuthoritativeRef checks the reference a caller sent with a decision,
// before anything is loaded: it may accompany only EXECUTED, and it must name
// a general-ledger-svc journal by its UUID. The journal id is canonicalized
// in place, so one journal is always stored under one spelling.
func ValidateAuthoritativeRef(to SuggestionStatus, ref *AuthoritativeRef) error {
	if ref == nil {
		return nil
	}
	if to != StatusExecuted {
		return fmt.Errorf("%w: it is recorded only when a suggestion is marked %s", ErrInvalidAuthoritativeRef, StatusExecuted)
	}
	if ref.Kind != AuthoritativeRefGLJournal {
		return fmt.Errorf("%w: kind must be %s, got %q", ErrInvalidAuthoritativeRef, AuthoritativeRefGLJournal, ref.Kind)
	}
	id, err := uuid.Parse(strings.TrimSpace(ref.JournalID))
	if err != nil {
		return fmt.Errorf("%w: journal_id must be a general-ledger-svc journal UUID", ErrInvalidAuthoritativeRef)
	}
	ref.JournalID = id.String()
	return nil
}

// ValidateExecutionRef checks a decision against the recommendation it is
// about, which the store already holds when it locks the suggestion; it
// needs no further read. WRITE_OFF and TIMING_ADJUSTMENT are accounting
// actions: marking one EXECUTED without the journal that did it is refused.
// MANUAL_REVIEW may end in a journal, so one is accepted but not required.
// CONFIRM_DUPLICATE and AUTO_MATCH are carried out in bank-reconciliation-svc,
// not the ledger, so a journal is not their reference and is refused.
func ValidateExecutionRef(rec ResolutionRecommendation, to SuggestionStatus, ref *AuthoritativeRef) error {
	if to != StatusExecuted {
		return nil
	}
	switch rec {
	case RecommendationWriteOff, RecommendationTimingAdjustment:
		if ref == nil {
			return fmt.Errorf("%w: %s is an accounting action; marking it %s needs the general-ledger-svc journal that posted it",
				ErrAuthoritativeRefRequired, rec, StatusExecuted)
		}
	case RecommendationManualReview:
	default:
		if ref != nil {
			return fmt.Errorf("%w: %s is carried out in bank-reconciliation-svc, so a ledger journal is not its reference",
				ErrInvalidAuthoritativeRef, rec)
		}
	}
	return nil
}
