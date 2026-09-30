// Package domain defines the authoritative domain types for
// semantic-model-svc (DATA-05, ZS-SVC-N-001 §4). This service defines
// governed reusable metrics, dimensions, joins and calculation plans over
// analytical data (DATA-04 dataset versions) — it never changes FIN-05
// KPI authority, accounting/tax rules, or grants raw SQL access to any
// source-of-record: a metric/dimension binding's source_column is always
// a plain column reference, validated against a strict allowlist, never
// an arbitrary SQL fragment.
package domain

import (
	"fmt"
	"regexp"
	"time"
)

const (
	PrefixModel     = "dsm_"
	PrefixVersion   = "dsv_"
	PrefixMetric    = "dmb_"
	PrefixDimension = "ddb_"
	PrefixCalcPlan  = "dcp_"
)

type errorString string

func (e errorString) Error() string { return string(e) }

type IdempotentReplayError struct {
	ResourceID string
}

func (e *IdempotentReplayError) Error() string {
	return fmt.Sprintf("idempotent replay: resource %s already exists", e.ResourceID)
}

type IdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

const SellerScope = "seller"

// ── SemanticModel / SemanticVersion ─────────────────────────────────────────

type SemanticModel struct {
	ModelID   string    `json:"model_id"`
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

type SemanticVersionStatus string

const (
	SemVerDraft      SemanticVersionStatus = "Draft"
	SemVerValidating SemanticVersionStatus = "Validating"
	SemVerPublished  SemanticVersionStatus = "Published"
	SemVerDeprecated SemanticVersionStatus = "Deprecated"
)

// SemanticVersion is one versioned build of a semantic model's metrics/
// dimensions/calculation plans. Published versions are immutable —
// changing a dimension hierarchy or metric definition always creates a
// NEW version, never rewrites the one already live (the doc's own named
// acceptance test).
type SemanticVersion struct {
	VersionID     string                `json:"version_id"`
	TenantID      string                `json:"tenant_id"`
	ModelID       string                `json:"model_id"`
	VersionNumber int                   `json:"version_number"`
	Status        SemanticVersionStatus `json:"status"`
	CreatedAt     time.Time             `json:"created_at"`
	CreatedBy     string                `json:"created_by"`
	PublishedAt   *time.Time            `json:"published_at,omitempty"`
	PublishedBy   *string               `json:"published_by,omitempty"`
}

type CreateDraftVersionRequest struct {
	ModelName string `json:"model_name"`
}

func (r CreateDraftVersionRequest) Validate() error {
	if r.ModelName == "" {
		return fmt.Errorf("model_name is required")
	}
	return nil
}

// ── MetricBinding ────────────────────────────────────────────────────────────

type AggregationMethod string

const (
	AggSum   AggregationMethod = "SUM"
	AggAvg   AggregationMethod = "AVG"
	AggCount AggregationMethod = "COUNT"
	AggMin   AggregationMethod = "MIN"
	AggMax   AggregationMethod = "MAX"
)

func (a AggregationMethod) valid() bool {
	switch a {
	case AggSum, AggAvg, AggCount, AggMin, AggMax:
		return true
	}
	return false
}

type MetricSign string

const (
	SignPositive MetricSign = "Positive"
	SignNegative MetricSign = "Negative"
	SignSigned   MetricSign = "Signed"
)

func (s MetricSign) valid() bool {
	switch s {
	case SignPositive, SignNegative, SignSigned:
		return true
	}
	return false
}

// sourceColumnPattern is the allowlist for MetricBinding/DimensionBinding
// source_column: a plain, possibly dotted, identifier — never arbitrary
// SQL. This is the actual enforcement point for the doc's non-ownership
// boundary ("raw SQL access to source-of-record") and its own named
// negative test ("unauthorized raw SQL/source-store binding is
// rejected").
var sourceColumnPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)?$`)

func ValidateSourceColumn(col string) error {
	if col == "" {
		return fmt.Errorf("source_column is required")
	}
	if !sourceColumnPattern.MatchString(col) {
		return fmt.Errorf("source_column %q is not a plain column reference (no raw SQL/expressions permitted)", col)
	}
	return nil
}

// MetricBinding binds a metric_key to a DATA-04 dataset version's column,
// with explicit unit/currency/sign/aggregation semantics — the doc's own
// "Core controls" line: "units/currency/sign/aggregation are explicit."
type MetricBinding struct {
	MetricBindingID  string            `json:"metric_binding_id"`
	TenantID         string            `json:"tenant_id"`
	VersionID        string            `json:"version_id"`
	MetricKey        string            `json:"metric_key"`
	DatasetVersionID string            `json:"dataset_version_id"`
	SourceColumn     string            `json:"source_column"`
	Aggregation      AggregationMethod `json:"aggregation"`
	Unit             string            `json:"unit"`
	Currency         *string           `json:"currency,omitempty"`
	Sign             MetricSign        `json:"sign"`
	Retired          bool              `json:"retired"`
	CreatedAt        time.Time         `json:"created_at"`
	CreatedBy        string            `json:"created_by"`
}

type AddMetricBindingRequest struct {
	VersionID        string            `json:"version_id"`
	MetricKey        string            `json:"metric_key"`
	DatasetVersionID string            `json:"dataset_version_id"`
	SourceColumn     string            `json:"source_column"`
	Aggregation      AggregationMethod `json:"aggregation"`
	Unit             string            `json:"unit"`
	Currency         *string           `json:"currency,omitempty"`
	Sign             MetricSign        `json:"sign"`
}

func (r AddMetricBindingRequest) Validate() error {
	if r.VersionID == "" {
		return fmt.Errorf("version_id is required")
	}
	if r.MetricKey == "" {
		return fmt.Errorf("metric_key is required")
	}
	if r.DatasetVersionID == "" {
		return fmt.Errorf("dataset_version_id is required")
	}
	if err := ValidateSourceColumn(r.SourceColumn); err != nil {
		return err
	}
	if !r.Aggregation.valid() {
		return fmt.Errorf("aggregation %q is not a recognized method", r.Aggregation)
	}
	if r.Unit == "" {
		return fmt.Errorf("unit is required")
	}
	if !r.Sign.valid() {
		return fmt.Errorf("sign %q is not a recognized sign convention", r.Sign)
	}
	return nil
}

// ── DimensionBinding ─────────────────────────────────────────────────────────

type DimensionBinding struct {
	DimensionBindingID string    `json:"dimension_binding_id"`
	TenantID           string    `json:"tenant_id"`
	VersionID          string    `json:"version_id"`
	DimensionKey       string    `json:"dimension_key"`
	DatasetVersionID   string    `json:"dataset_version_id"`
	SourceColumn       string    `json:"source_column"`
	HierarchyLevel     int       `json:"hierarchy_level"`
	ParentDimensionKey *string   `json:"parent_dimension_key,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	CreatedBy          string    `json:"created_by"`
}

type AddDimensionBindingRequest struct {
	VersionID          string  `json:"version_id"`
	DimensionKey       string  `json:"dimension_key"`
	DatasetVersionID   string  `json:"dataset_version_id"`
	SourceColumn       string  `json:"source_column"`
	HierarchyLevel     int     `json:"hierarchy_level"`
	ParentDimensionKey *string `json:"parent_dimension_key,omitempty"`
}

func (r AddDimensionBindingRequest) Validate() error {
	if r.VersionID == "" {
		return fmt.Errorf("version_id is required")
	}
	if r.DimensionKey == "" {
		return fmt.Errorf("dimension_key is required")
	}
	if r.DatasetVersionID == "" {
		return fmt.Errorf("dataset_version_id is required")
	}
	if err := ValidateSourceColumn(r.SourceColumn); err != nil {
		return err
	}
	if r.HierarchyLevel < 0 {
		return fmt.Errorf("hierarchy_level must not be negative")
	}
	return nil
}

// ── CalculationPlan ──────────────────────────────────────────────────────────

// CalculationPlan describes how a derived metric combines base metrics —
// a structured expression, never raw SQL. Op is one of a small closed set;
// Left/Right (or Operands for n-ary ops) reference OTHER metric_keys
// within the same version, resolved and checked by ValidateCalculationPlan.
type CalculationPlan struct {
	CalcPlanID string         `json:"calc_plan_id"`
	TenantID   string         `json:"tenant_id"`
	VersionID  string         `json:"version_id"`
	MetricKey  string         `json:"metric_key"`
	Expression PlanExpression `json:"expression"`
	CreatedAt  time.Time      `json:"created_at"`
	CreatedBy  string         `json:"created_by"`
}

type PlanExpression struct {
	Op       string   `json:"op"`       // "add" | "subtract" | "multiply" | "divide" | "ratio"
	Operands []string `json:"operands"` // referenced metric_keys, in order
}

func (e PlanExpression) Validate() error {
	switch e.Op {
	case "add", "subtract", "multiply", "divide", "ratio":
	default:
		return fmt.Errorf("op %q is not a recognized calculation op", e.Op)
	}
	if len(e.Operands) < 2 {
		return fmt.Errorf("expression requires at least 2 operands")
	}
	return nil
}

type AddCalculationPlanRequest struct {
	VersionID  string         `json:"version_id"`
	MetricKey  string         `json:"metric_key"`
	Expression PlanExpression `json:"expression"`
}

func (r AddCalculationPlanRequest) Validate() error {
	if r.VersionID == "" {
		return fmt.Errorf("version_id is required")
	}
	if r.MetricKey == "" {
		return fmt.Errorf("metric_key is required")
	}
	return r.Expression.Validate()
}

// ValidationResult is ValidateCalculationPlan's read-only outcome — a
// dry-run check callable any time before publish, never mutating state.
type ValidationResult struct {
	MetricKey       string   `json:"metric_key"`
	Valid           bool     `json:"valid"`
	MissingOperands []string `json:"missing_operands,omitempty"`
}

var (
	ErrModelNotFound         = errorString("semantic model not found")
	ErrVersionNotFound       = errorString("semantic version not found")
	ErrMetricBindingNotFound = errorString("metric binding not found")
	ErrCalcPlanNotFound      = errorString("calculation plan not found")
	ErrVersionNotDraft       = errorString("semantic version must be Draft or Validating for this action")
	ErrVersionPublished      = errorString("semantic version is published and immutable; publish a new version instead")
	ErrVersionAlreadyRetired = errorString("metric binding is already retired")
	ErrNoBindings            = errorString("semantic version has no active metric bindings to publish")
	ErrMetricCollision       = errorString("conflicting metric semantics: the same metric_key is bound more than once with different unit/aggregation/sign")
	ErrCalcPlanInvalid       = errorString("calculation plan references a metric_key with no active binding in this version")
	ErrIdempotencyKeyReused  = errorString("idempotency key was already used for a different request")
	ErrImmutableViolation    = errorString("this record cannot be mutated in that way")
)
