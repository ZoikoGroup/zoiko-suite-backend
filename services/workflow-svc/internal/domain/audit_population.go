package domain

import "time"

// AuditPopulation is the AUD-03 frozen, reproducible population manifest.
// This service does not own a source-system extraction engine: the extract
// itself happens outside this service, and BuildPopulation/FreezePopulation
// exist to turn a caller-supplied item_count/control_total into a
// hash-verifiable, immutable fact — not to re-implement ETL.
type AuditPopulation struct {
	PopulationID             string     `json:"population_id"`
	EngagementID             string     `json:"engagement_id"`
	TenantID                 string     `json:"tenant_id"`
	SourceSystem             string     `json:"source_system"`
	SourceObject             string     `json:"source_object"`
	FilterSpec               string     `json:"filter_spec"`
	PeriodStart              time.Time  `json:"period_start"`
	PeriodEnd                time.Time  `json:"period_end"`
	Watermark                string     `json:"watermark"`
	Status                   string     `json:"status"`
	ItemCount                *int64     `json:"item_count,omitempty"`
	ControlTotalAmount       *float64   `json:"control_total_amount,omitempty"`
	Digest                   *string    `json:"digest,omitempty"`
	PriorPopulationID        *string    `json:"prior_population_id,omitempty"`
	SupersededByPopulationID *string    `json:"superseded_by_population_id,omitempty"`
	SupersedeReason          *string    `json:"supersede_reason,omitempty"`
	QuarantineReason         *string    `json:"quarantine_reason,omitempty"`
	CreatedByPrincipalID     string     `json:"created_by_principal_id"`
	CreatedAt                time.Time  `json:"created_at"`
	FrozenAt                 *time.Time `json:"frozen_at,omitempty"`
}

const (
	AuditPopulationDefined     = "DEFINED"
	AuditPopulationBuilding    = "BUILDING"
	AuditPopulationValidating  = "VALIDATING"
	AuditPopulationFrozen      = "FROZEN"
	AuditPopulationInUse       = "IN_USE"
	AuditPopulationSuperseded  = "SUPERSEDED"
	AuditPopulationQuarantined = "QUARANTINED"
)

type DefinePopulationParams struct {
	EngagementID, TenantID, SourceSystem, SourceObject, FilterSpec, Watermark string
	PeriodStart, PeriodEnd                                                    time.Time
	CreatedByPrincipalID, CorrelationID                                       string
}

type BuildPopulationParams struct {
	PopulationID, TenantID, ActorPrincipalID, CorrelationID string
	ItemCount                                               int64
	ControlTotalAmount                                      float64
}

// ValidatePopulationParams carries the independently-computed reconciliation
// totals (e.g. from a separate source-system control-total query) that the
// built extract must match before it is eligible to freeze. A mismatch
// quarantines the population rather than allowing a silent freeze
// (AUD-NEG-008: "count matches source but monetary control total differs").
type ValidatePopulationParams struct {
	PopulationID, TenantID, ActorPrincipalID, CorrelationID string
	ExpectedItemCount                                       int64
	ExpectedControlTotalAmount                              float64
}

type FreezePopulationParams struct {
	PopulationID, TenantID, ActorPrincipalID, CorrelationID string
}

type SupersedePopulationParams struct {
	PopulationID, TenantID, ActorPrincipalID, CorrelationID, Reason string
}

type QuarantinePopulationParams struct {
	PopulationID, TenantID, ActorPrincipalID, CorrelationID, Reason string
}

// AddControlledDeltaParams creates a new successor population version that
// folds late items into a frozen population without mutating the original
// (AUD-NEG-010). DeltaItemCount/DeltaControlTotalAmount are the incremental
// counts being added; the successor's own totals are the original's totals
// plus this delta.
type AddControlledDeltaParams struct {
	PopulationID, TenantID, ActorPrincipalID, CorrelationID, Reason string
	DeltaItemCount                                                  int64
	DeltaControlTotalAmount                                         float64
}

var ErrAuditPopulationNotFound = errorString("audit population not found")
var ErrAuditPopulationInvalidState = errorString("audit population is not in a state that permits this action")
var ErrAuditPopulationValidationFailed = errorString("population extract does not reconcile to the expected counts — quarantined")
