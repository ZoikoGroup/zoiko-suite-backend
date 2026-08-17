// Package domain defines the authoritative domain types for
// metric-registry-svc.
//
// Per docs/original_doc/zoiko_suite_doc7.txt §27 REP-01: executive metrics
// must be "defined, versioned, source-traceable and labeled so operational
// intelligence is not misrepresented as financial/legal assurance." This
// service defines what a metric IS and MEANS; it does not compute or
// store metric values itself. metric_code and definition_status are plain
// strings, DATA ONLY, no code switch/case in this service.
package domain

import "time"

// DefaultIntelligenceDisclaimer is doc7 REP-01's required labeling —
// "operational intelligence is not misrepresented as financial/legal
// assurance." Set explicitly on every definition in Go, not left to the
// database column default alone: a caller-visible response built from a
// freshly-constructed struct (rather than a re-SELECT after INSERT) must
// still carry it, or the label exists in the schema but not in what
// anyone actually reads.
const DefaultIntelligenceDisclaimer = "Operational intelligence — not financial or legal assurance."

// ReportMetricDefinition is one version of an executive metric's
// definition. Immutable once created — a changed formula/scope/owner is a
// new row with an incremented version, never an UPDATE to an existing one.
type ReportMetricDefinition struct {
	MetricDefinitionID     string    `json:"metric_definition_id"`
	MetricCode             string    `json:"metric_code"`
	MetricName             string    `json:"metric_name"`
	FormulaDescription     string    `json:"formula_description"`
	DataSources            []string  `json:"data_sources"`
	OwnerPrincipalID       string    `json:"owner_principal_id"`
	IntelligenceDisclaimer string    `json:"intelligence_disclaimer"`
	Version                int       `json:"version"`
	DefinitionStatus       string    `json:"definition_status"` // DRAFT | ACTIVE | SUPERSEDED | RETIRED
	EffectiveFrom          time.Time `json:"effective_from"`
	CreatedAt              time.Time `json:"created_at"`
	CreatedByPrincipalID   string    `json:"created_by_principal_id"`
}

// CreateReportMetricRequest is the wire shape for POST /v1/report-metrics
// — always creates version 1 of a new metric_code.
type CreateReportMetricRequest struct {
	MetricCode         string   `json:"metric_code"`
	MetricName         string   `json:"metric_name"`
	FormulaDescription string   `json:"formula_description"`
	DataSources        []string `json:"data_sources,omitempty"`
	OwnerPrincipalID   string   `json:"owner_principal_id"`
	EffectiveFrom      string   `json:"effective_from"`
	CorrelationID      string   `json:"correlation_id"`
}

// PublishMetricVersionRequest is the wire shape for
// POST /v1/report-metrics/{metric_code}/versions — publishes a new
// version, atomically superseding whatever version was previously ACTIVE.
type PublishMetricVersionRequest struct {
	MetricName         string   `json:"metric_name"`
	FormulaDescription string   `json:"formula_description"`
	DataSources        []string `json:"data_sources,omitempty"`
	OwnerPrincipalID   string   `json:"owner_principal_id"`
	EffectiveFrom      string   `json:"effective_from"`
	CorrelationID      string   `json:"correlation_id"`
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrMetricNotFound = errorString("report metric definition not found")
	ErrConflict       = errorString("conflict: metric_code already exists")
)
