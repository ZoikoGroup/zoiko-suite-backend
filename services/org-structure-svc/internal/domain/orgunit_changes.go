package domain

import "time"

// Structural changes that touch financial mappings need an independent
// approver (ZS-SVC-I-001 s4.5 segregation of duties). The maker proposes; a
// different principal approves, and only then is the change applied.

const ChangeCommandReparent = "REPARENT"

const (
	ChangePending  = "PENDING"
	ChangeApproved = "APPROVED"
	ChangeRejected = "REJECTED"
	// ChangeFailed: approval was attempted but the change could no longer be
	// applied (the unit moved on, a loop appeared, the parent was retired).
	ChangeFailed = "FAILED"
)

// ApprovalRulesNote is carried on every impact report: ORG-05 knows the
// cost/profit-center mappings of the units it moves, but approval rules belong
// to the workflow and policy services, so they are not evaluated here.
const ApprovalRulesNote = "approval rules are owned by the workflow and policy services and are not evaluated here; only cost/profit-center mappings trigger independent approval"

// MappedUnit is a unit in the moved subtree that carries a financial mapping.
type MappedUnit struct {
	UnitID           string `json:"unit_id"`
	Code             string `json:"code"`
	CostCenterCode   string `json:"cost_center_code,omitempty"`
	ProfitCenterCode string `json:"profit_center_code,omitempty"`
}

// ImpactReport says what a reparent reaches: the unit and everything beneath it
// on the effective date, and which of those carry a financial mapping.
type ImpactReport struct {
	UnitID           string       `json:"unit_id"`
	UnitsAffected    int          `json:"units_affected"` // the unit itself plus its descendants
	MappedUnits      []MappedUnit `json:"mapped_units"`
	RequiresApproval bool         `json:"requires_approval"`
	Notes            []string     `json:"notes"`
}

// NewImpactReport builds the report. A move needs independent approval exactly
// when some unit it carries has a financial mapping.
func NewImpactReport(unitID string, unitsAffected int, mapped []MappedUnit) ImpactReport {
	if mapped == nil {
		mapped = []MappedUnit{}
	}
	return ImpactReport{
		UnitID: unitID, UnitsAffected: unitsAffected, MappedUnits: mapped,
		RequiresApproval: len(mapped) > 0,
		Notes:            []string{ApprovalRulesNote},
	}
}

// ChangeRequest is a proposed structural change awaiting an independent decision.
type ChangeRequest struct {
	RequestID       string       `json:"request_id"`
	TenantID        string       `json:"tenant_id"`
	LegalEntityID   string       `json:"legal_entity_id"`
	UnitID          string       `json:"unit_id"`
	Command         string       `json:"command"`
	NewParentUnitID *string      `json:"new_parent_unit_id"`
	EffectiveAt     string       `json:"effective_at"`
	Reason          string       `json:"reason"`
	UnitVersion     int64        `json:"unit_version"` // the version the maker read
	Impact          ImpactReport `json:"impact"`
	Status          string       `json:"status"`
	RequestedBy     string       `json:"requested_by"`
	RequestedAt     time.Time    `json:"requested_at"`
	DecidedBy       *string      `json:"decided_by,omitempty"`
	DecidedAt       *time.Time   `json:"decided_at,omitempty"`
	DecisionReason  string       `json:"decision_reason,omitempty"`
	FailureCode     string       `json:"failure_code,omitempty"`
}

// DecideChangeRequest carries a reviewer's note.
type DecideChangeRequest struct {
	Reason string `json:"reason"`
}

var (
	ErrChangeRequestNotFound = errorString("change request not found")
	// ErrChangeRequestDecided: only a PENDING request can be approved or rejected.
	ErrChangeRequestDecided = errorString("change request is no longer pending")
	// ErrSelfApproval: the person who proposed a change cannot approve it.
	ErrSelfApproval = errorString("a change cannot be approved by the person who proposed it")
	// ErrChangePending: the unit already has a different change awaiting decision.
	ErrChangePending = errorString("org unit already has a different change awaiting approval")
)

// IsTerminalApplyError reports whether err means a change can never be applied
// as written, so a request that hits it should be closed as FAILED rather than
// left pending for ever.
func IsTerminalApplyError(err error) bool {
	switch err {
	case ErrVersionConflict, ErrHierarchyLoop, ErrParentInvalid, ErrParentNotActive,
		ErrEffectiveDateInvalid, ErrNoHierarchyChange, ErrInvalidTransition, ErrOrgUnitNotFound:
		return true
	}
	return false
}

// FailureCode is the short, stable name stored on a FAILED request.
func FailureCode(err error) string {
	switch err {
	case ErrVersionConflict:
		return "version_conflict"
	case ErrHierarchyLoop:
		return "hierarchy_loop"
	case ErrParentInvalid:
		return "parent_invalid"
	case ErrParentNotActive:
		return "parent_not_active"
	case ErrEffectiveDateInvalid:
		return "invalid_effective_date"
	case ErrNoHierarchyChange:
		return "no_change"
	case ErrInvalidTransition:
		return "invalid_transition"
	case ErrOrgUnitNotFound:
		return "org_unit_not_found"
	}
	return "apply_failed"
}
