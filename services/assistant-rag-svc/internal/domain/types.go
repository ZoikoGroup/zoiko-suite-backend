// Package domain defines the authoritative domain types for
// assistant-rag-svc (AI-03, ZS-SVC-N-001 §4/§13 Wave 7). This service
// answers permitted questions from authorized source evidence and
// invokes approved tools through normal domain commands — it never
// holds hidden business authority, unrestricted database access, or
// direct privileged write authority, and never retains chain-of-thought.
//
// As with document-extraction-svc and classification-svc, this pass
// does not implement an actual retrieval engine or LLM. Retrieve and
// Ask accept already-produced retrieval candidates and model output as
// caller-supplied evidence; this service governs the authorization-
// before-retrieval boundary, citation enforcement, tool-call
// provenance/re-authorization, and audit trail around them.
package domain

import (
	"fmt"
	"time"
)

const (
	PrefixSession       = "ask_"
	PrefixRetrievalSet  = "rts_"
	PrefixRetrievedItem = "rti_"
	PrefixExecution     = "pex_"
	PrefixCitation      = "cit_"
	PrefixToolProposal  = "tpr_"
	PrefixEvidence      = "are_"
	PrefixToolPolicy    = "tlp_"
	PrefixSourceGrant   = "sgr_"
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

// ── Governance registries (mutable upsert, same idiom as ModelRelease) ────────

// SourceGrant is the seller-managed registration that a given source is
// authorized for retrieval under a given purpose. Presence is
// authorization: Retrieve excludes any candidate source with no
// matching grant from the RetrievalSet before any generation can see
// it — "RAG cannot retrieve unauthorized source before generation" is
// enforced here, not by a downstream filter.
type SourceGrant struct {
	TenantID  string    `json:"tenant_id"`
	SourceRef string    `json:"source_ref"`
	Purpose   string    `json:"purpose"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

type RegisterSourceGrantRequest struct {
	SourceRef string `json:"source_ref"`
	Purpose   string `json:"purpose"`
}

func (r RegisterSourceGrantRequest) Validate() error {
	if r.SourceRef == "" || r.Purpose == "" {
		return fmt.Errorf("source_ref and purpose are required")
	}
	return nil
}

// ToolPolicy is the seller-managed tool allow-list: ProposeToolCall
// refuses any tool not registered here outright, and Protected is
// frozen onto each proposal at creation time from this registry — a
// tool's protection status cannot be downgraded retroactively for an
// in-flight proposal.
type ToolPolicy struct {
	TenantID     string    `json:"tenant_id"`
	ToolName     string    `json:"tool_name"`
	TargetDomain string    `json:"target_domain"`
	Protected    bool      `json:"protected"`
	CreatedAt    time.Time `json:"created_at"`
	CreatedBy    string    `json:"created_by"`
}

type RegisterToolPolicyRequest struct {
	ToolName     string `json:"tool_name"`
	TargetDomain string `json:"target_domain"`
	Protected    bool   `json:"protected"`
}

func (r RegisterToolPolicyRequest) Validate() error {
	if r.ToolName == "" || r.TargetDomain == "" {
		return fmt.Errorf("tool_name and target_domain are required")
	}
	return nil
}

// ── AssistantSession ────────────────────────────────────────────────────────

type SessionStatus string

const (
	SessionActive    SessionStatus = "Active"
	SessionCompleted SessionStatus = "Completed"
	SessionBlocked   SessionStatus = "Blocked"
	SessionEscalated SessionStatus = "Escalated"
)

// AssistantSession is purpose-scoped: every Retrieve/Ask/ProposeToolCall
// within it is implicitly bound to the Purpose it was opened with.
// Forward-only lifecycle: Active -> exactly one of
// Completed/Blocked/Escalated, then terminal forever.
type AssistantSession struct {
	SessionID string        `json:"session_id"`
	TenantID  string        `json:"tenant_id"`
	SubjectID string        `json:"subject_id"`
	Purpose   string        `json:"purpose"`
	Status    SessionStatus `json:"status"`
	CreatedAt time.Time     `json:"created_at"`
	CreatedBy string        `json:"created_by"`
	EndedAt   *time.Time    `json:"ended_at,omitempty"`
	EndedBy   string        `json:"ended_by,omitempty"`
}

type StartSessionRequest struct {
	SubjectID string `json:"subject_id"`
	Purpose   string `json:"purpose"`
}

func (r StartSessionRequest) Validate() error {
	if r.SubjectID == "" || r.Purpose == "" {
		return fmt.Errorf("subject_id and purpose are required")
	}
	return nil
}

// ── RetrievalSet / RetrievedItem ────────────────────────────────────────────

// CandidateSource is a caller-asserted retrieval hit, evaluated against
// the tenant's SourceGrant registry before it can ever become citable
// evidence.
type CandidateSource struct {
	SourceRef string `json:"source_ref"`
	Snippet   string `json:"snippet"`
}

type RetrieveRequest struct {
	SessionID  string            `json:"session_id"`
	Query      string            `json:"query"`
	Candidates []CandidateSource `json:"candidates"`
}

func (r RetrieveRequest) Validate() error {
	if r.SessionID == "" || r.Query == "" {
		return fmt.Errorf("session_id and query are required")
	}
	if len(r.Candidates) == 0 {
		return fmt.Errorf("at least one candidate source is required")
	}
	for _, c := range r.Candidates {
		if c.SourceRef == "" {
			return fmt.Errorf("candidate source_ref is required")
		}
	}
	return nil
}

// RetrievalSet is the frozen record of one Retrieve call — immutable
// once written, including every excluded candidate (kept for audit,
// never silently dropped).
type RetrievalSet struct {
	RetrievalSetID string    `json:"retrieval_set_id"`
	TenantID       string    `json:"tenant_id"`
	SessionID      string    `json:"session_id"`
	Query          string    `json:"query"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedBy      string    `json:"created_by"`
}

// RetrievedItem is one candidate source's authorization outcome.
// Authorized=false items are retained for audit but can never be cited
// by a subsequent Ask against this retrieval set.
type RetrievedItem struct {
	ItemID         string    `json:"item_id"`
	TenantID       string    `json:"tenant_id"`
	RetrievalSetID string    `json:"retrieval_set_id"`
	SourceRef      string    `json:"source_ref"`
	Snippet        string    `json:"snippet"`
	Authorized     bool      `json:"authorized"`
	ExcludedReason string    `json:"excluded_reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// ── PromptExecution / Citation ──────────────────────────────────────────────

type CitationInput struct {
	SourceRef string `json:"source_ref"`
	Snippet   string `json:"snippet"`
}

type AskRequest struct {
	SessionID      string          `json:"session_id"`
	RetrievalSetID string          `json:"retrieval_set_id"`
	ModelProvider  string          `json:"model_provider"`
	ModelVersion   string          `json:"model_version"`
	PromptVersion  string          `json:"prompt_version"`
	AnswerText     string          `json:"answer_text"`
	Citations      []CitationInput `json:"citations"`
}

func (r AskRequest) Validate() error {
	if r.SessionID == "" || r.RetrievalSetID == "" {
		return fmt.Errorf("session_id and retrieval_set_id are required")
	}
	if r.ModelProvider == "" || r.ModelVersion == "" {
		return fmt.Errorf("model_provider and model_version are required")
	}
	if r.AnswerText == "" {
		return fmt.Errorf("answer_text is required")
	}
	if len(r.Citations) == 0 {
		return fmt.Errorf("at least one citation is required: an answer cannot assert authority with no cited evidence")
	}
	for _, c := range r.Citations {
		if c.SourceRef == "" {
			return fmt.Errorf("citation source_ref is required")
		}
	}
	return nil
}

// PromptExecution is one Ask call's immutable provenance record — a
// prompt/model upgrade is always a new execution, never a mutation of
// a prior one.
type PromptExecution struct {
	ExecutionID    string    `json:"execution_id"`
	TenantID       string    `json:"tenant_id"`
	SessionID      string    `json:"session_id"`
	RetrievalSetID string    `json:"retrieval_set_id"`
	ModelProvider  string    `json:"model_provider"`
	ModelVersion   string    `json:"model_version"`
	PromptVersion  string    `json:"prompt_version"`
	Query          string    `json:"query"`
	AnswerText     string    `json:"answer_text"`
	ContentHash    string    `json:"content_hash"`
	CreatedAt      time.Time `json:"created_at"`
}

// Citation ties one Ask response to one authorized RetrievedItem.
// Immutable, and structurally impossible to create against an
// unauthorized or absent source — the store layer validates every
// citation's source_ref against the retrieval set's Authorized=true
// items before inserting any citation row.
type Citation struct {
	CitationID  string    `json:"citation_id"`
	TenantID    string    `json:"tenant_id"`
	ExecutionID string    `json:"execution_id"`
	SourceRef   string    `json:"source_ref"`
	Snippet     string    `json:"snippet"`
	CreatedAt   time.Time `json:"created_at"`
}

// ── AIResponseEvidence ───────────────────────────────────────────────────────

type ResponseOutcome string

const (
	ResponseAnswered ResponseOutcome = "Answered"
)

// AIResponseEvidence is the append-only, exactly-once audit record for
// one Ask call — sufficient for replay/investigation without
// re-querying the execution/citation rows individually.
type AIResponseEvidence struct {
	EvidenceID    string          `json:"evidence_id"`
	TenantID      string          `json:"tenant_id"`
	ExecutionID   string          `json:"execution_id"`
	Outcome       ResponseOutcome `json:"outcome"`
	CitationCount int32           `json:"citation_count"`
	CreatedAt     time.Time       `json:"created_at"`
}

// ── ToolProposal ─────────────────────────────────────────────────────────────

type ToolProposalStatus string

const (
	ToolProposed ToolProposalStatus = "Proposed"
	ToolApproved ToolProposalStatus = "Approved"
	ToolRejected ToolProposalStatus = "Rejected"
	ToolExecuted ToolProposalStatus = "Executed"
)

// JustificationSource records whether a tool call was requested
// because of an explicit, trusted instruction (System — the subject's
// own request, or an operator command) or because the model inferred
// it from retrieved content (RetrievedContent — which may contain
// prompt injection). A Protected tool proposed from RetrievedContent
// can never auto-approve, no matter how the model phrased it: this is
// the structural answer to "retrieved prompt injection cannot invoke a
// protected tool."
type JustificationSource string

const (
	JustificationSystem           JustificationSource = "System"
	JustificationRetrievedContent JustificationSource = "RetrievedContent"
)

type ProposeToolCallRequest struct {
	SessionID           string              `json:"session_id"`
	ToolName            string              `json:"tool_name"`
	Justification       string              `json:"justification"`
	JustificationSource JustificationSource `json:"justification_source"`
}

func (r ProposeToolCallRequest) Validate() error {
	if r.SessionID == "" || r.ToolName == "" {
		return fmt.Errorf("session_id and tool_name are required")
	}
	if r.Justification == "" {
		return fmt.Errorf("justification is required")
	}
	if r.JustificationSource != JustificationSystem && r.JustificationSource != JustificationRetrievedContent {
		return fmt.Errorf("justification_source must be System or RetrievedContent")
	}
	return nil
}

// ToolProposal is forward-only: Proposed -> (Approved|Rejected), and
// Approved -> Executed. TargetDomain/Protected are frozen from the
// ToolPolicy registry at proposal time and never change afterward.
type ToolProposal struct {
	ProposalID          string              `json:"proposal_id"`
	TenantID            string              `json:"tenant_id"`
	SessionID           string              `json:"session_id"`
	ToolName            string              `json:"tool_name"`
	TargetDomain        string              `json:"target_domain"`
	Protected           bool                `json:"protected"`
	Justification       string              `json:"justification"`
	JustificationSource JustificationSource `json:"justification_source"`
	Status              ToolProposalStatus  `json:"status"`
	CreatedAt           time.Time           `json:"created_at"`
	CreatedBy           string              `json:"created_by"`
	DecidedAt           *time.Time          `json:"decided_at,omitempty"`
	DecidedBy           string              `json:"decided_by,omitempty"`
	DecisionReason      string              `json:"decision_reason,omitempty"`
}

type DecideToolProposalRequest struct {
	Reason string `json:"reason"`
}

func (r DecideToolProposalRequest) Validate() error {
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

var (
	ErrSessionNotFound             = errorString("assistant session not found")
	ErrSessionNotActive            = errorString("assistant session is not Active")
	ErrRetrievalSetNotFound        = errorString("retrieval set not found")
	ErrCitationSourceNotAuthorized = errorString("citation references a source not authorized in this retrieval set")
	ErrToolPolicyNotFound          = errorString("tool is not registered in the tool policy")
	ErrToolProposalNotFound        = errorString("tool proposal not found")
	ErrToolProposalNotPending      = errorString("tool proposal is not awaiting a decision")
	ErrToolProposalNotApproved     = errorString("tool proposal is not Approved")
	ErrIdempotencyKeyReused        = errorString("idempotency key was already used for a different request")
)
