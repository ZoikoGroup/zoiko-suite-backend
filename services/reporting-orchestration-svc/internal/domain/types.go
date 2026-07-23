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
	ReportTypeFinancialSummary    ReportType = "FINANCIAL_SUMMARY"
	ReportTypePayrollSummary      ReportType = "PAYROLL_SUMMARY"
	ReportTypeComplianceOverview  ReportType = "COMPLIANCE_OVERVIEW"
	ReportTypeAuditTrail          ReportType = "AUDIT_TRAIL"
	ReportTypeCashFlow            ReportType = "CASH_FLOW"
	ReportTypeWorkforceAnalytics  ReportType = "WORKFORCE_ANALYTICS"
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
	ID              string        `json:"id"`
	TenantID        string        `json:"tenant_id"`
	DefinitionID    string        `json:"definition_id"`
	TriggeredBy     TriggerSource `json:"triggered_by"`
	PeriodStart     string        `json:"period_start,omitempty"` // ISO date YYYY-MM-DD
	PeriodEnd       string        `json:"period_end,omitempty"`
	Status          RunStatus     `json:"status"`
	RowCount        int           `json:"row_count"`
	OutputLocation  string        `json:"output_location,omitempty"`
	ErrorMessage    string        `json:"error_message,omitempty"`
	StartedAt       *time.Time    `json:"started_at,omitempty"`
	CompletedAt     *time.Time    `json:"completed_at,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
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

// OrchestratReportRun simulates the cross-service data aggregation and report generation
// In production this would fan-out requests to data source services (ledger-svc, payroll-svc, etc.)
func OrchestratReportRun(def *ReportDefinition, run *ReportRun) {
	now := time.Now()
	run.StartedAt = &now

	// Simulate row count based on report type
	rowCountMap := map[ReportType]int{
		ReportTypeFinancialSummary:   245,
		ReportTypePayrollSummary:     180,
		ReportTypeComplianceOverview: 94,
		ReportTypeAuditTrail:         1200,
		ReportTypeCashFlow:           312,
		ReportTypeWorkforceAnalytics: 430,
	}

	count, ok := rowCountMap[def.ReportType]
	if !ok {
		count = 100
	}

	run.RowCount = count
	run.Status = RunStatusCompleted
	run.OutputLocation = fmt.Sprintf("/reports/%s/%s/%s.%s",
		def.TenantID, def.ReportType, run.ID, def.OutputFormat)
	completed := time.Now()
	run.CompletedAt = &completed
}
