// Package domain defines the canonical types for privacy-decision-svc —
// PRV-03, "Purpose Binding & Runtime Data-Use Decision Service", from
// ZS-SVC-W-001 §12/§13, §18, §32.
package domain

import (
	"encoding/json"
	"time"
)

// ProposedOperation is data only (the spec's own list, §12.1).
type ProposedOperation string

const (
	OperationCollect    ProposedOperation = "COLLECT"
	OperationAccess     ProposedOperation = "ACCESS"
	OperationUse        ProposedOperation = "USE"
	OperationCombine    ProposedOperation = "COMBINE"
	OperationInfer      ProposedOperation = "INFER"
	OperationDisclose   ProposedOperation = "DISCLOSE"
	OperationExport     ProposedOperation = "EXPORT"
	OperationTrainModel ProposedOperation = "TRAIN_MODEL"
	OperationProfile    ProposedOperation = "PROFILE"
	OperationRetain     ProposedOperation = "RETAIN"
	OperationDelete     ProposedOperation = "DELETE"
	OperationAnonymize  ProposedOperation = "ANONYMIZE"
)

func (o ProposedOperation) Valid() bool {
	switch o {
	case OperationCollect, OperationAccess, OperationUse, OperationCombine, OperationInfer,
		OperationDisclose, OperationExport, OperationTrainModel, OperationProfile,
		OperationRetain, OperationDelete, OperationAnonymize:
		return true
	}
	return false
}

// DecisionResult represents the 5 canonical outcomes from §12.2.
type DecisionResult string

const (
	ResultPermit         DecisionResult = "PERMIT"
	ResultRestrict       DecisionResult = "RESTRICT"
	ResultBlock          DecisionResult = "BLOCK"
	ResultReviewRequired DecisionResult = "REVIEW_REQUIRED"
	ResultIndeterminate  DecisionResult = "INDETERMINATE"
)

// §32 Stable Error and Reason Codes
const (
	PRV001PurposeNotRegistered          = "PRV-001: PURPOSE_NOT_REGISTERED"
	PRV002ProcessingActivityInactive    = "PRV-002: PROCESSING_ACTIVITY_INACTIVE"
	PRV003PrivacyRoleUnresolved         = "PRV-003: PRIVACY_ROLE_UNRESOLVED"
	PRV004JurisdictionUnresolved        = "PRV-004: JURISDICTION_UNRESOLVED"
	PRV005PolicyUnavailable             = "PRV-005: POLICY_UNAVAILABLE"
	PRV006ConsentRequiredMissing        = "PRV-006: CONSENT_REQUIRED_MISSING"
	PRV007ConsentWithdrawn              = "PRV-007: CONSENT_WITHDRAWN"
	PRV008NoticeVersionInvalid          = "PRV-008: NOTICE_VERSION_INVALID"
	PRV009PurposeIncompatible           = "PRV-009: PURPOSE_INCOMPATIBLE"
	PRV010DataCategoryRestricted        = "PRV-010: DATA_CATEGORY_RESTRICTED"
	PRV011MinimizationRequired          = "PRV-011: MINIMIZATION_REQUIRED"
	PRV012IdentityAssuranceInsufficient = "PRV-012: IDENTITY_ASSURANCE_INSUFFICIENT"
	PRV013ThirdPartyReviewRequired      = "PRV-013: THIRD_PARTY_REVIEW_REQUIRED"
	PRV014RetentionOrHoldBlock          = "PRV-014: RETENTION_OR_HOLD_BLOCK"
	PRV015TransferNotAuthorized         = "PRV-015: TRANSFER_NOT_AUTHORIZED"
	PRV016AssessmentRequired            = "PRV-016: ASSESSMENT_REQUIRED"
	PRV017ProcessorInstructionMissing   = "PRV-017: PROCESSOR_INSTRUCTION_MISSING"
	PRV018SubprocessorNotApproved       = "PRV-018: SUBPROCESSOR_NOT_APPROVED"
	PRV019PrivacyContextIndeterminate   = "PRV-019: PRIVACY_CONTEXT_INDETERMINATE"
	PRV020ImmutableEvidenceConflict     = "PRV-020: IMMUTABLE_EVIDENCE_CONFLICT"

	// Backwards-compatible aliases
	ReasonActivityNotActive         = PRV002ProcessingActivityInactive
	ReasonPurposeNotPublished       = PRV001PurposeNotRegistered
	ReasonPurposeNotBoundToActivity = PRV009PurposeIncompatible
	ReasonConsentNotGranted         = PRV006ConsentRequiredMissing
	ReasonLegalHoldBlocksUse        = PRV014RetentionOrHoldBlock
	ReasonDependencyUnavailable     = PRV019PrivacyContextIndeterminate
)

// SubjectContext captures §12.1 subject context dimensions.
type SubjectContext struct {
	SubjectRef   string `json:"subject_ref,omitempty"`
	SubjectClass string `json:"subject_class,omitempty"` // CUSTOMER, EMPLOYEE, PROSPECT, USER, VENDOR, MINOR
	AgeBand      string `json:"age_band,omitempty"`      // MINOR, ADULT, SENIOR, UNKNOWN
	Residency    string `json:"residency,omitempty"`     // Country/jurisdiction code, e.g. GB, EU, US-CA
	Jurisdiction string `json:"jurisdiction,omitempty"`  // Alias for residency
	Relationship string `json:"relationship,omitempty"`  // Relationship to tenant
}

// DataContext captures §12.1 data context dimensions.
type DataContext struct {
	DataCategories   []string `json:"data_categories,omitempty"`
	DataCategory     string   `json:"data_category,omitempty"`
	SensitivityFlags []string `json:"sensitivity_flags,omitempty"` // SENSITIVE, SPECIAL_CATEGORY, HEALTH, BIOMETRIC, CHILD_DATA, HIGH_RISK
	Source           string   `json:"source,omitempty"`            // DIRECT, DERIVED, THIRD_PARTY, PUBLIC
	Classification   string   `json:"classification,omitempty"`    // PUBLIC, INTERNAL, CONFIDENTIAL, RESTRICTED, ANONYMOUS
}

// RecipientContext captures §12.1 recipient and destination dimensions.
type RecipientContext struct {
	RecipientRef            string `json:"recipient_ref,omitempty"`
	RecipientType           string `json:"recipient_type,omitempty"` // INTERNAL, THIRD_PARTY, PROCESSOR, SUBPROCESSOR, AUTHORITY
	DestinationJurisdiction string `json:"destination_jurisdiction,omitempty"`
	TransferMechanismID     string `json:"transfer_mechanism_id,omitempty"`
}

// TransferCheckRequest specifies parameters for §12.1 / §13 step 4 transfer evaluation.
type TransferCheckRequest struct {
	RelationshipID          string `json:"relationship_id,omitempty"`
	TransferMechanismID     string `json:"transfer_mechanism_id,omitempty"`
	DestinationJurisdiction string `json:"destination_jurisdiction,omitempty"`
	AssessmentCheck         bool   `json:"assessment_check,omitempty"`
}

// DecisionConstraint represents a machine-enforceable output constraint for RESTRICT (§13.1 Rule 7).
type DecisionConstraint struct {
	Type        string                 `json:"type"` // FIELD_MINIMIZATION, REDACTION, RECIPIENT_LIMITATION, RETENTION_CONDITION, PURPOSE_LIMITATION
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// ConsentCheckRequest is opt-in, or triggered by activity dependency.
type ConsentCheckRequest struct {
	Required  bool   `json:"required"`
	ReceiptID string `json:"receipt_id,omitempty"`
}

// LegalHoldCheckRequest is opt-in and caller-supplied.
type LegalHoldCheckRequest struct {
	RecordClass string `json:"record_class"`
	EntityRef   string `json:"entity_ref,omitempty"`
}

// EvaluateDecisionRequest is the wire input to POST /privacy/decisions (§12.1).
type EvaluateDecisionRequest struct {
	TenantID                   string                 `json:"tenant_id,omitempty"`
	SubjectRef                 string                 `json:"subject_ref"`
	ProcessingActivityID       string                 `json:"processing_activity_id"`
	ActivityVersionID          string                 `json:"activity_version_id,omitempty"`
	PurposeID                  string                 `json:"purpose_id"`
	PurposeVersionID           string                 `json:"purpose_version_id,omitempty"`
	ProposedOperation          ProposedOperation      `json:"proposed_operation"`
	SubjectContext             *SubjectContext        `json:"subject_context,omitempty"`
	DataContext                *DataContext           `json:"data_context,omitempty"`
	SecondaryPurposeID         string                 `json:"secondary_purpose_id,omitempty"`
	ProposedSecondaryPurpose   string                 `json:"proposed_secondary_purpose,omitempty"`
	RecipientContext           *RecipientContext      `json:"recipient_context,omitempty"`
	ConsentCheck               *ConsentCheckRequest   `json:"consent_check,omitempty"`
	LegalHoldCheck             *LegalHoldCheckRequest `json:"legal_hold_check,omitempty"`
	TransferCheck              *TransferCheckRequest  `json:"transfer_check,omitempty"`
	DeidentificationControlRef string                 `json:"deidentification_control_ref,omitempty"`
}

// PrivacyDecision is the append-only evidence record for one evaluation — §13.2 "decision durability" & §18 canonical contract.
type PrivacyDecision struct {
	DecisionID           string               `json:"decision_id"`
	TenantID             *string              `json:"tenant_id,omitempty"`
	InputFingerprint     string               `json:"input_fingerprint"`
	ContextHash          string               `json:"context_hash"` // §18 alias for input_fingerprint
	SubjectRef           string               `json:"subject_ref"`
	SubjectContext       *SubjectContext      `json:"subject_context,omitempty"`
	DataContext          *DataContext         `json:"data_context,omitempty"`
	ProcessingActivityID string               `json:"processing_activity_id"`
	ActivityVersionID    *string              `json:"activity_version_id,omitempty"`
	PurposeID            string               `json:"purpose_id"`
	PurposeVersionID     *string              `json:"purpose_version_id,omitempty"`
	SecondaryPurposeID   *string              `json:"secondary_purpose_id,omitempty"`
	ProposedOperation    ProposedOperation    `json:"proposed_operation"`
	RecipientContext     *RecipientContext    `json:"recipient_context,omitempty"`
	Result               DecisionResult       `json:"result"`
	ReasonCodes          []string             `json:"reason_codes"`
	Constraints          []DecisionConstraint `json:"constraints"`
	PolicyRefs           []string             `json:"policy_refs"`          // §18
	EvidenceRef          string               `json:"evidence_ref"`         // §18
	ExpiresAt            *time.Time           `json:"expires_at,omitempty"` // §18
	ConsentReceiptID     *string              `json:"consent_receipt_id,omitempty"`
	NoticeVersionID      *string              `json:"notice_version_id,omitempty"`
	LegalHoldID          *string              `json:"legal_hold_id,omitempty"`
	TransferDecisionID   *string              `json:"transfer_decision_id,omitempty"`
	ActorPrincipalID     string               `json:"actor_principal_id"`
	CorrelationID        string               `json:"correlation_id,omitempty"`
	DecidedAt            time.Time            `json:"decided_at"`
}

// IdempotencyRecord stores idempotency execution state (§18.1).
type IdempotencyRecord struct {
	Key          string    `json:"idempotency_key"`
	TenantID     string    `json:"tenant_id"`
	PrincipalID  string    `json:"principal_id"`
	Endpoint     string    `json:"endpoint"`
	RequestHash  string    `json:"request_hash"`
	ResponseCode int       `json:"response_code"`
	ResponseBody []byte    `json:"response_body"`
	CreatedAt    time.Time `json:"created_at"`
}

// MarshalJSONB / UnmarshalJSONB helpers
func MarshalJSONB(v interface{}) []byte {
	if v == nil {
		return []byte("{}")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

func MarshalConstraints(constraints []DecisionConstraint) []byte {
	if constraints == nil {
		constraints = []DecisionConstraint{}
	}
	raw, _ := json.Marshal(constraints)
	return raw
}

func UnmarshalConstraints(raw []byte) []DecisionConstraint {
	var c []DecisionConstraint
	if len(raw) == 0 {
		return []DecisionConstraint{}
	}
	_ = json.Unmarshal(raw, &c)
	if c == nil {
		c = []DecisionConstraint{}
	}
	return c
}

func MarshalReasonCodes(codes []string) []byte {
	if codes == nil {
		codes = []string{}
	}
	raw, _ := json.Marshal(codes)
	return raw
}

func UnmarshalReasonCodes(raw []byte) []string {
	var codes []string
	if len(raw) == 0 {
		return []string{}
	}
	_ = json.Unmarshal(raw, &codes)
	if codes == nil {
		codes = []string{}
	}
	return codes
}

func MarshalPolicyRefs(refs []string) []byte {
	if refs == nil {
		refs = []string{}
	}
	raw, _ := json.Marshal(refs)
	return raw
}

func UnmarshalPolicyRefs(raw []byte) []string {
	var refs []string
	if len(raw) == 0 {
		return []string{}
	}
	_ = json.Unmarshal(raw, &refs)
	if refs == nil {
		refs = []string{}
	}
	return refs
}

// ── sentinel errors ──────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrDecisionNotFound    = errorString("privacy decision not found")
	ErrStoreUnavailable    = errorString("privacy-decision store unavailable")
	ErrIdempotencyConflict = errorString("PRV-020: IMMUTABLE_EVIDENCE_CONFLICT: idempotency key already used with different payload")
)
