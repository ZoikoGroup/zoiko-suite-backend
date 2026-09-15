package domain

import (
	"errors"
	"time"
)

// AUD-03 Audit Population is a genuinely separate concern from
// EvidenceManifest above: a population's identity is a source query plus
// control totals plus a frozen row set evaluated against a source-of-truth
// system, with its own Defined->Frozen state machine — distinct from
// EvidenceManifest's job of snapshotting other SERVICES' governance/access/
// workflow decisions. A Frozen population may still generate an
// EvidenceManifest as an optional cover artifact (reusing its checksum
// primitive), but AUD-03 owns its own tables.
//
// This build does not own a source-system extraction engine. BuildPopulation
// accepts a caller-computed row set (item_count/control totals arrive from
// outside this service); these tables exist to make that extract a frozen,
// reproducible, hash-verifiable fact, not to re-implement ETL.

type PopulationStatus string

const (
	PopulationDefined     PopulationStatus = "DEFINED"
	PopulationBuilding    PopulationStatus = "BUILDING"
	PopulationValidating  PopulationStatus = "VALIDATING"
	PopulationFrozen      PopulationStatus = "FROZEN"
	PopulationInUse       PopulationStatus = "IN_USE"
	PopulationSuperseded  PopulationStatus = "SUPERSEDED"
	PopulationQuarantined PopulationStatus = "QUARANTINED"
)

// AuditPopulation is AUD-03's own "AuditPopulation" + "PopulationSpecification"
// combined into one row — the specification fields (source/filter/watermark)
// are set once at DefinePopulation and never updated again, which alone is
// "source/filter/watermark preserved."
type AuditPopulation struct {
	PopulationID              string           `json:"population_id"`
	EngagementID              string           `json:"engagement_id"`
	TenantID                  string           `json:"tenant_id"`
	LegalEntityID             string           `json:"legal_entity_id"`
	ObjectClass               string           `json:"object_class"`
	PeriodStart               time.Time        `json:"period_start"`
	PeriodEnd                 time.Time        `json:"period_end"`
	SourceSystem              string           `json:"source_system"`
	SourceQuery               string           `json:"source_query"`
	SourceWatermark           string           `json:"source_watermark"`
	Assertion                 string           `json:"assertion"`
	ExpectedCompletenessCheck string           `json:"expected_completeness_check"`
	Status                    PopulationStatus `json:"status"`
	RowCount                  *int64           `json:"row_count,omitempty"`
	DigestSHA256              *string          `json:"digest_sha256,omitempty"`
	PriorPopulationID         *string          `json:"prior_population_id,omitempty"`
	SupersededByPopulationID  *string          `json:"superseded_by_population_id,omitempty"`
	QuarantineReason          *string          `json:"quarantine_reason,omitempty"`
	CreatedByPrincipalID      string           `json:"created_by_principal_id"`
	CreatedAt                 time.Time        `json:"created_at"`
	FrozenAt                  *time.Time       `json:"frozen_at,omitempty"`
}

// PopulationControlTotal is one per-measure reconciliation check —
// AUD-CTRL-007's real teeth: FreezePopulation refuses unless every measure
// reconciles.
type PopulationControlTotal struct {
	ControlTotalID string  `json:"control_total_id"`
	PopulationID   string  `json:"population_id"`
	MeasureName    string  `json:"measure_name"`
	SourceValue    float64 `json:"source_value"`
	ComputedValue  float64 `json:"computed_value"`
	Reconciled     bool    `json:"reconciled"`
}

// PopulationRow is the population's own frozen, ordered row set —
// Ordinal is fixed at Freeze time and is what AUD-04's reproducible
// sampling walks.
type PopulationRow struct {
	RowID          string   `json:"row_id"`
	PopulationID   string   `json:"population_id"`
	Ordinal        int64    `json:"ordinal"`
	SourceRecordID string   `json:"source_record_id"`
	Amount         *float64 `json:"amount,omitempty"`
	RowSnapshot    []byte   `json:"row_snapshot"`
}

// PopulationDelta is AUD-NEG-010's own mechanism: a late item never
// mutates the frozen population, it links to a NEW population version.
type PopulationDelta struct {
	DeltaID              string    `json:"delta_id"`
	PopulationID         string    `json:"population_id"`
	Reason               string    `json:"reason"`
	NewPopulationID      string    `json:"new_population_id"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	CreatedAt            time.Time `json:"created_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

type DefinePopulationParams struct {
	EngagementID, TenantID, LegalEntityID, ObjectClass             string
	PeriodStart, PeriodEnd                                         time.Time
	SourceSystem, SourceQuery, SourceWatermark, Assertion          string
	ExpectedCompletenessCheck, CreatedByPrincipalID, CorrelationID string
}

// BuildPopulationRow is one caller-supplied extract row — this service
// does not run source-system ETL itself (see the package doc above).
type BuildPopulationRow struct {
	SourceRecordID string
	Amount         *float64
	RowSnapshot    []byte
}

type BuildPopulationParams struct {
	PopulationID, TenantID, CorrelationID string
	Rows                                  []BuildPopulationRow
}

type ValidatePopulationParams struct {
	PopulationID, TenantID, CorrelationID string
	// ControlTotals maps measure_name -> source_value, the value the
	// caller asserts the SOURCE system reports for that measure — the
	// computed_value is always derived here, from population_rows, never
	// caller-supplied.
	ControlTotals map[string]float64
}

type FreezePopulationParams struct {
	PopulationID, TenantID, CorrelationID string
}

type SupersedePopulationParams struct {
	PopulationID, TenantID, Reason, CorrelationID string
}

type QuarantinePopulationParams struct {
	PopulationID, TenantID, Reason string
}

type AddControlledDeltaParams struct {
	PopulationID, TenantID, Reason, CreatedByPrincipalID, CorrelationID string
	Rows                                                                []BuildPopulationRow
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrPopulationNotFound             = errors.New("audit population not found")
	ErrPopulationInvalidState         = errors.New("audit population is not in a state that permits this action")
	ErrPopulationNotFrozen            = errors.New("audit population must be FROZEN or IN_USE for evidential use")
	ErrPopulationControlTotalMismatch = errors.New("population control totals do not reconcile")
)
