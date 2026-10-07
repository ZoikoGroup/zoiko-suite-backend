package registry

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrUnownedCriticalAsset is returned when a critical asset (Tier 1) has no active accountable owner (GOV-01, DG-001, NP-01).
	ErrUnownedCriticalAsset = errors.New("critical data asset has no active accountable owner (blocked per NP-01/DG-001)")

	// ErrUnstewardedCriticalAsset is returned when a critical asset has no operational steward (DG-002).
	ErrUnstewardedCriticalAsset = errors.New("critical data asset has no operational steward (blocked per DG-002)")

	// ErrConflictingAuthoritativeOwnership is returned when multiple services attempt to claim authoritative writes (GOV-01, DG-004, NP-02).
	ErrConflictingAuthoritativeOwnership = errors.New("conflicting authoritative write ownership claimed by multiple services (blocked per NP-02/DG-004)")

	// ErrInvalidGovernanceScope is returned when scope metadata is missing required tenant or boundary context (GOV-05, DG-009).
	ErrInvalidGovernanceScope = errors.New("governance scope missing mandatory tenant or business process (blocked per DG-009)")

	// ErrMissingClassification is returned when a data asset lacks required security or privacy classification binding (DG-007).
	ErrMissingClassification = errors.New("data asset lacks mandatory classification binding (blocked per DG-007)")

	// ErrNoActiveAssignment is returned when no effective assignment exists as of the requested timestamp.
	ErrNoActiveAssignment = errors.New("no active assignment found for requested timestamp")

	// ErrConflictingActiveAssignments is returned when more than one active assignment overlaps in the same interval.
	ErrConflictingActiveAssignments = errors.New("multiple overlapping active assignments found for effective interval")
)

// InvariantValidator provides methods to enforce governance invariants across registry entities.
type InvariantValidator struct{}

func NewInvariantValidator() *InvariantValidator {
	return &InvariantValidator{}
}

// ValidateAssetRegistration verifies all constitution rules prior to accepting an asset into production.
func (v *InvariantValidator) ValidateAssetRegistration(
	asset *DataAsset,
	owners []DataOwnerAssignment,
	stewards []StewardshipAssignment,
	classification *DataClassificationBinding,
	asOf time.Time,
) error {
	if asset == nil {
		return errors.New("data asset is nil")
	}
	if asset.TenantID.IsNil() {
		return errors.New("asset tenant_id is required")
	}
	if asset.DomainID.IsNil() {
		return errors.New("asset domain_id is required")
	}
	if asset.AssetCode == "" {
		return errors.New("asset_code cannot be empty")
	}

	// DG-009: Governance scope check
	if asset.Scope.BusinessProcess == "" {
		return ErrInvalidGovernanceScope
	}

	// DG-007: Classification binding mandatory
	if classification == nil || classification.SensitivityLevel == "" {
		return ErrMissingClassification
	}

	// NP-01, DG-001: Critical asset must have an active accountable owner
	activeOwner, err := ResolveActiveOwner(owners, asOf)
	if asset.CriticalityTier == CriticalityTier1Critical {
		if err != nil || activeOwner == nil || activeOwner.OwnerPrincipalID == "" {
			return ErrUnownedCriticalAsset
		}
	}

	// DG-002: Critical asset must have an operational steward
	activeSteward, err := ResolveActiveSteward(stewards, asOf)
	if asset.CriticalityTier == CriticalityTier1Critical {
		if err != nil || activeSteward == nil || activeSteward.StewardPrincipalID == "" {
			return ErrUnstewardedCriticalAsset
		}
	}

	return nil
}

// AssertSingleAuthoritativeWriter verifies that only one registered service claims authoritative write rights (NP-02, DG-004).
func (v *InvariantValidator) AssertSingleAuthoritativeWriter(
	assetCode string,
	existingAuthoritativeService string,
	claimingService string,
) error {
	if existingAuthoritativeService != "" && claimingService != "" && existingAuthoritativeService != claimingService {
		return fmt.Errorf("%w: asset %q is already authoritatively owned by %q; rejected claiming service %q",
			ErrConflictingAuthoritativeOwnership, assetCode, existingAuthoritativeService, claimingService)
	}
	return nil
}

// ResolveActiveOwner finds the single active owner assignment as of a given timestamp.
func ResolveActiveOwner(assignments []DataOwnerAssignment, asOf time.Time) (*DataOwnerAssignment, error) {
	var active *DataOwnerAssignment
	for i := range assignments {
		a := &assignments[i]
		if isEffective(a.EffectiveFrom, a.EffectiveTo, asOf) {
			if active != nil {
				return nil, ErrConflictingActiveAssignments
			}
			active = a
		}
	}
	if active == nil {
		return nil, ErrNoActiveAssignment
	}
	return active, nil
}

// ResolveActiveSteward finds the single active steward assignment as of a given timestamp.
func ResolveActiveSteward(assignments []StewardshipAssignment, asOf time.Time) (*StewardshipAssignment, error) {
	var active *StewardshipAssignment
	for i := range assignments {
		a := &assignments[i]
		if isEffective(a.EffectiveFrom, a.EffectiveTo, asOf) {
			if active != nil {
				return nil, ErrConflictingActiveAssignments
			}
			active = a
		}
	}
	if active == nil {
		return nil, ErrNoActiveAssignment
	}
	return active, nil
}

// ResolveActiveCustodian finds the active custodian assignment as of a given timestamp.
func ResolveActiveCustodian(assignments []CustodianAssignment, asOf time.Time) (*CustodianAssignment, error) {
	var active *CustodianAssignment
	for i := range assignments {
		a := &assignments[i]
		if isEffective(a.EffectiveFrom, a.EffectiveTo, asOf) {
			if active != nil {
				return nil, ErrConflictingActiveAssignments
			}
			active = a
		}
	}
	if active == nil {
		return nil, ErrNoActiveAssignment
	}
	return active, nil
}

func isEffective(from time.Time, to *time.Time, asOf time.Time) bool {
	if asOf.Before(from) {
		return false
	}
	if to != nil && !asOf.Before(*to) {
		return false
	}
	return true
}
