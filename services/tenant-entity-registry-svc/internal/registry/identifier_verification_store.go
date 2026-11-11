package registry

import (
	"context"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// IdentifierVerificationStore is the data-access contract for identifier verification (migration 000021). Asserted by
// the service like PartyStore: a store without it answers ErrServiceUnavailable.
type IdentifierVerificationStore interface {
	// VerifyPartyIdentifier runs ZS-SVC-U-001 s14.1 steps 1 to 7 for one observation, in one transaction under the
	// party's row lock: resolve the identifier, apply scheme rules, record the observation, apply precedence and
	// freshness (domain.DecideObservation), open a stewardship case when a higher authority disagrees with a verified
	// identifier, and publish only when the acceptance rules pass. A stale expected_master_version is
	// ErrVersionConflict.
	VerifyPartyIdentifier(ctx context.Context, identifierID string, req domain.VerifyIdentifierRequest, actor string, allowUnversioned bool) (*IdentifierVerifyOutcome, error)
	// ListIdentifierVerifications lists an identifier's observations, newest first; ErrNotFound when it is not the
	// tenant's.
	ListIdentifierVerifications(ctx context.Context, identifierID string) ([]domain.IdentifierObservation, error)
}

// IdentifierVerifyOutcome is what recording an observation produced.
type IdentifierVerifyOutcome struct {
	Identifier  domain.PartyIdentifier       `json:"identifier"`
	Observation domain.IdentifierObservation `json:"observation"`
	Party       domain.Party                 `json:"party"`
	// Case is set when a higher-authority disagreement opened a stewardship case.
	Case *domain.ResolutionCase `json:"case,omitempty"`
	// LEIAlignment compares a verified LEI with the legal entity profile's: ALIGNED, PROFILE_ABSENT, DIFFERS,
	// NOT_LINKED, or NOT_APPLICABLE for other schemes. ORG-03 owns the profile, so a difference is reported, not
	// written over.
	LEIAlignment string `json:"lei_alignment"`
}
