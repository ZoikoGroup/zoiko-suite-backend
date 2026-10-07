package records

import (
	"time"

	"zoiko.io/contract/types"
)

// DeclarationTrigger specifies the business lifecycle transition that triggered record creation (§11, DG-031).
type DeclarationTrigger string

const (
	TriggerPosted    DeclarationTrigger = "POSTED"    // Accounting journal or subledger posted
	TriggerFiled     DeclarationTrigger = "FILED"     // Statutory tax return or regulatory report filed
	TriggerClosed    DeclarationTrigger = "CLOSED"    // Financial period or fiscal year closed
	TriggerExecuted  DeclarationTrigger = "EXECUTED"  // Legal contract signed or payroll run completed
)

// TriggerStatus tracks the retention eligibility lifecycle of a declared record (§15, DG-035).
type TriggerStatus string

const (
	TriggerStatusPending            TriggerStatus = "PENDING"
	TriggerStatusEligibilityReached TriggerStatus = "ELIGIBILITY_REACHED" // Date reached; requires review/batching (DG-035)
	TriggerStatusDispositionHeld    TriggerStatus = "DISPOSITION_HELD"    // Held by active legal hold
	TriggerStatusDisposed           TriggerStatus = "DISPOSED"            // Fully destroyed via certificate
)

// RecordClass models a canonical category of managed record (§11, §28).
type RecordClass struct {
	RecordClassID         types.UUID `json:"record_class_id"`
	TenantID              types.UUID `json:"tenant_id"`
	ClassCode             string     `json:"class_code"` // e.g. "TAX_INVOICE", "GENERAL_LEDGER", "EMPLOYEE_RECORD"
	ClassName             string     `json:"class_name"`
	Description           string     `json:"description,omitempty"`
	DefaultRetentionYears int        `json:"default_retention_years"`
	CreatedAt             time.Time  `json:"created_at"`
}

// RecordDeclaration records the binding of a business object to governed record controls (§11, §28, DG-031, DG-032).
type RecordDeclaration struct {
	DeclarationID      types.UUID         `json:"declaration_id"`
	TenantID           types.UUID         `json:"tenant_id"`
	RecordClassID      types.UUID         `json:"record_class_id"`
	BusinessObjectType string             `json:"business_object_type"`
	BusinessObjectID   types.UUID         `json:"business_object_id"`
	DeclarationTrigger DeclarationTrigger `json:"declaration_trigger"`
	DeclaredAt         time.Time          `json:"declared_at"`
	DeclaredBy         string             `json:"declared_by"`
	IsImmutable        bool               `json:"is_immutable"` // Always true once declared (GOV-12, DG-032, NP-11)
}

// RetentionScheduleVersion models an effective-dated retention rule (§14, §28, DG-033, NP-12).
type RetentionScheduleVersion struct {
	ScheduleID           types.UUID `json:"schedule_id"`
	TenantID             types.UUID `json:"tenant_id"`
	RecordClassID        types.UUID `json:"record_class_id"`
	JurisdictionCode     string     `json:"jurisdiction_code"`
	LegalRegulatoryBasis string     `json:"legal_regulatory_basis"`
	Version              int        `json:"version"` // Incremented on change; pinned per record (NP-12)
	MinRetentionDays     int        `json:"min_retention_days"`
	MaxRetentionDays     *int       `json:"max_retention_days,omitempty"`
	TriggerEventType     string     `json:"trigger_event_type"` // e.g. "TAX_YEAR_END", "TERMINATION_DATE"
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
}

// RetentionTrigger records the authoritative event date and calculated disposition eligibility (§15, §28, DG-034, DG-035).
type RetentionTrigger struct {
	TriggerID               types.UUID    `json:"trigger_id"`
	TenantID                types.UUID    `json:"tenant_id"`
	DeclarationID           types.UUID    `json:"declaration_id"`
	ScheduleID              types.UUID    `json:"schedule_id"`
	ScheduleVersion         int           `json:"schedule_version"` // Pinned schedule version
	TriggerDate             time.Time     `json:"trigger_date"`     // Authoritative event/date
	EligibleDispositionDate time.Time     `json:"eligible_disposition_date"`
	Status                  TriggerStatus `json:"status"`
	CalculatedAt            time.Time     `json:"calculated_at"`
}
