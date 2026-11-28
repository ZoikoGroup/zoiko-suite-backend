// Package govdecision is the shared cross-service policy decision contract of
// ZS-GOV-INT-002 (Document 02) and ZS-GOV-POL-001 (Document 01): one decision
// envelope, one outcome vocabulary, one reason-code namespace, and the checks
// every enforcement point runs before it acts on a decision.
//
// The owning service of a policy family (policy-svc for signatory matrices,
// spend-controls-svc for spend, authorization-svc for segregation of duties,
// privacy-decision-svc for notification lawfulness) produces a Decision. A
// caller never trusts it because the HTTP status was 200: it runs Check, which
// turns anything it cannot trust (unknown outcome, missing field, wrong tenant,
// entity, actor or resource, wrong owner, future-dated, expired, bad
// signature, mismatched fingerprint) into a local ERROR (Doc 02 §2.3). Only a
// trusted PERMIT for the precise action lets a caller continue, and even then
// only to its next safeguarded step (Doc 02 §3).
package govdecision

import (
	"encoding/json"
	"time"
)

// ContractVersion is the request and response contract version (Doc 02 §2.1).
const ContractVersion = "1.0"

// Outcomes (Doc 01 §8.1, Doc 02 §2.2). Exactly one per decision.
const (
	Permit            = "PERMIT"
	ApprovalRequired  = "APPROVAL_REQUIRED"
	SignatureRequired = "SIGNATURE_REQUIRED"
	ReviewRequired    = "REVIEW_REQUIRED"
	Deny              = "DENY"
	NotConfigured     = "NOT_CONFIGURED"
	NotImplemented    = "NOT_IMPLEMENTED"
	Error             = "ERROR"
)

// Outcomes lists every outcome of the contract, in the order of Doc 01 §8.1.
var Outcomes = []string{Permit, ApprovalRequired, SignatureRequired, ReviewRequired, Deny, NotConfigured, NotImplemented, Error}

// KnownOutcome reports whether s is an outcome of this contract version.
func KnownOutcome(s string) bool {
	for _, o := range Outcomes {
		if o == s {
			return true
		}
	}
	return false
}

// Normalize returns s when it is a known outcome and ERROR otherwise: an
// unknown or empty outcome can never be read as permission (C02-P0-01).
func Normalize(s string) string {
	if KnownOutcome(s) {
		return s
	}
	return Error
}

// Executable reports whether outcome lets a caller continue to its next
// safeguarded step. Only PERMIT does; it is still never the sole condition for
// an irreversible action (Doc 02 §3).
func Executable(outcome string) bool { return outcome == Permit }

// Effects of an outcome at the execution boundary (Doc 02 §3).
const (
	EffectContinue = "CONTINUE"
	EffectHold     = "HOLD"
	EffectBlock    = "BLOCK"
)

// Effect is the execution-boundary reaction to an outcome: PERMIT continues,
// the three *_REQUIRED outcomes hold, everything else (DENY, NOT_CONFIGURED,
// NOT_IMPLEMENTED, ERROR and any unknown value) blocks.
func Effect(outcome string) string {
	switch outcome {
	case Permit:
		return EffectContinue
	case ApprovalRequired, SignatureRequired, ReviewRequired:
		return EffectHold
	default:
		return EffectBlock
	}
}

// ReasonCodesVersion versions the reason-code list below (Doc 02 §3.1: code
// lists are versioned in one shared repository; display text is localized
// independently). Adding a code is a minor change; removing or re-meaning one
// is a new contract major version.
const ReasonCodesVersion = "2026-11-17.1"

// Reason codes (Doc 02 §3.1 groups, plus the codes the owning services need).
const (
	// Configuration.
	ReasonConfigPolicyMissing           = "CONFIG_POLICY_MISSING"
	ReasonConfigRuleOverlap             = "CONFIG_RULE_OVERLAP"
	ReasonConfigPolicyInvalid           = "CONFIG_POLICY_INVALID"
	ReasonConfigCurrencyUnsupported     = "CONFIG_CURRENCY_UNSUPPORTED"
	ReasonConfigValuationBasisMismatch  = "CONFIG_VALUATION_BASIS_MISMATCH"
	ReasonConfigInstrumentMatrixMissing = "CONFIG_INSTRUMENT_MATRIX_MISSING"
	ReasonConfigBandMissing             = "CONFIG_BAND_MISSING"
	// Capability.
	ReasonEvaluatorNotImplemented = "EVALUATOR_NOT_IMPLEMENTED"
	ReasonOwnedByDomainService    = "OWNED_BY_DOMAIN_SERVICE"
	// Authorization / segregation of duties.
	ReasonAuthSoDConflict       = "AUTH_SOD_CONFLICT"
	ReasonAuthDelegationExpired = "AUTH_DELEGATION_EXPIRED"
	// Spend.
	ReasonSpendHardCap       = "SPEND_HARD_CAP"
	ReasonSpendSoftCap       = "SPEND_SOFT_CAP"
	ReasonSpendFXUnavailable = "SPEND_FX_UNAVAILABLE"
	// Approval thresholds.
	ReasonApprovalThresholdExceeded = "APPROVAL_THRESHOLD_EXCEEDED"
	ReasonWithinApprovalThreshold   = "WITHIN_APPROVAL_THRESHOLD"
	// Signatory matrix.
	ReasonSignatureQuorumIncomplete   = "SIGNATURE_QUORUM_INCOMPLETE"
	ReasonSignatureDocumentChanged    = "SIGNATURE_DOCUMENT_CHANGED"
	ReasonSignatureExempt             = "SIGNATURE_EXEMPT"
	ReasonSignatureLegalReview        = "SIGNATURE_LEGAL_REVIEW_REQUIRED"
	ReasonSignatureSeriesAggregated   = "SIGNATURE_SERIES_AGGREGATED"
	ReasonSignatureHighRiskInstrument = "SIGNATURE_HIGH_RISK_INSTRUMENT"
	ReasonSignatureReleaseMandate     = "SIGNATURE_RELEASE_MANDATE_REQUIRED"
	// Privacy and notifications.
	ReasonPrivacyMarketingSuppressed = "PRIVACY_MARKETING_SUPPRESSED"
	ReasonPrivacyRulepackMissing     = "PRIVACY_RULEPACK_MISSING"
	// Runtime and contract (also the local synthetic ERROR reasons).
	ReasonEvidenceUnavailable = "EVIDENCE_UNAVAILABLE"
	ReasonServiceTimeout      = "SERVICE_TIMEOUT"
	ReasonContractInvalid     = "CONTRACT_RESPONSE_INVALID"
	ReasonDecisionExpired     = "DECISION_EXPIRED"
	ReasonIntegrityFailed     = "DECISION_INTEGRITY_FAILED"
)

// ReasonCodes lists every reason code of ReasonCodesVersion.
var ReasonCodes = []string{
	ReasonConfigPolicyMissing, ReasonConfigRuleOverlap, ReasonConfigPolicyInvalid, ReasonConfigCurrencyUnsupported,
	ReasonConfigValuationBasisMismatch, ReasonConfigInstrumentMatrixMissing, ReasonConfigBandMissing,
	ReasonEvaluatorNotImplemented, ReasonOwnedByDomainService,
	ReasonAuthSoDConflict, ReasonAuthDelegationExpired,
	ReasonSpendHardCap, ReasonSpendSoftCap, ReasonSpendFXUnavailable,
	ReasonApprovalThresholdExceeded, ReasonWithinApprovalThreshold,
	ReasonSignatureQuorumIncomplete, ReasonSignatureDocumentChanged, ReasonSignatureExempt, ReasonSignatureLegalReview,
	ReasonSignatureSeriesAggregated, ReasonSignatureHighRiskInstrument, ReasonSignatureReleaseMandate,
	ReasonPrivacyMarketingSuppressed, ReasonPrivacyRulepackMissing,
	ReasonEvidenceUnavailable, ReasonServiceTimeout, ReasonContractInvalid, ReasonDecisionExpired, ReasonIntegrityFailed,
}

// Resource names the exact object a decision is about. Version is immutable or
// concurrency-checked; a changed version invalidates the decision (Doc 02 §2.3).
type Resource struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Version string `json:"version"`
}

// Money is an exact monetary value: a decimal string, an ISO 4217 currency and
// the valuation basis (GROSS, or a published alternate such as NET). Never a
// floating-point number (Doc 02 §2.1).
type Money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
	Basis    string `json:"basis"`
}

// Valuation bases.
const (
	BasisGross = "GROSS"
	BasisNet   = "NET"
)

// RequestContext is the common request context of every governed evaluation
// (Doc 02 §2.1). Family-specific fields travel beside it.
type RequestContext struct {
	ContractVersion  string    `json:"contract_version"`
	RequestID        string    `json:"request_id"`
	CorrelationID    string    `json:"correlation_id"`
	TenantID         string    `json:"tenant_id"`
	LegalEntityID    string    `json:"legal_entity_id"`
	ActorPrincipalID string    `json:"actor_principal_id"`
	Action           string    `json:"action"`
	Resource         Resource  `json:"resource"`
	Money            *Money    `json:"money,omitempty"`
	EvaluatedForAt   time.Time `json:"evaluated_for_at"`
}

// OfficeRequirement is a minimum number of distinct signers who must hold one
// corporate office (DIRECTOR, CFO, CEO, FINANCE_SIGNER, ...).
type OfficeRequirement struct {
	Office   string `json:"office"`
	MinCount int    `json:"min_count"`
}

// Kinds of required action.
const (
	ActionKindApproval             = "APPROVAL"
	ActionKindSignatureQuorum      = "SIGNATURE_QUORUM"
	ActionKindSignerEligibility    = "SIGNER_ELIGIBILITY"
	ActionKindLegalReview          = "LEGAL_REVIEW"
	ActionKindReleaseAuthorization = "RELEASE_AUTHORIZATION"
	ActionKindReview               = "REVIEW"
)

// Constraints a signature quorum carries. They are invariants, not options: a
// matrix cannot switch them off.
const (
	ConstraintDistinctHumans         = "DISTINCT_HUMANS"
	ConstraintPreparerExcluded       = "PREPARER_EXCLUDED"
	ConstraintAuthorityAtSigning     = "SIGNER_AUTHORITY_AT_SIGNING"
	ConstraintDocumentHashBound      = "DOCUMENT_HASH_BOUND"
	ConstraintLegalEntityScoped      = "LEGAL_ENTITY_SCOPED"
	ConstraintDistinctDepartments    = "DISTINCT_DEPARTMENTS"
	ConstraintNoImplicitDelegate     = "NO_IMPLICIT_DELEGATE"
	ConstraintNotProxyForBankMandate = "APPROVAL_IS_NOT_A_BANK_MANDATE"
)

// RequiredAction is one controlled task a decision hands to its enforcer:
// approvals, a signature quorum, signer eligibility, legal review, release
// authorization. Never client-supplied authority (Doc 02 §2.2).
type RequiredAction struct {
	Kind               string              `json:"kind"`
	Owner              string              `json:"owner,omitempty"`
	Office             string              `json:"office,omitempty"`
	MinDistinctSigners int                 `json:"min_distinct_signers,omitempty"`
	RequiredOffices    []OfficeRequirement `json:"required_offices,omitempty"`
	EligibleOffices    []string            `json:"eligible_offices,omitempty"`
	Sequencing         string              `json:"sequencing,omitempty"`
	Order              []string            `json:"order,omitempty"`
	Constraints        []string            `json:"constraints,omitempty"`
	DocumentHash       string              `json:"document_hash,omitempty"`
	Detail             string              `json:"detail,omitempty"`
}

// Integrity is the decision's integrity assertion: an Ed25519 signature
// ("EdDSA") by key KeyID over SigningInput of the decision.
type Integrity struct {
	Alg   string `json:"alg"`
	KeyID string `json:"kid"`
	Value string `json:"value"`
}

// Decision is the canonical decision envelope (Doc 02 §2.2).
type Decision struct {
	ContractVersion string `json:"contract_version"`
	DecisionID      string `json:"decision_id"`
	RequestID       string `json:"request_id"`
	CorrelationID   string `json:"correlation_id"`

	Outcome       string `json:"outcome"`
	OwningService string `json:"owning_service"`
	PolicyFamily  string `json:"policy_family"`

	// RuleID and PolicyVersion are the provenance of the evaluated rule. A
	// decision no rule produced (NOT_CONFIGURED, NOT_IMPLEMENTED) says so in
	// RuleID and carries an explicit reason code.
	RuleID          string `json:"rule_id"`
	PolicyVersion   string `json:"policy_version"`
	PolicyID        string `json:"policy_id,omitempty"`
	PolicyCode      string `json:"policy_code,omitempty"`
	PolicyVersionID string `json:"policy_version_id,omitempty"`

	TenantID         string   `json:"tenant_id"`
	LegalEntityID    string   `json:"legal_entity_id"`
	ActorPrincipalID string   `json:"actor_principal_id"`
	Action           string   `json:"action"`
	Resource         Resource `json:"resource"`

	EvaluatedAt      time.Time `json:"evaluated_at"`
	ValidUntil       time.Time `json:"valid_until"`
	InputFingerprint string    `json:"input_fingerprint"`

	ReasonCodes     []string         `json:"reason_codes"`
	RequiredActions []RequiredAction `json:"required_actions"`

	EvidenceRef  string   `json:"evidence_ref"`
	UpstreamRefs []string `json:"upstream_refs"`

	// Detail is family-specific, non-authoritative context (the band chosen,
	// the amount compared). Enforcers act on Outcome and RequiredActions only.
	Detail json.RawMessage `json:"detail,omitempty"`

	Integrity *Integrity `json:"integrity,omitempty"`
}
