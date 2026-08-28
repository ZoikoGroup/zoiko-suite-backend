package domain

import (
	"fmt"
	"time"
)

// ─── Enums ────────────────────────────────────────────────────────────────────

type ReportType string
type OutputFormat string
type TriggerSource string
type RunStatus string
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
	TriggerManual    TriggerSource = "MANUAL"
	TriggerScheduled TriggerSource = "SCHEDULED"
	TriggerAPI       TriggerSource = "API"
)

const (
	RunStatusPending   RunStatus = "PENDING"
	RunStatusRunning   RunStatus = "RUNNING"
	RunStatusCompleted RunStatus = "COMPLETED"
	RunStatusFailed    RunStatus = "FAILED"

	// RunStatusNotImplemented is what OrchestratReportRun actually produces
	// today. This service has no real cross-service data-aggregation
	// fan-out — no HTTP calls to ledger-svc/payroll-svc/any source service
	// exist anywhere in this codebase. It previously returned
	// RunStatusCompleted with a fabricated row count looked up from a
	// hardcoded map and a fabricated OutputLocation path that was never
	// written. ZS-SVC-AC-001 (Governed Reporting/Analytics/Semantic
	// Metrics/Export Control) names this exact anti-pattern by its own
	// negative-path test NP-60: "Operator manually sets report state
	// COMPLETE during incident → No bypass; state derived from execution/
	// certification evidence." A run this function performed was never
	// derived from any execution — it never ran anything. Building the
	// real aggregation fan-out is a legitimate multi-service feature, out
	// of scope here; what this status removes is the lie that it already
	// happened.
	RunStatusNotImplemented RunStatus = "NOT_IMPLEMENTED"
)

const (
	DefStatusActive   DefinitionStatus = "ACTIVE"
	DefStatusPaused   DefinitionStatus = "PAUSED"
	DefStatusArchived DefinitionStatus = "ARCHIVED"
)

// ─── Domain Models ────────────────────────────────────────────────────────────

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
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

type ReportRun struct {
	ID             string        `json:"id"`
	TenantID       string        `json:"tenant_id"`
	DefinitionID   string        `json:"definition_id"`
	TriggeredBy    TriggerSource `json:"triggered_by"`
	PeriodStart    string        `json:"period_start,omitempty"` // ISO date YYYY-MM-DD
	PeriodEnd      string        `json:"period_end,omitempty"`
	Status         RunStatus     `json:"status"`
	RowCount       int           `json:"row_count"`
	OutputLocation string        `json:"output_location,omitempty"`
	ErrorMessage   string        `json:"error_message,omitempty"`
	StartedAt      *time.Time    `json:"started_at,omitempty"`
	CompletedAt    *time.Time    `json:"completed_at,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
}

// ─── Request / Response DTOs ──────────────────────────────────────────────────

type CreateDefinitionRequest struct {
	LegalEntityID string       `json:"legal_entity_id"`
	ReportName    string       `json:"report_name"`
	ReportType    ReportType   `json:"report_type"`
	OutputFormat  OutputFormat `json:"output_format"`
	DataSources   []string     `json:"data_sources"`
	ScheduleCron  string       `json:"schedule_cron,omitempty"`
	IsScheduled   bool         `json:"is_scheduled"`
}

type TriggerRunRequest struct {
	TriggeredBy TriggerSource `json:"triggered_by"`
	PeriodStart string        `json:"period_start,omitempty"`
	PeriodEnd   string        `json:"period_end,omitempty"`
}

// ─── Validation ───────────────────────────────────────────────────────────────

func (r *CreateDefinitionRequest) Validate() error {
	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.ReportName == "" {
		return fmt.Errorf("report_name is required")
	}
	if r.ReportType == "" {
		return fmt.Errorf("report_type is required")
	}
	if r.OutputFormat == "" {
		r.OutputFormat = FormatJSON
	}
	if len(r.DataSources) == 0 {
		return fmt.Errorf("at_least one data_source is required")
	}
	return nil
}

// ─── Orchestration Engine ─────────────────────────────────────────────────────

// OrchestratReportRun records that a run was requested. It does not
// aggregate any real data — no cross-service fan-out exists in this
// codebase — and previously fabricated a COMPLETED result with a row count
// looked up from a hardcoded map and an OutputLocation path that was never
// written. See the RunStatusNotImplemented doc comment for the full
// reasoning. This function now reports that state honestly instead.
func OrchestratReportRun(def *ReportDefinition, run *ReportRun) {
	now := time.Now()
	run.StartedAt = &now

	run.RowCount = 0
	run.Status = RunStatusNotImplemented
	run.OutputLocation = ""
	completed := time.Now()
	run.CompletedAt = &completed
}
