package domain

import (
	"errors"
	"time"
)

// BIZ-02 Record Classification is a parallel table set referencing
// Document by ID (migration 000008), not new mutable columns on the
// documents row itself — "record classification... does not grant
// access itself" and carries its own distinct command/query/event
// catalogue in the doc, exactly the same reasoning that already kept
// AUD-06's evidence chain (evidence.go) as its own aggregate.
//
// documents.classification (migration 000001) is kept as a cached
// snapshot of the current CONFIRMED value for backward-compatible
// reads — every existing caller of GetDocument/ListDocuments keeps
// working unchanged. This table is the real source of truth and the
// full governed history.
type ClassificationStatus string

const (
	ClassificationStatusCandidate  ClassificationStatus = "CANDIDATE"
	ClassificationStatusConfirmed  ClassificationStatus = "CONFIRMED"
	ClassificationStatusSuperseded ClassificationStatus = "SUPERSEDED"
	// ClassificationStatusRestricted is reached from CONFIRMED only when
	// ClassificationValue is RESTRICTED, gated by an additional
	// security/privacy-owner authorization per the doc's own SoD line —
	// wired in a later wave (see this package's Wave 2/3 commit history),
	// not part of the initial Candidate->Confirmed mechanism.
	ClassificationStatusRestricted ClassificationStatus = "RESTRICTED"
)

type ClassificationSource string

const (
	// ClassificationSourceHuman is a person's own judgment — no numeric
	// confidence, gated by maker-checker (the proposer cannot also
	// confirm their own proposal) instead of a confidence threshold.
	ClassificationSourceHuman ClassificationSource = "HUMAN"
	// ClassificationSourceAI is an automated classifier's proposal —
	// carries Confidence, gated by AIConfidenceAutoConfirmThreshold
	// instead of maker-checker (there is no personal "maker" to self-
	// approve).
	ClassificationSourceAI ClassificationSource = "AI"
)

// AIConfidenceAutoConfirmThreshold is the confidence an AI-sourced
// proposal would need to auto-confirm without human review.
//
// EXPLICIT PLACEHOLDER, not a business decision made here: the doc names
// AI-02 (classification) and DATA-GOV (retention engine) as the real
// owners of confidence policy and classification->policy mapping — ,
// neither of which exists in this codebase yet. Wiring auto-confirm
// against this constant would be inventing a policy this service was
// never given. For now every AI-sourced proposal — regardless of
// confidence — lands CANDIDATE and requires a human ConfirmClassification,
// exactly like a human-sourced one. This constant exists so the real
// mechanism (a threshold check) is visibly wired and ready, once told
// what the actual number and its owner should be.
const AIConfidenceAutoConfirmThreshold = -1.0

// RecordClassification is one classification decision in a document's
// governed classification history. Rows are inserted once and only
// status/confirmed_*/superseded_by_classification_id ever change
// afterward (migration 000008's own trigger enforces this at the DB
// layer, not just here).
type RecordClassification struct {
	ClassificationID             string               `json:"classification_id"`
	DocumentID                   string               `json:"document_id"`
	TenantID                     string               `json:"tenant_id"`
	LegalEntityID                string               `json:"legal_entity_id"`
	ClassificationValue          Classification       `json:"classification_value"`
	Status                       ClassificationStatus `json:"status"`
	Source                       ClassificationSource `json:"source"`
	Confidence                   *float64             `json:"confidence,omitempty"`
	RuleModelVersion             *string              `json:"rule_model_version,omitempty"`
	SourceEvidence               *string              `json:"source_evidence,omitempty"`
	ProposedByPrincipalID        string               `json:"proposed_by_principal_id"`
	ProposedAt                   time.Time            `json:"proposed_at"`
	ConfirmedByPrincipalID       *string              `json:"confirmed_by_principal_id,omitempty"`
	ConfirmedAt                  *time.Time           `json:"confirmed_at,omitempty"`
	SupersededByClassificationID *string              `json:"superseded_by_classification_id,omitempty"`
	EffectiveAt                  time.Time            `json:"effective_at"`
	CorrelationID                *string              `json:"correlation_id,omitempty"`
}

// ClassifyRecordParams is ClassifyRecord's input — proposes a
// classification for a document with no existing CONFIRMED
// classification (its first-ever proposal, or a repeat proposal while
// still Candidate). Reclassify (a later wave) is the equivalent command
// once a CONFIRMED classification already exists.
type ClassifyRecordParams struct {
	DocumentID            string
	ClassificationValue   Classification
	Source                ClassificationSource
	Confidence            *float64
	RuleModelVersion      string
	SourceEvidence        string
	ProposedByPrincipalID string
	CorrelationID         string
}

// ConfirmClassificationParams is ConfirmClassification's input.
type ConfirmClassificationParams struct {
	ClassificationID       string
	ConfirmedByPrincipalID string
}

// ReclassifyParams is Reclassify's input — proposes a replacement value
// for a document that already has a CONFIRMED/RESTRICTED classification.
// Structurally identical to ClassifyRecordParams; kept as its own type
// because the two commands have opposite preconditions (ClassifyRecord
// requires no existing confirmed classification, Reclassify requires
// one) and must not be interchangeable at the call site.
type ReclassifyParams struct {
	DocumentID            string
	ClassificationValue   Classification
	Source                ClassificationSource
	Confidence            *float64
	RuleModelVersion      string
	SourceEvidence        string
	ProposedByPrincipalID string
	CorrelationID         string
}

// SupersedeClassificationParams is SupersedeClassification's input — the
// governance act that confirms a Reclassify proposal and, in the same
// transaction, marks the classification it replaces as SUPERSEDED. This
// is the one place a CONFIRMED/RESTRICTED classification's
// superseded_by_classification_id is ever set (NULL -> value exactly
// once, enforced by migration 000008's trigger).
type SupersedeClassificationParams struct {
	PreviousClassificationID string
	NewClassificationID      string
	ActorPrincipalID         string
}

// PolicyMappingExplanation is ExplainPolicyMapping's result — an honest
// report of what decided a classification, not a fabricated policy
// engine. See AIConfidenceAutoConfirmThreshold's own doc comment: this
// service has no confidence-threshold/policy-mapping owner yet, so
// AutoConfirmApplied is always false and Explanation says so plainly.
type PolicyMappingExplanation struct {
	ClassificationID       string               `json:"classification_id"`
	ClassificationValue    Classification       `json:"classification_value"`
	Source                 ClassificationSource `json:"source"`
	Confidence             *float64             `json:"confidence,omitempty"`
	RuleModelVersion       *string              `json:"rule_model_version,omitempty"`
	SourceEvidence         *string              `json:"source_evidence,omitempty"`
	AutoConfirmThreshold   float64              `json:"auto_confirm_threshold"`
	AutoConfirmPolicyOwner string               `json:"auto_confirm_policy_owner"`
	AutoConfirmApplied     bool                 `json:"auto_confirm_applied"`
	Explanation            string               `json:"explanation"`
}

var (
	// ErrClassificationNotFound backs GetClassification et al.
	ErrClassificationNotFound = errors.New("record classification not found")
	// ErrInvalidClassificationSource backs ClassifyRecord — Source must
	// be HUMAN or AI.
	ErrInvalidClassificationSource = errors.New("classification source must be HUMAN or AI")
	// ErrAIConfidenceRequired backs ClassifyRecord — an AI-sourced
	// proposal must carry a confidence value.
	ErrAIConfidenceRequired = errors.New("an AI-sourced classification proposal requires a confidence value")
	// ErrHumanConfidenceNotAllowed backs ClassifyRecord — a human
	// proposal must not carry a numeric confidence.
	ErrHumanConfidenceNotAllowed = errors.New("a human-sourced classification proposal must not carry a confidence value")
	// ErrClassificationSelfConfirmation backs ConfirmClassification —
	// the doc's own maker-checker requirement for human proposals.
	ErrClassificationSelfConfirmation = errors.New("the principal who proposed a classification cannot also confirm it")
	// ErrClassificationNotCandidate backs ConfirmClassification — only a
	// CANDIDATE classification may be confirmed.
	ErrClassificationNotCandidate = errors.New("classification is not in CANDIDATE status")
	// ErrClassificationNotConfirmed backs Reclassify — there must already
	// be a CONFIRMED/RESTRICTED classification to replace. ClassifyRecord,
	// not Reclassify, is the command for a document's first-ever proposal.
	ErrClassificationNotConfirmed = errors.New("document has no confirmed classification to reclassify")
	// ErrClassificationAlreadySuperseded backs SupersedeClassification —
	// the classification being replaced must not already have been
	// superseded by an earlier reclassification.
	ErrClassificationAlreadySuperseded = errors.New("classification has already been superseded")
	// ErrClassificationDocumentMismatch backs SupersedeClassification —
	// the previous and new classification must belong to the same
	// document.
	ErrClassificationDocumentMismatch = errors.New("classification and its replacement belong to different documents")
)

// CanConfirmClassification: only a CANDIDATE classification.
func CanConfirmClassification(c *RecordClassification) bool {
	return c.Status == ClassificationStatusCandidate
}

// CanReclassify: only a CONFIRMED/RESTRICTED classification has
// something for Reclassify to replace.
func CanReclassify(c *RecordClassification) bool {
	return c.Status == ClassificationStatusConfirmed || c.Status == ClassificationStatusRestricted
}
