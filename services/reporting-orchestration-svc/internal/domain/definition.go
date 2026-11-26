package domain

import (
	"fmt"
	"strings"
	"time"
)

// ─── Report definition (REP-05 Report Builder) ───────────────────────────────
//
// A report definition describes WHAT a report is: its type, output format and
// the governed data sources it may read. It follows the REP-05 state model:
//
//	DRAFT ──> VALIDATED ──> ACTIVE <──> PAUSED
//	  │           │           │           │
//	  └───────────┴───────────┴───────────┴──> DEPRECATED (terminal)
//
// Runs may only be requested against an ACTIVE definition.

type ReportType string
type OutputFormat string
type DefinitionStatus string

const (
	ReportTypeFinancialSummary   ReportType = "FINANCIAL_SUMMARY"
	ReportTypePayrollSummary     ReportType = "PAYROLL_SUMMARY"
	ReportTypeComplianceOverview ReportType = "COMPLIANCE_OVERVIEW"
	ReportTypeAuditTrail         ReportType = "AUDIT_TRAIL"
	ReportTypeCashFlow           ReportType = "CASH_FLOW"
	ReportTypeWorkforceAnalytics ReportType = "WORKFORCE_ANALYTICS"
)

const (
	FormatJSON OutputFormat = "JSON"
	FormatCSV  OutputFormat = "CSV"
	FormatPDF  OutputFormat = "PDF"
)

const (
	DefStatusDraft      DefinitionStatus = "DRAFT"
	DefStatusValidated  DefinitionStatus = "VALIDATED"
	DefStatusActive     DefinitionStatus = "ACTIVE"
	DefStatusPaused     DefinitionStatus = "PAUSED"
	DefStatusDeprecated DefinitionStatus = "DEPRECATED"
)

// knownReportTypes is the closed set of report types a definition may use.
var knownReportTypes = map[ReportType]bool{
	ReportTypeFinancialSummary:   true,
	ReportTypePayrollSummary:     true,
	ReportTypeComplianceOverview: true,
	ReportTypeAuditTrail:         true,
	ReportTypeCashFlow:           true,
	ReportTypeWorkforceAnalytics: true,
}

// knownOutputFormats is the closed set of output formats.
var knownOutputFormats = map[OutputFormat]bool{
	FormatJSON: true,
	FormatCSV:  true,
	FormatPDF:  true,
}

// allowedDefinitionTransitions is the single source of truth for the
// definition lifecycle. A transition not listed here is illegal (HTTP 409).
var allowedDefinitionTransitions = map[DefinitionStatus][]DefinitionStatus{
	DefStatusDraft:      {DefStatusValidated, DefStatusDeprecated},
	DefStatusValidated:  {DefStatusActive, DefStatusDeprecated},
	DefStatusActive:     {DefStatusPaused, DefStatusDeprecated},
	DefStatusPaused:     {DefStatusActive, DefStatusDeprecated},
	DefStatusDeprecated: {},
}

// ParseDefinitionStatus returns the status named by text, and whether it is
// one of the known lifecycle states.
func ParseDefinitionStatus(text string) (DefinitionStatus, bool) {
	status := DefinitionStatus(strings.ToUpper(strings.TrimSpace(text)))
	_, known := allowedDefinitionTransitions[status]
	return status, known
}

// CanTransitionDefinition reports whether the lifecycle allows from -> to.
func CanTransitionDefinition(from, to DefinitionStatus) bool {
	for _, allowed := range allowedDefinitionTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

type ReportDefinition struct {
	ID            string           `json:"id"`
	TenantID      string           `json:"tenant_id"`
	LegalEntityID string           `json:"legal_entity_id"`
	ReportName    string           `json:"report_name"`
	ReportType    ReportType       `json:"report_type"`
	OutputFormat  OutputFormat     `json:"output_format"`
	DataSources   []string         `json:"data_sources"`
	ScheduleCron  string           `json:"schedule_cron,omitempty"`
	IsScheduled   bool             `json:"is_scheduled"`
	Status        DefinitionStatus `json:"status"`
	Version       int              `json:"version"`
	CreatedBy     string           `json:"created_by"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// CreateDefinitionRequest is the body of POST /v1/reports/definitions.
// TenantID is optional; when present it must equal X-Tenant-Id.
type CreateDefinitionRequest struct {
	TenantID      string       `json:"tenant_id,omitempty"`
	LegalEntityID string       `json:"legal_entity_id"`
	ReportName    string       `json:"report_name"`
	ReportType    ReportType   `json:"report_type"`
	OutputFormat  OutputFormat `json:"output_format"`
	DataSources   []string     `json:"data_sources"`
	ScheduleCron  string       `json:"schedule_cron,omitempty"`
	IsScheduled   bool         `json:"is_scheduled"`
}

// UpdateDefinitionStatusRequest is the body of
// PATCH /v1/reports/definitions/{id}/status.
type UpdateDefinitionStatusRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Validate checks the create request field by field and fills defaults.
// It returns the first problem found, phrased for the API caller.
func (r *CreateDefinitionRequest) Validate() error {
	r.ReportName = strings.TrimSpace(r.ReportName)
	r.LegalEntityID = strings.TrimSpace(r.LegalEntityID)

	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.ReportName == "" {
		return fmt.Errorf("report_name is required")
	}
	if len(r.ReportName) > 128 {
		return fmt.Errorf("report_name must be at most 128 characters")
	}
	if r.ReportType == "" {
		return fmt.Errorf("report_type is required")
	}
	if !knownReportTypes[r.ReportType] {
		return fmt.Errorf("report_type %q is not a known report type", r.ReportType)
	}

	if r.OutputFormat == "" {
		r.OutputFormat = FormatJSON
	}
	if !knownOutputFormats[r.OutputFormat] {
		return fmt.Errorf("output_format %q must be one of JSON, CSV, PDF", r.OutputFormat)
	}

	if len(r.DataSources) == 0 {
		return fmt.Errorf("at least one data_source is required")
	}
	for _, source := range r.DataSources {
		if strings.TrimSpace(source) == "" {
			return fmt.Errorf("data_sources must not contain empty entries")
		}
	}

	if r.IsScheduled && !looksLikeCronExpression(r.ScheduleCron) {
		return fmt.Errorf("schedule_cron must be a 5-field cron expression when is_scheduled is true")
	}
	if !r.IsScheduled && r.ScheduleCron != "" && !looksLikeCronExpression(r.ScheduleCron) {
		return fmt.Errorf("schedule_cron must be a 5-field cron expression")
	}
	return nil
}

// looksLikeCronExpression accepts exactly five whitespace-separated fields.
// It is a shape check only; schedule execution is not implemented here.
func looksLikeCronExpression(expression string) bool {
	return len(strings.Fields(expression)) == 5
}
