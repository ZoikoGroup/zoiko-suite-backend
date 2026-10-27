package domain

import "time"

// Doc 03 §8.5: obligations-svc "Owns obligation definitions … obligation-to-
// entity mappings". A definition is the tenant's catalogue entry for a kind of
// obligation (what it is, where it comes from, who answers for it); each
// change makes a new immutable version. A mapping says which legal entities a
// definition binds, and from when. An Obligation is an instance: one due date
// for one entity, optionally raised from a definition and pinned to the
// version it was raised from, so the record shows what the definition said
// at the time.

// ObligationDefinition is a catalogue entry with its current version inlined.
type ObligationDefinition struct {
	ObligationDefinitionID string     `json:"obligation_definition_id"`
	DefinitionCode         string     `json:"definition_code"`
	CurrentVersion         int        `json:"current_version"`
	DefinitionStatus       string     `json:"definition_status"`
	CreatedAt              time.Time  `json:"created_at"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	UpdatedAt              time.Time  `json:"updated_at"`
	RetiredAt              *time.Time `json:"retired_at,omitempty"`

	Current *ObligationDefinitionVersion `json:"current,omitempty"`
}

// ObligationDefinitionVersion is one immutable version of a definition.
type ObligationDefinitionVersion struct {
	ObligationDefinitionID string    `json:"obligation_definition_id"`
	Version                int       `json:"version"`
	JurisdictionID         string    `json:"jurisdiction_id"`
	ObligationType         string    `json:"obligation_type"`
	ObligationSourceType   string    `json:"obligation_source_type"`
	ObligationSourceID     string    `json:"obligation_source_id"`
	SourceReference        string    `json:"source_reference"`
	SeverityLevel          string    `json:"severity_level"`
	ResponsibleFunction    string    `json:"responsible_function"`
	Description            string    `json:"description"`
	ChangeReason           string    `json:"change_reason"`
	CreatedAt              time.Time `json:"created_at"`
	CreatedByPrincipalID   string    `json:"created_by_principal_id"`
}

// DefinitionContent is the versioned part of a definition, as written.
type DefinitionContent struct {
	JurisdictionID       string
	ObligationType       string
	ObligationSourceType string
	ObligationSourceID   string
	SourceReference      string
	SeverityLevel        string
	ResponsibleFunction  string
	Description          string
}

// CreateDefinitionParams creates a definition at version 1.
type CreateDefinitionParams struct {
	DefinitionCode       string
	Content              DefinitionContent
	CreatedByPrincipalID string
	CorrelationID        string
}

// NewDefinitionVersionParams supersedes the current version.
type NewDefinitionVersionParams struct {
	ObligationDefinitionID string
	ExpectedVersion        int
	Content                DefinitionContent
	ChangeReason           string
	ActorPrincipalID       string
	CorrelationID          string
}

// ObligationEntityMapping binds a definition to a legal entity over an interval.
type ObligationEntityMapping struct {
	MappingID              string     `json:"mapping_id"`
	ObligationDefinitionID string     `json:"obligation_definition_id"`
	LegalEntityID          string     `json:"legal_entity_id"`
	EffectiveFrom          time.Time  `json:"effective_from"`
	EffectiveTo            *time.Time `json:"effective_to,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	EndedByPrincipalID     *string    `json:"ended_by_principal_id,omitempty"`
}

// CreateMappingParams opens a mapping.
type CreateMappingParams struct {
	ObligationDefinitionID string
	LegalEntityID          string
	EffectiveFrom          time.Time
	ActorPrincipalID       string
	CorrelationID          string
}

// UpdateObligationParams changes an obligation's mutable attributes under
// If-Match. A nil field is left unchanged.
type UpdateObligationParams struct {
	ObligationID        string
	ExpectedVersion     int
	DueDate             *time.Time
	SeverityLevel       *string
	ResponsibleFunction *string
	ActorPrincipalID    string
	CorrelationID       string
}

// FilingStatuses is the FilingRequirement lifecycle (Doc 04 §13.1 filing_status).
var FilingStatuses = map[string][]string{
	"PENDING":        {"IN_PREPARATION", "SUBMITTED", "WITHDRAWN"},
	"IN_PREPARATION": {"SUBMITTED", "WITHDRAWN"},
	"SUBMITTED":      {"ACCEPTED", "REJECTED"},
	"REJECTED":       {"IN_PREPARATION", "SUBMITTED", "WITHDRAWN"},
	"ACCEPTED":       {},
	"WITHDRAWN":      {},
}

// UpdateFilingStatusParams moves a filing requirement along its lifecycle.
type UpdateFilingStatusParams struct {
	ObligationID        string
	FilingRequirementID string
	NewStatus           string
	Reason              string
	ExpectedVersion     int
	ActorPrincipalID    string
	CorrelationID       string
}

// FilingTransition is one append-only step of a filing requirement.
type FilingTransition struct {
	TransitionID        string    `json:"transition_id"`
	FilingRequirementID string    `json:"filing_requirement_id"`
	FromStatus          string    `json:"from_status"`
	ToStatus            string    `json:"to_status"`
	Reason              *string   `json:"reason,omitempty"`
	ActorPrincipalID    string    `json:"actor_principal_id"`
	CorrelationID       string    `json:"correlation_id"`
	CreatedAt           time.Time `json:"created_at"`
}

// ObligationStatuses are the values obligation_status may take.
var ObligationStatuses = map[string]bool{"OPEN": true, "IN_PROGRESS": true, "OVERDUE": true, "CLOSED": true}

// ApplicabilityDecisionValues are the decisions that may be stored. UNASSESSED
// is the absence of a decision and is never stored (000006 CHECK).
var ApplicabilityDecisionValues = map[string]bool{"APPLICABLE": true, "NOT_APPLICABLE": true, "UNCERTAIN": true}

var (
	ErrObligationIDConflict      = errorString("obligation_id already in use")
	ErrFilingRequirementNotFound = errorString("filing requirement not found")
	ErrDefinitionNotFound        = errorString("obligation definition not found")
	ErrDefinitionRetired         = errorString("obligation definition is retired")
	ErrDefinitionCodeConflict    = errorString("definition_code already exists with different content")
	ErrMappingNotFound           = errorString("entity mapping not found")
	ErrMappingExists             = errorString("an open mapping already binds this definition to the entity")
	ErrEntityNotMapped           = errorString("the definition is not mapped to this legal entity")
	ErrObligationClosed          = errorString("obligation is closed")
	ErrLegalEntityNotFound       = errorString("legal entity does not exist in this tenant")
	ErrRegistryUnavailable       = errorString("tenant-entity-registry-svc unavailable")
)
