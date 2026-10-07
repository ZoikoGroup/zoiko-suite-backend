package domain

import (
	"errors"
	"time"
)

// AUD-04 Sampling operates over an AUD-03 frozen population's own ordered
// row set. Selection uses systematic interval sampling over the
// population's frozen `ordinal` order — reproducible from two stored
// numbers (start_point, interval) rather than requiring an exact PRNG
// implementation match across time. sample_size is an explicit,
// auditor-declared input, never derived from a confidence-interval
// formula: the doc's own §9.1 "Controlled pre-production decisions" names
// "numerical precision, RNG requirements" under Sampling engines as
// requiring resolution "with methodology specialists" — fabricating an
// unvalidated statistical sample-size formula here would misrepresent
// actuarial rigor this codebase does not have.

type SampleDesignStatus string

const (
	SampleDesignDraft       SampleDesignStatus = "DRAFT_DESIGN"
	SampleDesignApproved    SampleDesignStatus = "APPROVED_DESIGN"
	SampleDesignSelected    SampleDesignStatus = "SELECTED"
	SampleDesignTesting     SampleDesignStatus = "TESTING"
	SampleDesignEvaluated   SampleDesignStatus = "EVALUATED"
	SampleDesignLocked      SampleDesignStatus = "LOCKED"
	SampleDesignInvalidated SampleDesignStatus = "INVALIDATED"
)

// SamplingParameterSet is versioned — a parameter change never mutates a
// row in place, RecordMateriality-style: a new version is inserted. This
// is the mechanism AUD-NEG-011 depends on (see SelectSample's own doc
// comment).
type SamplingParameterSet struct {
	ParamSetID            string    `json:"param_set_id"`
	TenantID              string    `json:"tenant_id"`
	Version               int       `json:"version"`
	Approach              string    `json:"approach"`
	TolerableMisstatement *float64  `json:"tolerable_misstatement,omitempty"`
	ExpectedMisstatement  *float64  `json:"expected_misstatement,omitempty"`
	ConfidenceLevel       *float64  `json:"confidence_level,omitempty"`
	KeyItemThreshold      *float64  `json:"key_item_threshold,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
}

const (
	SamplingApproachRandom     = "RANDOM"
	SamplingApproachSystematic = "SYSTEMATIC_MUS"
	SamplingApproachJudgmental = "JUDGMENTAL"
)

type SampleDesign struct {
	DesignID              string             `json:"design_id"`
	TenantID              string             `json:"tenant_id"`
	PopulationID          string             `json:"population_id"`
	Objective             string             `json:"objective"`
	ParamSetID            string             `json:"param_set_id"`
	SampleSize            int                `json:"sample_size"`
	Status                SampleDesignStatus `json:"status"`
	ApprovedByPrincipalID *string            `json:"approved_by_principal_id,omitempty"`
	CreatedByPrincipalID  string             `json:"created_by_principal_id"`
	CreatedAt             time.Time          `json:"created_at"`
}

// SampleSelection pins the exact reproducibility inputs at selection
// time: the param set's version and the population's own digest, both
// captured verbatim so ReproduceSelection never depends on either
// changing later.
type SampleSelection struct {
	SelectionID            string    `json:"selection_id"`
	DesignID               string    `json:"design_id"`
	TenantID               string    `json:"tenant_id"`
	Method                 string    `json:"method"`
	RNGSeed                string    `json:"rng_seed"`
	IntervalSize           *float64  `json:"interval_size,omitempty"`
	StartPoint             *float64  `json:"start_point,omitempty"`
	ParamSetID             string    `json:"param_set_id"`
	ParamSetVersion        int       `json:"param_set_version"`
	PopulationDigestSHA256 string    `json:"population_digest_sha256"`
	SelectedAt             time.Time `json:"selected_at"`
}

const SamplingMethodSystematicInterval = "SYSTEMATIC_INTERVAL"

type SampleItemStatus string

const (
	SampleItemSelected             SampleItemStatus = "SELECTED"
	SampleItemTested               SampleItemStatus = "TESTED"
	SampleItemNonresponse          SampleItemStatus = "NONRESPONSE"
	SampleItemAlternativeProcedure SampleItemStatus = "ALTERNATIVE_PROCEDURE"
)

type SampleItem struct {
	ItemID          string           `json:"item_id"`
	SelectionID     string           `json:"selection_id"`
	TenantID        string           `json:"tenant_id"`
	PopulationRowID string           `json:"population_row_id"`
	IsKeyItem       bool             `json:"is_key_item"`
	Status          SampleItemStatus `json:"status"`
}

const (
	SampleExecutionResult               = "RESULT"
	SampleExecutionNonresponse          = "NONRESPONSE"
	SampleExecutionAlternativeProcedure = "ALTERNATIVE_PROCEDURE"
)

type SampleExecution struct {
	ExecutionID      string    `json:"execution_id"`
	ItemID           string    `json:"item_id"`
	TenantID         string    `json:"tenant_id"`
	Action           string    `json:"action"`
	ObjectiveTested  string    `json:"objective_tested"`
	Result           *string   `json:"result,omitempty"`
	ExceptionAmount  *float64  `json:"exception_amount,omitempty"`
	ActorPrincipalID string    `json:"actor_principal_id"`
	CreatedAt        time.Time `json:"created_at"`
}

type SampleEvaluation struct {
	EvaluationID          string    `json:"evaluation_id"`
	DesignID              string    `json:"design_id"`
	TenantID              string    `json:"tenant_id"`
	ProjectedMisstatement *float64  `json:"projected_misstatement,omitempty"`
	KnownExceptionCount   int       `json:"known_exception_count"`
	CompletenessOK        bool      `json:"completeness_ok"`
	EvaluatedAt           time.Time `json:"evaluated_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateSamplingParameterSetParams struct {
	TenantID, Approach                                                             string
	TolerableMisstatement, ExpectedMisstatement, ConfidenceLevel, KeyItemThreshold *float64
}

type CreateSampleDesignParams struct {
	TenantID, PopulationID, Objective, ParamSetID, CreatedByPrincipalID, CorrelationID string
	SampleSize                                                                         int
}

type ApproveSampleDesignParams struct {
	DesignID, TenantID, ActorPrincipalID, CorrelationID string
}

type SelectSampleParams struct {
	DesignID, TenantID, CorrelationID string
}

type RecordItemResultParams struct {
	ItemID, TenantID, ActorPrincipalID, ObjectiveTested, Result, CorrelationID string
	ExceptionAmount                                                            *float64
}

type RecordNonresponseParams struct {
	ItemID, TenantID, ActorPrincipalID, CorrelationID string
}

type AddAlternativeProcedureParams struct {
	ItemID, TenantID, ActorPrincipalID, ObjectiveTested, Result, CorrelationID string
}

type EvaluateSampleParams struct {
	DesignID, TenantID, CorrelationID string
}

// SupersedeSampleParams is AUD-04's own AUD-NEG-011 mechanism: since
// sampling_parameter_sets is fully immutable (no in-place revision path
// exists anywhere in this store), the only way to change parameters after
// selection is to create a brand-new design against a brand-new parameter
// set and explicitly supersede the old one — never a silent drift.
type SupersedeSampleParams struct {
	DesignID, TenantID, Reason, NewParamSetID, ActorPrincipalID, CorrelationID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrSampleDesignNotFound       = errors.New("sample design not found")
	ErrSampleDesignInvalidState   = errors.New("sample design is not in a state that permits this action")
	ErrSampleItemNotFound         = errors.New("sample item not found")
	ErrSampleItemInvalidState     = errors.New("sample item is not in a state that permits this action")
	ErrSampleSelectionInvalidated = errors.New("sample selection is invalidated — its parameter set has changed since selection; supersede the design")
	ErrObjectiveMismatch          = errors.New("execution objective does not match the sample design's own objective")
	ErrSampleEvaluationIncomplete = errors.New("sample evaluation is incomplete — one or more selected items lack a terminal result")
)
