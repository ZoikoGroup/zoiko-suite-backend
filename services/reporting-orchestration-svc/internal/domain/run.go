package domain

import (
	"fmt"
	"time"
)

// ─── Report run ──────────────────────────────────────────────────────────────

type TriggerSource string
type RunStatus string

const (
	TriggerManual    TriggerSource = "MANUAL"
	TriggerScheduled TriggerSource = "SCHEDULED"
	TriggerAPI       TriggerSource = "API"
)

var knownTriggerSources = map[TriggerSource]bool{
	TriggerManual:    true,
	TriggerScheduled: true,
	TriggerAPI:       true,
}

const (
	RunStatusPending   RunStatus = "PENDING"
	RunStatusRunning   RunStatus = "RUNNING"
	RunStatusCompleted RunStatus = "COMPLETED"
	RunStatusFailed    RunStatus = "FAILED"

	// RunStatusNotImplemented is what a run request honestly produces
	// today. This service has no cross-service data-aggregation fan-out —
	// no calls to ledger-svc/payroll-svc/any source service exist. It used
	// to return COMPLETED with a fabricated row count and an output path
	// that was never written. ZS-SVC-AC-001 NP-60 forbids exactly that:
	// "state derived from execution/certification evidence". Until the
	// real aggregation exists, the run is recorded as NOT_IMPLEMENTED.
	RunStatusNotImplemented RunStatus = "NOT_IMPLEMENTED"
)

// isoDateLayout is the only accepted period date format (YYYY-MM-DD).
const isoDateLayout = "2006-01-02"

type ReportRun struct {
	ID             string        `json:"id"`
	TenantID       string        `json:"tenant_id"`
	DefinitionID   string        `json:"definition_id"`
	TriggeredBy    TriggerSource `json:"triggered_by"`
	RequestedBy    string        `json:"requested_by"`
	PeriodStart    string        `json:"period_start,omitempty"`
	PeriodEnd      string        `json:"period_end,omitempty"`
	Status         RunStatus     `json:"status"`
	RowCount       int           `json:"row_count"`
	OutputLocation string        `json:"output_location,omitempty"`
	ErrorMessage   string        `json:"error_message,omitempty"`
	StartedAt      *time.Time    `json:"started_at,omitempty"`
	CompletedAt    *time.Time    `json:"completed_at,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
}

// TriggerRunRequest is the (optional) body of
// POST /v1/reports/definitions/{id}/runs.
type TriggerRunRequest struct {
	TriggeredBy TriggerSource `json:"triggered_by"`
	PeriodStart string        `json:"period_start,omitempty"`
	PeriodEnd   string        `json:"period_end,omitempty"`
}

// Validate fills the default trigger source and checks the reporting period.
func (r *TriggerRunRequest) Validate() error {
	if r.TriggeredBy == "" {
		r.TriggeredBy = TriggerManual
	}
	if !knownTriggerSources[r.TriggeredBy] {
		return fmt.Errorf("triggered_by %q must be one of MANUAL, SCHEDULED, API", r.TriggeredBy)
	}

	hasStart := r.PeriodStart != ""
	hasEnd := r.PeriodEnd != ""
	if hasStart != hasEnd {
		return fmt.Errorf("period_start and period_end must be supplied together")
	}
	if !hasStart {
		return nil
	}

	start, err := time.Parse(isoDateLayout, r.PeriodStart)
	if err != nil {
		return fmt.Errorf("period_start must be a date in YYYY-MM-DD format")
	}
	end, err := time.Parse(isoDateLayout, r.PeriodEnd)
	if err != nil {
		return fmt.Errorf("period_end must be a date in YYYY-MM-DD format")
	}
	if end.Before(start) {
		return fmt.Errorf("period_end must not be before period_start")
	}
	return nil
}

// NewRecordedRun builds the run row for a run request against definition.
//
// No report data is aggregated (see RunStatusNotImplemented). The run is
// recorded with zero rows, no output location, and NOT_IMPLEMENTED status,
// so no consumer is ever told a report exists when none was produced.
func NewRecordedRun(definition *ReportDefinition, request TriggerRunRequest, requestedBy string) *ReportRun {
	now := time.Now().UTC()
	return &ReportRun{
		TenantID:       definition.TenantID,
		DefinitionID:   definition.ID,
		TriggeredBy:    request.TriggeredBy,
		RequestedBy:    requestedBy,
		PeriodStart:    request.PeriodStart,
		PeriodEnd:      request.PeriodEnd,
		Status:         RunStatusNotImplemented,
		RowCount:       0,
		OutputLocation: "",
		ErrorMessage:   "report aggregation is not implemented in this service; no report data was produced",
		StartedAt:      &now,
		CompletedAt:    &now,
		CreatedAt:      now,
	}
}
