// Package domain holds search-indexer-svc's canonical entities, state
// machines and reason codes, as specified by ZS-SVC-AB-001 §2 and §11.3.
//
// This service implements ESR-01 through ESR-05 in one process. The spec
// describes five canonical services; the estate runs one binary per bounded
// context, and search's five concerns share one control-plane database and one
// engine connection, so splitting them into five deployables would mean five
// copies of the same contract registry with nothing to gain. The BOUNDARIES
// are kept — each concern is its own package, and nothing in the query path
// writes a projection — so the split remains available later without a
// redesign.
package domain

import (
	"errors"
	"time"
)

// ── §11.3 stable reason codes ────────────────────────────────────────────────
//
// These are a wire contract, not log strings. A caller branches on them, so
// renaming one is a breaking change and inventing one outside this list means a
// caller sees a code it cannot handle. The full ESR-001..ESR-020 set is present
// even where a code is only reachable from one route, because a partial
// taxonomy is the thing that makes callers pattern-match on message text.
type ReasonCode string

const (
	ReasonTenantContextMissing    ReasonCode = "ESR-001" // TENANT_CONTEXT_MISSING
	ReasonScopeNotRegistered      ReasonCode = "ESR-002" // SCOPE_NOT_REGISTERED
	ReasonQueryOperatorForbidden  ReasonCode = "ESR-003" // QUERY_OPERATOR_FORBIDDEN
	ReasonQueryComplexityExceeded ReasonCode = "ESR-004" // QUERY_COMPLEXITY_EXCEEDED
	ReasonFieldNotSearchable      ReasonCode = "ESR-005" // FIELD_NOT_SEARCHABLE
	ReasonFieldNotReturnable      ReasonCode = "ESR-006" // FIELD_NOT_RETURNABLE
	ReasonPartitionNotAuthorized  ReasonCode = "ESR-007" // PARTITION_NOT_AUTHORIZED
	ReasonAuthorizationIndet      ReasonCode = "ESR-008" // AUTHORIZATION_INDETERMINATE
	ReasonPrivacyPurposeBlocked   ReasonCode = "ESR-009" // PRIVACY_PURPOSE_BLOCKED
	ReasonSourceSuppressed        ReasonCode = "ESR-010" // SOURCE_RESOURCE_SUPPRESSED
	ReasonGenerationNotActive     ReasonCode = "ESR-011" // INDEX_GENERATION_NOT_ACTIVE
	ReasonIndexStaleForScope      ReasonCode = "ESR-012" // INDEX_STALE_FOR_SCOPE
	ReasonRestrictionEpochMismtch ReasonCode = "ESR-013" // RESTRICTION_EPOCH_MISMATCH
	ReasonSourceHydrationFailed   ReasonCode = "ESR-014" // SOURCE_HYDRATION_FAILED
	ReasonResultWindowExceeded    ReasonCode = "ESR-015" // RESULT_WINDOW_EXCEEDED
	ReasonExportAuthzRequired     ReasonCode = "ESR-016" // EXPORT_AUTHORIZATION_REQUIRED
	ReasonReindexValidationFailed ReasonCode = "ESR-017" // REINDEX_VALIDATION_FAILED
	ReasonRestrictionPropFailed   ReasonCode = "ESR-018" // RESTRICTION_PROPAGATION_FAILED
	ReasonSemanticModelMismatch   ReasonCode = "ESR-019" // SEMANTIC_MODEL_MISMATCH
	ReasonSearchDegradedPartial   ReasonCode = "ESR-020" // SEARCH_DEGRADED_PARTIAL
)

// ReasonMeaning is the canonical meaning per §11.3, returned alongside the code
// so a caller never has to carry the table.
func ReasonMeaning(c ReasonCode) string {
	switch c {
	case ReasonTenantContextMissing:
		return "TENANT_CONTEXT_MISSING"
	case ReasonScopeNotRegistered:
		return "SCOPE_NOT_REGISTERED"
	case ReasonQueryOperatorForbidden:
		return "QUERY_OPERATOR_FORBIDDEN"
	case ReasonQueryComplexityExceeded:
		return "QUERY_COMPLEXITY_EXCEEDED"
	case ReasonFieldNotSearchable:
		return "FIELD_NOT_SEARCHABLE"
	case ReasonFieldNotReturnable:
		return "FIELD_NOT_RETURNABLE"
	case ReasonPartitionNotAuthorized:
		return "PARTITION_NOT_AUTHORIZED"
	case ReasonAuthorizationIndet:
		return "AUTHORIZATION_INDETERMINATE"
	case ReasonPrivacyPurposeBlocked:
		return "PRIVACY_PURPOSE_BLOCKED"
	case ReasonSourceSuppressed:
		return "SOURCE_RESOURCE_SUPPRESSED"
	case ReasonGenerationNotActive:
		return "INDEX_GENERATION_NOT_ACTIVE"
	case ReasonIndexStaleForScope:
		return "INDEX_STALE_FOR_SCOPE"
	case ReasonRestrictionEpochMismtch:
		return "RESTRICTION_EPOCH_MISMATCH"
	case ReasonSourceHydrationFailed:
		return "SOURCE_HYDRATION_FAILED"
	case ReasonResultWindowExceeded:
		return "RESULT_WINDOW_EXCEEDED"
	case ReasonExportAuthzRequired:
		return "EXPORT_AUTHORIZATION_REQUIRED"
	case ReasonReindexValidationFailed:
		return "REINDEX_VALIDATION_FAILED"
	case ReasonRestrictionPropFailed:
		return "RESTRICTION_PROPAGATION_FAILED"
	case ReasonSemanticModelMismatch:
		return "SEMANTIC_MODEL_MISMATCH"
	case ReasonSearchDegradedPartial:
		return "SEARCH_DEGRADED_PARTIAL"
	default:
		return "UNKNOWN"
	}
}

// ── §2.2 orthogonal state dimensions ─────────────────────────────────────────

// ContractState is §2.2's contract lifecycle. Only PUBLISHED contracts may
// build production generations.
type ContractState string

const (
	ContractDraft     ContractState = "DRAFT"
	ContractCertified ContractState = "CERTIFIED"
	ContractPublished ContractState = "PUBLISHED"
	ContractRetired   ContractState = "RETIRED"
)

// ContractTransitions is the whole legal state graph. A map rather than a
// switch so the handler, the store and the tests all read the same one: three
// copies of a transition table is how the fourth gets it wrong.
var ContractTransitions = map[ContractState][]ContractState{
	ContractDraft:     {ContractCertified, ContractRetired},
	ContractCertified: {ContractPublished, ContractDraft, ContractRetired},
	ContractPublished: {ContractRetired},
	ContractRetired:   {},
}

func (s ContractState) CanTransitionTo(next ContractState) bool {
	for _, allowed := range ContractTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// GenerationState is §8.1's generation lifecycle.
//
// READY and ACTIVE are separate states, which is the whole point of the
// table: "ACTIVE is separate from READY; cutover requires explicit
// validation." Collapsing them is how NP-42 ("alias switched before validation
// completes") happens.
type GenerationState string

const (
	GenerationPlanned    GenerationState = "PLANNED"
	GenerationBuilding   GenerationState = "BUILDING"
	GenerationValidating GenerationState = "VALIDATING"
	GenerationReady      GenerationState = "READY"
	GenerationActive     GenerationState = "ACTIVE"
	GenerationFailed     GenerationState = "FAILED"
	GenerationRetired    GenerationState = "RETIRED"
)

// GenerationTransitions encodes §8.1's entry gates.
//
// Note what is absent: nothing reaches ACTIVE except from READY. A FAILED
// generation goes nowhere — §8.1 says "rebuild new generation; never patch
// into ACTIVE authority silently", so the only way out is a new generation id.
var GenerationTransitions = map[GenerationState][]GenerationState{
	GenerationPlanned:    {GenerationBuilding, GenerationFailed},
	GenerationBuilding:   {GenerationValidating, GenerationFailed},
	GenerationValidating: {GenerationReady, GenerationFailed},
	GenerationReady:      {GenerationActive, GenerationFailed, GenerationRetired},
	GenerationActive:     {GenerationRetired, GenerationFailed},
	GenerationFailed:     {},
	GenerationRetired:    {},
}

func (s GenerationState) CanTransitionTo(next GenerationState) bool {
	for _, allowed := range GenerationTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Freshness is §2.2's source-freshness dimension. UNKNOWN is never represented
// as CURRENT — the one rule the table states explicitly, and the reason
// FreshnessUnknown is the zero value here rather than FreshnessCurrent.
type Freshness string

const (
	FreshnessUnknown Freshness = "UNKNOWN"
	FreshnessCurrent Freshness = "CURRENT"
	FreshnessLagging Freshness = "LAGGING"
	FreshnessStale   Freshness = "STALE"
)

// RetrievalOutcome is §2.2's retrieval-authorization dimension. INDETERMINATE
// fails closed for protected content (INV-07).
type RetrievalOutcome string

const (
	RetrievalPermit        RetrievalOutcome = "PERMIT"
	RetrievalSuppress      RetrievalOutcome = "SUPPRESS"
	RetrievalReviewReq     RetrievalOutcome = "REVIEW_REQUIRED"
	RetrievalIndeterminate RetrievalOutcome = "INDETERMINATE"
)

// PropagationState is §2.2's restriction-propagation dimension. APPLIED is not
// VERIFIED until search visibility has actually been tested — the distinction
// NP-59 and NP-60 both turn on.
type PropagationState string

const (
	PropagationPending  PropagationState = "PENDING"
	PropagationApplied  PropagationState = "APPLIED"
	PropagationVerified PropagationState = "VERIFIED"
	PropagationFailed   PropagationState = "FAILED"
)

// Completeness is §2.2's search-completeness dimension. "Partial results must
// be surfaced as partial, not silently presented as exhaustive."
type Completeness string

const (
	CompletenessComplete Completeness = "COMPLETE"
	CompletenessPartial  Completeness = "PARTIAL"
	CompletenessDegraded Completeness = "DEGRADED"
	CompletenessUnknown  Completeness = "UNKNOWN"
)

// RetrievalClass is §7.1's R0–R3 ladder. It decides how much work a hit costs
// before its content may be shown.
type RetrievalClass string

const (
	// RetrievalR0 returns pre-approved minimal metadata with no source
	// hydration, provided the restriction epoch is current.
	RetrievalR0 RetrievalClass = "R0"
	// RetrievalR1 re-authorizes the candidate against current resource
	// authority before any metadata or snippet is returned.
	RetrievalR1 RetrievalClass = "R1"
	// RetrievalR2 additionally hydrates the current source object: index
	// content is not trusted as a current display value.
	RetrievalR2 RetrievalClass = "R2"
	// RetrievalR3 is an explicit export, with its own authorization, purpose
	// and evidence. Never implied by search access (INV-29).
	RetrievalR3 RetrievalClass = "R3"
)

// RequiresReauthorization reports whether a class must consult current
// authority before returning anything at all.
func (c RetrievalClass) RequiresReauthorization() bool {
	return c == RetrievalR1 || c == RetrievalR2 || c == RetrievalR3
}

// RequiresHydration reports whether index content may be shown directly or
// must be replaced by the current source object.
func (c RetrievalClass) RequiresHydration() bool {
	return c == RetrievalR2 || c == RetrievalR3
}

// SensitivityClass is §4.1's per-field and per-document classification.
type SensitivityClass string

const (
	SensitivityPublic     SensitivityClass = "PUBLIC"
	SensitivityInternal   SensitivityClass = "INTERNAL"
	SensitivityPersonal   SensitivityClass = "PERSONAL"
	SensitivityFinancial  SensitivityClass = "FINANCIAL"
	SensitivityHR         SensitivityClass = "HR"
	SensitivityLegal      SensitivityClass = "LEGAL_PRIVILEGED"
	SensitivityRestricted SensitivityClass = "RESTRICTED"
	// SensitivitySecretProhibited marks a field that must NEVER be indexed.
	// INV-09: "secrets, private keys, raw payment credentials and equivalent
	// prohibited fields never enter search indexes." A contract that declares
	// one is refused at registration; a projection carrying one is quarantined.
	SensitivitySecretProhibited SensitivityClass = "SECRET_PROHIBITED"
)

// ── §2.1 core entities ───────────────────────────────────────────────────────

// SearchSource is a registered authoritative source eligible for indexing.
type SearchSource struct {
	SourceID        string `json:"source_id"`
	OwnerService    string `json:"owner_service"`
	SourceType      string `json:"source_type"`
	TenantScope     string `json:"tenant_scope"`
	ResidencyRegion string `json:"residency_region"`
	// SensitivityCeiling caps every field in every contract over this source.
	// A contract cannot register a field more sensitive than its source's
	// ceiling, so raising exposure needs a source-level decision rather than a
	// contract-level one.
	SensitivityCeiling SensitivityClass `json:"sensitivity_ceiling"`
	// EventTopic and EventTypes are the durable ingestion contract (§5.1).
	// Without both, a source is registered but nothing will ever reach it.
	EventTopic string   `json:"event_topic"`
	EventTypes []string `json:"event_types"`
	// RestrictionEventTypes is the priority lane (§8.2): visibility-reducing
	// events outrank normal indexing backlog.
	RestrictionEventTypes []string  `json:"restriction_event_types"`
	FreshnessClass        string    `json:"freshness_class"`
	MaxLagSeconds         int       `json:"max_lag_seconds"`
	CreatedAt             time.Time `json:"created_at"`
	CreatedByPrincipalID  string    `json:"created_by_principal_id"`
}

// SearchFieldDefinition is one field-level search/exposure contract (§4.1).
type SearchFieldDefinition struct {
	FieldID          string           `json:"field_id"`
	SourcePath       string           `json:"source_path"`
	Type             string           `json:"type"`
	Searchable       bool             `json:"searchable"`
	Filterable       bool             `json:"filterable"`
	Facetable        bool             `json:"facetable"`
	Sortable         bool             `json:"sortable"`
	SnippetAllowed   bool             `json:"snippet_allowed"`
	Returnable       bool             `json:"returnable"`
	Exportable       bool             `json:"exportable"`
	SensitivityClass SensitivityClass `json:"sensitivity_class"`
	AnalyzerProfile  string           `json:"analyzer_profile"`
}

// IndexContract is an immutable composition of source, field, analyzer and
// partition rules (§2.1). Immutable after publication — a change is a new
// version, which is what makes a generation reproducible (§4.3).
type IndexContract struct {
	ContractID           string                  `json:"contract_id"`
	SourceID             string                  `json:"source_id"`
	ScopeName            string                  `json:"scope_name"`
	Version              int                     `json:"version"`
	SchemaDigest         string                  `json:"schema_digest"`
	State                ContractState           `json:"publication_state"`
	FreshnessClass       string                  `json:"freshness_class"`
	RetrievalClass       RetrievalClass          `json:"retrieval_class"`
	AnalyzerProfile      string                  `json:"analyzer_profile"`
	AuthzAction          string                  `json:"authz_action"`
	Fields               []SearchFieldDefinition `json:"fields"`
	CreatedAt            time.Time               `json:"created_at"`
	PublishedAt          *time.Time              `json:"published_at,omitempty"`
	CreatedByPrincipalID string                  `json:"created_by_principal_id"`
}

// IndexGeneration is a concrete searchable index version (§2.1).
type IndexGeneration struct {
	GenerationID    string          `json:"generation_id"`
	ContractID      string          `json:"contract_id"`
	ContractVersion int             `json:"contract_version"`
	ScopeName       string          `json:"scope_name"`
	PhysicalIndex   string          `json:"engine_ref"`
	State           GenerationState `json:"validation_state"`
	BuildFrom       *time.Time      `json:"build_from,omitempty"`
	// ValidationDigest pins what was actually checked before activation —
	// TC-09: "every reindex activation traces to validation digest".
	ValidationDigest     string     `json:"validation_digest,omitempty"`
	ValidationNote       string     `json:"validation_note,omitempty"`
	ActivatedAt          *time.Time `json:"activated_at,omitempty"`
	RetiredAt            *time.Time `json:"retired_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
}

// IndexCheckpoint is source-to-index progress evidence (§2.1, §5.3).
type IndexCheckpoint struct {
	ScopeName         string    `json:"scope_name"`
	SourcePartition   string    `json:"source_partition"`
	Watermark         int64     `json:"watermark"`
	CommittedAt       time.Time `json:"committed_at"`
	ObservedAt        time.Time `json:"observed_at"`
	LagMS             int64     `json:"lag_ms"`
	Freshness         Freshness `json:"freshness"`
	IndexedLive       int64     `json:"indexed_live"`
	IndexedTombstoned int64     `json:"indexed_tombstoned"`
}

// RestrictionTombstone is a priority visibility-removal marker (§2.1).
//
// Epoch is the monotonic counter that makes ordering safe. NP-48: "restriction
// events arrive out of order → monotonic epoch/version prevents visibility
// resurrection."
type RestrictionTombstone struct {
	TenantID      string           `json:"tenant_id"`
	SourceType    string           `json:"source_type"`
	SourceID      string           `json:"source_id"`
	ScopeName     string           `json:"scope_name"`
	Reason        string           `json:"reason"`
	Epoch         int64            `json:"epoch"`
	SourceEventID string           `json:"source_event_id"`
	EffectiveAt   time.Time        `json:"effective_at"`
	PropagatedAt  *time.Time       `json:"propagated_at,omitempty"`
	VerifiedAt    *time.Time       `json:"verified_at,omitempty"`
	State         PropagationState `json:"state"`
	FailureReason string           `json:"failure_reason,omitempty"`
}

// SearchEvidence is replay/audit evidence for one executed search (§2.1, TC-10).
type SearchEvidence struct {
	EvidenceID    string `json:"evidence_id"`
	RequestID     string `json:"request_id"`
	CorrelationID string `json:"correlation_id"`
	TenantID      string `json:"tenant_id"`
	ActorID       string `json:"actor_id"`
	WorkloadID    string `json:"workload_id,omitempty"`
	OnBehalfOfID  string `json:"on_behalf_of_principal_id,omitempty"`
	Purpose       string `json:"purpose_context"`
	ScopeName     string `json:"scope_name"`
	// QueryDigest, never query text. INV-17: "query logs never store
	// prohibited sensitive query text where a digest/classification is
	// sufficient", and §9.2 is explicit that a query IS personal data when it
	// can reveal a person, condition, employment matter or legal issue.
	QueryDigest     string       `json:"query_digest"`
	FiltersDigest   string       `json:"mandatory_filters_digest"`
	PlanDigest      string       `json:"plan_digest"`
	IndexGeneration string       `json:"index_generation"`
	PartitionSet    []string     `json:"partition_set"`
	ComplexityScore int          `json:"complexity_score"`
	ResultCount     int          `json:"result_count"`
	SuppressedCount int          `json:"suppressed_count"`
	Completeness    Completeness `json:"completeness_state"`
	ReasonCodes     []string     `json:"reason_codes,omitempty"`
	DurationMS      int64        `json:"duration_ms"`
	TraceID         string       `json:"trace_id,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
}

// RetrievalDecision is one candidate's current authorization outcome (§2.1).
type RetrievalDecision struct {
	SourceType string           `json:"source_type"`
	SourceID   string           `json:"source_id"`
	Outcome    RetrievalOutcome `json:"outcome"`
	ReasonCode ReasonCode       `json:"reason_code,omitempty"`
	PolicyRef  string           `json:"policy_ref,omitempty"`
	DecidedAt  time.Time        `json:"decided_at"`
}

// ProjectionRecord is the control-plane ledger row for one indexed projection.
//
// A deliberate second copy of what OpenSearch already holds. The engine is a
// disposable projection (§8.3) and cannot be trusted as the record of what was
// indexed and when, which is what §5.3's completeness certification compares
// against; keeping the ledger in Postgres means a rebuilt index can be checked
// against something that did not get rebuilt with it.
type ProjectionRecord struct {
	TenantID         string    `json:"tenant_id"`
	ScopeName        string    `json:"scope_name"`
	SourceType       string    `json:"source_type"`
	SourceID         string    `json:"source_id"`
	SourceVersion    int64     `json:"source_version"`
	RestrictionEpoch int64     `json:"restriction_epoch"`
	ContentHash      string    `json:"content_hash"`
	Tombstoned       bool      `json:"tombstoned"`
	LastEventID      string    `json:"last_event_id"`
	IndexedAt        time.Time `json:"indexed_at"`
}

// ── Errors ───────────────────────────────────────────────────────────────────

var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrInvalidState    = errors.New("invalid state transition")
	ErrImmutable       = errors.New("published contract is immutable")
	ErrProhibitedField = errors.New("prohibited field class may not be indexed")
	ErrReservedField   = errors.New("reserved governance field may not be registered")
	ErrStaleEpoch      = errors.New("restriction epoch is older than the one already applied")
	ErrTenantRequired  = errors.New("verified tenant context is required")
)
