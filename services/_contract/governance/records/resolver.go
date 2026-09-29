package records

import (
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

var (
	// ErrRecordImmutable is returned when attempting to edit a declared record in-place (GOV-12, DG-032, NP-11).
	ErrRecordImmutable = errors.New("declared record is strictly immutable; in-place edit prohibited; must use supersession or corrective record (blocked per NP-11/DG-032)")

	// ErrRetentionCalculationImmutable is returned when attempting retroactive recalculation without authorized rule (DG-033, NP-12).
	ErrRetentionCalculationImmutable = errors.New("prior records retain version-pinned retention calculation; retroactive update prohibited absent recalculation rule (per NP-12/DG-033)")
)

// Resolver provides record declaration, retention calculation, and immutability enforcement.
type Resolver struct{}

func NewResolver() *Resolver {
	return &Resolver{}
}

// DeclareRecord marks a business object as a governed immutable record (DG-031).
func (r *Resolver) DeclareRecord(
	tenantID types.UUID,
	recordClassID types.UUID,
	objectType string,
	objectID types.UUID,
	trigger DeclarationTrigger,
	declaredBy string,
	declaredAt time.Time,
) (*RecordDeclaration, error) {
	if objectType == "" || objectID.IsNil() {
		return nil, errors.New("business object type and ID are mandatory for record declaration")
	}

	declarationID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	return &RecordDeclaration{
		DeclarationID:      declarationID,
		TenantID:           tenantID,
		RecordClassID:      recordClassID,
		BusinessObjectType: objectType,
		BusinessObjectID:   objectID,
		DeclarationTrigger: trigger,
		DeclaredAt:         declaredAt,
		DeclaredBy:         declaredBy,
		IsImmutable:        true,
	}, nil
}

// AssertRecordImmutability verifies that a declared record is never edited in place (NP-11).
func (r *Resolver) AssertRecordImmutability(declaration *RecordDeclaration, attemptEditInPlace bool) error {
	if declaration != nil && declaration.IsImmutable && attemptEditInPlace {
		return fmt.Errorf("%w: target record %s (%s)", ErrRecordImmutable, declaration.BusinessObjectID, declaration.BusinessObjectType)
	}
	return nil
}

// CalculateRetentionTrigger calculates the retention eligibility date and pins the exact schedule version (DG-033, DG-034).
func (r *Resolver) CalculateRetentionTrigger(
	declaration *RecordDeclaration,
	schedule *RetentionScheduleVersion,
	triggerDate time.Time,
	calculatedAt time.Time,
) (*RetentionTrigger, error) {
	if declaration == nil || schedule == nil {
		return nil, errors.New("declaration and schedule cannot be nil")
	}

	triggerID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	eligibleDate := triggerDate.AddDate(0, 0, schedule.MinRetentionDays)

	return &RetentionTrigger{
		TriggerID:               triggerID,
		TenantID:                declaration.TenantID,
		DeclarationID:           declaration.DeclarationID,
		ScheduleID:              schedule.ScheduleID,
		ScheduleVersion:         schedule.Version,
		TriggerDate:             triggerDate,
		EligibleDispositionDate: eligibleDate,
		Status:                  TriggerStatusPending,
		CalculatedAt:            calculatedAt,
	}, nil
}

// EvaluateEligibility checks if the retention period has elapsed as of asOf, transitioning status without deleting (DG-035).
func (r *Resolver) EvaluateEligibility(trigger *RetentionTrigger, asOf time.Time) TriggerStatus {
	if trigger.Status == TriggerStatusDispositionHeld || trigger.Status == TriggerStatusDisposed {
		return trigger.Status
	}

	if !asOf.Before(trigger.EligibleDispositionDate) {
		trigger.Status = TriggerStatusEligibilityReached
	} else {
		trigger.Status = TriggerStatusPending
	}
	return trigger.Status
}

// UpdateScheduleVersion increments the retention schedule version for prospective declarations (NP-12).
func (r *Resolver) UpdateScheduleVersion(
	current *RetentionScheduleVersion,
	newMinDays int,
	effectiveFrom time.Time,
) *RetentionScheduleVersion {
	newSched := *current
	newSched.Version = current.Version + 1
	newSched.MinRetentionDays = newMinDays
	newSched.EffectiveFrom = effectiveFrom
	newSched.EffectiveTo = nil
	return &newSched
}
