// Package domain defines the canonical types for privacy-decision-svc —
// PRV-03, "Purpose Binding & Runtime Data-Use Decision Service", from
// ZS-SVC-W-001 §12/§13.
//
// v1 scope, documented rather than hidden (same doctrine as PRV-01/PRV-02):
//
//   - The full spec names FIVE outcomes: PERMIT, RESTRICT, BLOCK,
//     REVIEW_REQUIRED, INDETERMINATE. RESTRICT (machine-enforceable
//     minimization/redaction/recipient-limitation constraints) and
//     REVIEW_REQUIRED (routing to a qualified human/privacy reviewer) both
//     require a PDC (jurisdiction-specific lawful-basis/policy package)
//     rules engine that does not exist anywhere in this codebase — same
//     gap already documented in PRV-01's own package doc comment. Both
//     values are defined in the wire contract (so a future version can
//     start returning them without a breaking change), but THIS version
//     never produces them — only PERMIT, BLOCK and INDETERMINATE are
//     reachable. Inventing minimization logic to make RESTRICT reachable
//     would mean fabricating the exact business rules PDC is supposed to
//     own — the same defect shape this platform's fixing pass exists to
//     remove, not add.
//   - What v1 DOES check is real, not fabricated: (1) the processing
//     activity resolves to ACTIVE in PRV-01, (2) the purpose resolves to
//     PUBLISHED in PRV-01, (3) the proposed purpose_id is actually one of
//     that activity's registered purpose_ids (PRV-C01, purpose
//     limitation — enforced directly from PRV-01's own
//     ProcessingActivityVersion.PurposeIDs, not invented), (4) IF the
//     caller declares this use requires consent, the subject's consent
//     for that purpose resolves to GRANTED in PRV-02 (PRV-C04), and (5)
//     IF the caller supplies its own real retention record_class/
//     entity_ref, no active legal hold blocks the use (via
//     retention-registry-svc). Consent and legal-hold checks are
//     opt-in, caller-declared: this service does not infer whether a
//     purpose requires consent (PRV-01's lawful_basis_refs is documented
//     as opaque, not a signal this service is allowed to interpret), and
//     it does not invent a retention record_class the way evidence-
//     manifest-svc's finding warned against (see master-register-
//     findings-2026-08-27.md §2.3) — the calling service supplies its
//     own real one, since only it knows what it is.
//   - Every decision is recorded permanently (§13.2 "decision
//     durability") — decisions are append-only, never updated. Historical
//     reproduction of a past decision must not depend on today's PRV-01/
//     PRV-02/retention state, which is why the exact resolved version IDs
//     are captured on the decision row itself, not just the inputs.
package domain

import (
	"encoding/json"
	"time"
)

// ProposedOperation is data only (the spec's own list, §12.1) — new
// values are added via data, same doctrine as every enum-shaped string
// column in this platform.
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

// DecisionResult — see the package doc comment: RESTRICT and
// REVIEW_REQUIRED are defined but never produced in this version.
type DecisionResult string

const (
	ResultPermit         DecisionResult = "PERMIT"
	ResultRestrict       DecisionResult = "RESTRICT" // reserved, unreachable in v1
	ResultBlock          DecisionResult = "BLOCK"
	ResultReviewRequired DecisionResult = "REVIEW_REQUIRED" // reserved, unreachable in v1
	ResultIndeterminate  DecisionResult = "INDETERMINATE"
)

// Reason codes — data only. Named distinctly enough that a caller acting
// on BLOCK/INDETERMINATE can tell which real check failed, per §13.2's
// requirement that a decision record its reason codes.
const (
	ReasonActivityNotActive         = "ACTIVITY_NOT_ACTIVE"
	ReasonPurposeNotPublished       = "PURPOSE_NOT_PUBLISHED"
	ReasonPurposeNotBoundToActivity = "PURPOSE_NOT_BOUND_TO_ACTIVITY"
	ReasonConsentNotGranted         = "CONSENT_NOT_GRANTED"
	ReasonLegalHoldBlocksUse        = "LEGAL_HOLD_BLOCKS_USE"
	ReasonDependencyUnavailable     = "DEPENDENCY_UNAVAILABLE"
)

// ConsentCheckRequest is opt-in: the caller (who owns the domain
// operation) declares that this specific use requires consent for
// purpose_id. Omitting it means no consent check is performed — this
// service does not infer that requirement itself.
type ConsentCheckRequest struct {
	Required bool `json:"required"`
}

// LegalHoldCheckRequest is opt-in and caller-supplied, never invented by
// this service — see the package doc comment on why a made-up
// record_class would be worse than no check at all.
type LegalHoldCheckRequest struct {
	RecordClass string `json:"record_class"`
	EntityRef   string `json:"entity_ref,omitempty"`
}

// EvaluateDecisionRequest is the wire input to POST /v1/privacy/decisions.
type EvaluateDecisionRequest struct {
	TenantID             string                 `json:"tenant_id,omitempty"`
	SubjectRef           string                 `json:"subject_ref"`
	ProcessingActivityID string                 `json:"processing_activity_id"`
	PurposeID            string                 `json:"purpose_id"`
	ProposedOperation    ProposedOperation      `json:"proposed_operation"`
	ConsentCheck         *ConsentCheckRequest   `json:"consent_check,omitempty"`
	LegalHoldCheck       *LegalHoldCheckRequest `json:"legal_hold_check,omitempty"`
}

// PrivacyDecision is the append-only evidence record for one evaluation —
// §13.2 "decision durability." Never updated once written. Captures the
// EXACT resolved activity/purpose version IDs (not just the IDs the
// caller supplied) so a past decision remains reproducible regardless of
// what PRV-01/PRV-02 report today.
type PrivacyDecision struct {
	DecisionID           string            `json:"decision_id"`
	TenantID             *string           `json:"tenant_id,omitempty"`
	SubjectRef           string            `json:"subject_ref"`
	ProcessingActivityID string            `json:"processing_activity_id"`
	ActivityVersionID    *string           `json:"activity_version_id,omitempty"`
	PurposeID            string            `json:"purpose_id"`
	PurposeVersionID     *string           `json:"purpose_version_id,omitempty"`
	ProposedOperation    ProposedOperation `json:"proposed_operation"`
	Result               DecisionResult    `json:"result"`
	ReasonCodes          []string          `json:"reason_codes"`
	ConsentReceiptID     *string           `json:"consent_receipt_id,omitempty"`
	LegalHoldID          *string           `json:"legal_hold_id,omitempty"`
	ActorPrincipalID     string            `json:"actor_principal_id"`
	CorrelationID        string            `json:"correlation_id,omitempty"`
	DecidedAt            time.Time         `json:"decided_at"`
}

// MarshalReasonCodes/UnmarshalReasonCodes are small helpers the store
// uses to move ReasonCodes through a JSONB column — kept here so the
// domain type stays the single source of truth for the wire shape.
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
	return codes
}

// ── sentinel errors ──────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrDecisionNotFound = errorString("privacy decision not found")
	ErrStoreUnavailable = errorString("privacy-decision store unavailable")
)
