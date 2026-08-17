// Control test / attestation domain types — doc7 §E3, §E6, §I3.
//
// Same doctrine as the rest of this package: control_ref, subject_ref,
// design_status, result, and attestation_status are all plain strings, DATA
// ONLY, never a Go enum or a switch/case in validation logic.
package domain

import (
	"time"
)

// ControlTestDefinition is a repeatable test methodology for a control.
// Immutable once created — same doctrine as Policy: no UPDATE/DELETE, a
// changed methodology is a new row with a new test_code.
//
// DesignStatus answers only "does a defined test methodology exist for this
// control" (doc7 §E3's DESIGN_STATUS) — it is NOT and must never become a
// stand-in for whether the control actually works. That answer lives
// exclusively on ControlTestExecution.Result.
type ControlTestDefinition struct {
	ControlTestDefinitionID string    `json:"control_test_definition_id"`
	ControlRef              string    `json:"control_ref"`
	TestCode                string    `json:"test_code"`
	Title                   string    `json:"title"`
	Methodology             string    `json:"methodology"`
	SampleApproach          string    `json:"sample_approach,omitempty"`
	TestFrequency           string    `json:"test_frequency"`
	DesignStatus            string    `json:"design_status"`
	CreatedAt               time.Time `json:"created_at"`
	CreatedByPrincipalID    string    `json:"created_by_principal_id"`
}

// CreateControlTestDefinitionParams holds input for creating a definition.
type CreateControlTestDefinitionParams struct {
	ControlTestDefinitionID string
	ControlRef              string
	TestCode                string
	Title                   string
	Methodology             string
	SampleApproach          string
	TestFrequency           string
	CreatedByPrincipalID    string
}

// ControlTestExecution is one actual test run against a
// ControlTestDefinition — doc7 §I3's full field list: test_definition
// reference, period, population/sample, procedure, evidence refs, tester,
// result, exceptions, reviewer, timestamps.
//
// Result is the OPERATING_EFFECTIVENESS signal doc7 §E3 requires be kept
// separate from a control's mere existence. It lives here, only here, and
// is never copied back onto ControlTestDefinition.
type ControlTestExecution struct {
	ControlTestExecutionID  string     `json:"control_test_execution_id"`
	ControlTestDefinitionID string     `json:"control_test_definition_id"`
	PeriodStart             time.Time  `json:"period_start"`
	PeriodEnd               time.Time  `json:"period_end"`
	PopulationDescription   string     `json:"population_description,omitempty"`
	SampleDescription       string     `json:"sample_description,omitempty"`
	ProcedureNotes          string     `json:"procedure_notes,omitempty"`
	EvidenceRefs            []string   `json:"evidence_refs"`
	TesterPrincipalID       string     `json:"tester_principal_id"`
	Result                  string     `json:"result"`
	ExceptionsNoted         string     `json:"exceptions_noted,omitempty"`
	ReviewerPrincipalID     *string    `json:"reviewer_principal_id,omitempty"`
	ReviewedAt              *time.Time `json:"reviewed_at,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	CreatedByPrincipalID    string     `json:"created_by_principal_id"`
}

// CreateControlTestExecutionParams holds input for recording an execution.
type CreateControlTestExecutionParams struct {
	ControlTestExecutionID  string
	ControlTestDefinitionID string
	PeriodStart             time.Time
	PeriodEnd               time.Time
	PopulationDescription   string
	SampleDescription       string
	ProcedureNotes          string
	EvidenceRefs            []string
	TesterPrincipalID       string
	Result                  string
	ExceptionsNoted         string
	ReviewerPrincipalID     string
	CreatedByPrincipalID    string
}

// ControlEffectiveness is the answer to doc7 §E3's core question, composing
// DESIGN_STATUS (does a tested-methodology exist for this control) and
// OPERATING_EFFECTIVENESS (what the most recent execution actually found) —
// deliberately two independent fields, never collapsed into one status.
type ControlEffectiveness struct {
	ControlRef             string     `json:"control_ref"`
	DesignStatus           string     `json:"design_status"`           // TESTED | NOT_TESTED
	OperatingEffectiveness string     `json:"operating_effectiveness"` // mirrors the latest execution's Result, or NO_EXECUTIONS_RECORDED
	AsOf                   *time.Time `json:"as_of,omitempty"`
	LatestExecutionID      *string    `json:"latest_execution_id,omitempty"`
}

// Attestation is doc7 §E6's signed/attributed assertion — a representation
// by an identified actor, never automatic proof and never inferred from a
// ControlTestExecution's result.
type Attestation struct {
	AttestationID        string     `json:"attestation_id"`
	Statement            string     `json:"statement"`
	StatementVersion     string     `json:"statement_version"`
	SubjectRef           string     `json:"subject_ref"`
	PeriodStart          time.Time  `json:"period_start"`
	PeriodEnd            time.Time  `json:"period_end"`
	SignerPrincipalID    string     `json:"signer_principal_id"`
	SignerRole           string     `json:"signer_role"`
	SignedAt             time.Time  `json:"signed_at"`
	EvidenceRefs         []string   `json:"evidence_refs"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`
	AttestationStatus    string     `json:"attestation_status"` // ACTIVE | CHALLENGED | REVOKED
	RevocationReason     string     `json:"revocation_reason,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
}

// CreateAttestationParams holds input for creating an attestation.
type CreateAttestationParams struct {
	AttestationID        string
	Statement            string
	StatementVersion     string
	SubjectRef           string
	PeriodStart          time.Time
	PeriodEnd            time.Time
	SignerPrincipalID    string
	SignerRole           string
	EvidenceRefs         []string
	ExpiresAt            *time.Time
	CreatedByPrincipalID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrControlTestDefinitionNotFound = errorString("control test definition not found")
	ErrControlTestExecutionNotFound  = errorString("control test execution not found")
	ErrAttestationNotFound           = errorString("attestation not found")
	// ErrInvalidAttestationTransition is returned when an attestation_status
	// transition is illegal (e.g. reviving a REVOKED attestation).
	ErrInvalidAttestationTransition = errorString("invalid attestation status transition")
)
