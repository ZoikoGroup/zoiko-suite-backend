package legalhold

import (
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

var (
	// ErrHoldPreemptsDisposition is returned when attempting to destroy or purge records under active legal hold (GOV-16, DG-036, NP-13).
	ErrHoldPreemptsDisposition = errors.New("record is protected by active legal hold; destructive disposition prohibited (blocked per NP-13/DG-036)")

	// ErrUnauthorizedRelease is returned when hold release lacks authorized dual-approver or segregation of duties (DG-039, NP-16).
	ErrUnauthorizedRelease = errors.New("unauthorized hold release; custodian cannot unilaterally release hold absent distinct legal approval (blocked per NP-16/DG-039)")

	// ErrHeldDataPurposeProhibited is returned when held data is accessed for unrelated secondary purposes like AI training (GOV-17, DG-053, NP-19).
	ErrHeldDataPurposeProhibited = errors.New("held data cannot be accessed for unrelated secondary purpose like AI training; preservation is not authorization (blocked per NP-19/DG-053)")
)

// Engine provides rule evaluation for legal hold coverage, prospective capture, and release governance.
type Engine struct{}

func NewEngine() *Engine {
	return &Engine{}
}

// MatchesScope checks whether a candidate record falls within the defined legal hold scope (DG-037, NP-15).
func (e *Engine) MatchesScope(scope *LegalHoldScope, record RecordCandidate) bool {
	if scope == nil || scope.TenantID != record.TenantID {
		return false
	}

	// Entity type match
	entityMatched := false
	for _, t := range scope.TargetEntityTypes {
		if t == record.EntityType {
			entityMatched = true
			break
		}
	}
	if !entityMatched {
		return false
	}

	// Custodian match (if scoped to specific custodians)
	if len(scope.CustodianPrincipalIDs) > 0 {
		custodianMatched := false
		for _, c := range scope.CustodianPrincipalIDs {
			if c == record.CustodianID {
				custodianMatched = true
				break
			}
		}
		if !custodianMatched {
			return false
		}
	}

	// Date range match
	if scope.DateRangeStart != nil && record.RecordDate.Before(*scope.DateRangeStart) {
		return false
	}
	// For non-prospective holds, date cannot exceed DateRangeEnd
	if !scope.IsProspective && scope.DateRangeEnd != nil && record.RecordDate.After(*scope.DateRangeEnd) {
		return false
	}

	return true
}

// AssertDispositionPermitted ensures that no active legal hold covers the candidate record before any disposition action (NP-13).
func (e *Engine) AssertDispositionPermitted(
	record RecordCandidate,
	activeHolds []LegalHold,
	activeScopes []LegalHoldScope,
) error {
	for _, hold := range activeHolds {
		if hold.Status != HoldStatusActive || hold.TenantID != record.TenantID {
			continue
		}
		for _, scope := range activeScopes {
			if scope.HoldID == hold.HoldID && e.MatchesScope(&scope, record) {
				return fmt.Errorf("%w: record %s (%s) matches active hold %s (%s)",
					ErrHoldPreemptsDisposition, record.RecordID, record.EntityType, hold.HoldMatterCode, hold.HoldID)
			}
		}
	}
	return nil
}

// AmendScope creates a new scope version when the hold scope is expanded or amended (NP-14, DG-038).
func (e *Engine) AmendScope(
	current *LegalHoldScope,
	newEntityTypes []string,
	newCustodians []string,
	isProspective bool,
	createdBy string,
	createdAt time.Time,
) (*LegalHoldScope, error) {
	if current == nil {
		return nil, errors.New("current scope cannot be nil")
	}

	scopeID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	return &LegalHoldScope{
		ScopeID:               scopeID,
		TenantID:              current.TenantID,
		HoldID:                current.HoldID,
		Version:               current.Version + 1,
		TargetEntityTypes:     newEntityTypes,
		CustodianPrincipalIDs: newCustodians,
		DateRangeStart:        current.DateRangeStart,
		DateRangeEnd:          current.DateRangeEnd,
		IsProspective:         isProspective,
		FilterPredicate:       current.FilterPredicate,
		CreatedAt:             createdAt,
		CreatedBy:             createdBy,
	}, nil
}

// ReleaseHold releases a legal hold ensuring strict segregation of duties (NP-16, DG-039).
func (e *Engine) ReleaseHold(
	hold *LegalHold,
	releasedBy string,
	approvedBy string,
	justification string,
	releasedAt time.Time,
) error {
	if hold == nil {
		return errors.New("hold cannot be nil")
	}
	if hold.Status == HoldStatusReleased {
		return errors.New("hold is already released")
	}
	if releasedBy == "" || approvedBy == "" {
		return fmt.Errorf("%w: both releaser and distinct legal approver are required", ErrUnauthorizedRelease)
	}
	// Custodian cannot be the sole approver of their own release (SoD)
	if releasedBy == approvedBy {
		return fmt.Errorf("%w: releaser (%q) and release approver cannot be the same principal", ErrUnauthorizedRelease, releasedBy)
	}

	hold.Status = HoldStatusReleased
	hold.ReleasedBy = &releasedBy
	hold.ReleaseApprovedBy = &approvedBy
	hold.ReleaseJustification = &justification
	hold.ReleasedAt = &releasedAt

	return nil
}

// AssertPurposePermitted validates that held data is never used for secondary unauthorized purposes (e.g. AI training) (NP-19, DG-053).
func (e *Engine) AssertPurposePermitted(
	record RecordCandidate,
	activeHolds []LegalHold,
	activeScopes []LegalHoldScope,
	requestedPurpose string,
) error {
	isHeld := false
	for _, hold := range activeHolds {
		if hold.Status == HoldStatusActive && hold.TenantID == record.TenantID {
			for _, scope := range activeScopes {
				if scope.HoldID == hold.HoldID && e.MatchesScope(&scope, record) {
					isHeld = true
					break
				}
			}
		}
	}

	if isHeld {
		// Held data can only be used for legal preservation/discovery/audit
		if requestedPurpose == "AI_MODEL_TRAINING" || requestedPurpose == "MARKETING" || requestedPurpose == "PRODUCT_ANALYTICS" {
			return fmt.Errorf("%w: record %s is under legal hold; access for purpose %q is denied",
				ErrHeldDataPurposeProhibited, record.RecordID, requestedPurpose)
		}
	}
	return nil
}
