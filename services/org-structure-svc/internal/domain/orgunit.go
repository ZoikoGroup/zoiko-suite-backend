package domain

import "time"

// ORG-05 Organizational Unit (ZS-SVC-I-001 section 4.5).

const (
	UnitDepartment   = "DEPARTMENT"
	UnitBusinessUnit = "BUSINESS_UNIT"
	UnitFunction     = "FUNCTION"
	UnitDivision     = "DIVISION"
	UnitTeam         = "TEAM"
)

const (
	UnitStatusDraft    = "DRAFT"
	UnitStatusActive   = "ACTIVE"
	UnitStatusInactive = "INACTIVE"
)

// IsValidUnitType reports whether t is one of the unit types the schema allows.
func IsValidUnitType(t string) bool {
	switch t {
	case UnitDepartment, UnitBusinessUnit, UnitFunction, UnitDivision, UnitTeam:
		return true
	}
	return false
}

// OrgUnit is the identity and lifecycle of a unit. Its parent, manager and
// cost/profit-center mapping are effective-dated and read from their own rows;
// the fields below the line are the values current on the read date.
type OrgUnit struct {
	UnitID        string    `json:"unit_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	UnitType      string    `json:"unit_type"`
	Status        string    `json:"status"` // DRAFT, ACTIVE, INACTIVE
	LocationRef   string    `json:"location_ref,omitempty"`
	RecordVersion int64     `json:"record_version"`
	CorrelationID string    `json:"correlation_id"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	ParentUnitID      *string `json:"parent_unit_id,omitempty"`
	ManagerEmployeeID *string `json:"manager_employee_id,omitempty"`
	CostCenterCode    string  `json:"cost_center_code,omitempty"`
	ProfitCenterCode  string  `json:"profit_center_code,omitempty"`
}

// HierarchyVersion is one effective-dated placement of a unit under a parent.
// ValidTo is exclusive; nil means open-ended.
type HierarchyVersion struct {
	VersionID    string     `json:"version_id"`
	UnitID       string     `json:"unit_id"`
	ParentUnitID *string    `json:"parent_unit_id,omitempty"`
	ValidFrom    string     `json:"valid_from"`
	ValidTo      *string    `json:"valid_to,omitempty"`
	RecordedAt   time.Time  `json:"recorded_at"`
	SupersededAt *time.Time `json:"superseded_at,omitempty"`
	ChangeReason string     `json:"change_reason,omitempty"`
	ApprovalRef  string     `json:"approval_ref,omitempty"`
	EvidenceRef  string     `json:"evidence_ref,omitempty"`
	ActorID      string     `json:"actor_id,omitempty"`
}

// CreateOrgUnitRequest creates a unit in DRAFT. EffectiveFrom is the date the
// unit takes its first place in the hierarchy.
type CreateOrgUnitRequest struct {
	LegalEntityID     string  `json:"legal_entity_id"`
	Code              string  `json:"code"`
	Name              string  `json:"name"`
	UnitType          string  `json:"unit_type"`
	ParentUnitID      *string `json:"parent_unit_id,omitempty"`
	ManagerEmployeeID *string `json:"manager_employee_id,omitempty"`
	LocationRef       string  `json:"location_ref,omitempty"`
	CostCenterCode    string  `json:"cost_center_code,omitempty"`
	ProfitCenterCode  string  `json:"profit_center_code,omitempty"`
	EffectiveFrom     string  `json:"effective_from"`
	Reason            string  `json:"reason,omitempty"`
	CorrelationID     string  `json:"correlation_id"`
}

var (
	ErrOrgUnitNotFound = errorString("org unit not found")
	ErrOrgUnitCodeTaken = errorString("org unit code already used in this legal entity and type")
	// ErrParentInvalid: the parent is missing, in another legal entity, or
	// not usable on the effective date.
	ErrParentInvalid = errorString("parent org unit is missing or not in the same legal entity")
)
