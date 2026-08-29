// Package domain defines the canonical types for privacy-rights-svc —
// PRV-04, "Data Rights, Complaint & Disclosure Control Service", from
// ZS-SVC-W-001 §14/§15.
//
// v1 scope, documented rather than hidden (same doctrine as PRV-01/02/03):
//
//   - §14.1 draws an explicit ownership line: "PRV-04 owns the privacy
//     meaning and evidence of a rights request. WFC [workflow-svc] owns
//     long-running orchestration, tasks, deadlines, approvals and
//     escalations. Domain adapters perform search, correction,
//     restriction, export or erasure under explicit instructions and
//     return evidence." This service builds ONLY the first half: case
//     intake, identity-assurance evidence, discovery-manifest evidence,
//     and closure/outcome evidence. It does not orchestrate tasks or
//     deadlines, and it does not perform search/correction/export/
//     erasure itself — those remain other services' responsibility,
//     exactly as the spec assigns them.
//   - The canonical API table (§18) shows this service's own
//     POST /privacy/rights-requests returning a wfc_process_ref,
//     implying PRV-04 creates a workflow-svc instance at intake. This is
//     deliberately NOT done: workflow-svc's real CreateWorkflow contract
//     requires the caller to name a concrete approver_principal_id for
//     every stage up front (see workflow-svc/internal/handler.go's
//     createWorkflowRequest validation) — at intake time this service has
//     no legitimate way to know who the right approver is for an
//     arbitrary jurisdiction/right-family combination without inventing
//     one. WFCProcessRef is therefore an OPTIONAL, caller-supplied field:
//     whoever actually creates the real workflow-svc instance (a human
//     process, or a future orchestrator with real approver-routing rules)
//     may attach its reference here, but this service never invents it.
//   - Identity verification is recorded as a caller-declared fact, not
//     performed by this service — §15.1's own risk-proportionate identity
//     assurance is an organizational/legal judgment call, not something
//     this service can safely automate without a real verification
//     provider integration, which does not exist in this codebase.
//   - The one substantive rule this service DOES enforce is §15.2's own
//     explicit text, not an invented one: "DISCLOSURE GATE: Finding a
//     record is only discovery. Disclosure requires identity assurance...
//     [and] approved response assembly." A request can only be closed
//     with outcome FULFILLED if identity has been verified AND at least
//     one discovery manifest has been recorded — REJECTED/WITHDRAWN carry
//     no such precondition, since those outcomes mean the request never
//     reached fulfillment.
//   - Exemption/third-party review, redaction, and response-package
//     assembly (§15.1's other control points) are not modeled — they
//     require case-specific legal judgment this service cannot supply.
package domain

import "time"

// RightFamily is data only (§14.2's own table) — new values are added via
// data, same doctrine as every enum-shaped string column in this
// platform.
type RightFamily string

const (
	RightAccess                     RightFamily = "ACCESS"
	RightRectification              RightFamily = "RECTIFICATION"
	RightErasure                    RightFamily = "ERASURE"
	RightRestriction                RightFamily = "RESTRICTION"
	RightPortability                RightFamily = "PORTABILITY"
	RightObjectionWithdrawal        RightFamily = "OBJECTION_WITHDRAWAL"
	RightAutomatedDecisionChallenge RightFamily = "AUTOMATED_DECISION_CHALLENGE"
	// RightComplaint is deliberately in the same enum as the statutory
	// rights (§14.2 lists it in the same input table), but §14.2 also
	// warns not to conflate it with a statutory rights request — this
	// service does not special-case it in the state machine, only in
	// meaning; callers/reporting are expected to treat it distinctly.
	RightComplaint RightFamily = "COMPLAINT"
)

func (f RightFamily) Valid() bool {
	switch f {
	case RightAccess, RightRectification, RightErasure, RightRestriction, RightPortability,
		RightObjectionWithdrawal, RightAutomatedDecisionChallenge, RightComplaint:
		return true
	}
	return false
}

// RequestStatus is the privacy-meaning milestone PRV-04 itself tracks —
// deliberately coarser than the spec's full Figure 7 pipeline
// (RECEIVE -> CLASSIFY -> IDENTITY ASSURANCE -> DISCOVERY -> EXEMPTION
// REVIEW -> ACTION INSTRUCTIONS -> RESPONSE PACKAGE -> QA/APPROVAL ->
// RESPOND -> CLOSE), because everything between "identity assurance" and
// "respond" is WFC-owned task/approval orchestration, not PRV-04's own
// state.
type RequestStatus string

const (
	StatusReceived         RequestStatus = "RECEIVED"
	StatusIdentityVerified RequestStatus = "IDENTITY_VERIFIED"
	StatusInDiscovery      RequestStatus = "IN_DISCOVERY"
	StatusClosed           RequestStatus = "CLOSED"
)

// Outcome is recorded only at closure.
type Outcome string

const (
	OutcomeFulfilled Outcome = "FULFILLED"
	OutcomeRejected  Outcome = "REJECTED"
	OutcomeWithdrawn Outcome = "WITHDRAWN"
)

func (o Outcome) Valid() bool {
	switch o {
	case OutcomeFulfilled, OutcomeRejected, OutcomeWithdrawn:
		return true
	}
	return false
}

// RightsRequest is the case record — PRV-04's own privacy-meaning state,
// not a workflow/task list.
type RightsRequest struct {
	RequestID            string        `json:"request_id"`
	TenantID             *string       `json:"tenant_id,omitempty"`
	SubjectRef           string        `json:"subject_ref"`
	RightFamily          RightFamily   `json:"right_family"`
	Jurisdiction         string        `json:"jurisdiction,omitempty"`
	RequesterRef         string        `json:"requester_ref,omitempty"` // proxy/representative, if not the subject themselves
	SubmittedVia         string        `json:"submitted_via,omitempty"`
	Status               RequestStatus `json:"status"`
	IdentityVerified     bool          `json:"identity_verified"`
	Outcome              *Outcome      `json:"outcome,omitempty"`
	ResponseEvidenceHash *string       `json:"response_evidence_hash,omitempty"`
	// WFCProcessRef is optional and caller-supplied — see the package doc
	// comment on why this service never creates it itself.
	WFCProcessRef        *string    `json:"wfc_process_ref,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	ClosedAt             *time.Time `json:"closed_at,omitempty"`
}

// IdentityVerificationEvent is append-only evidence of one verification
// attempt — recording an attempt that FAILED is just as evidentially
// important as recording one that succeeded (§15.1: proportionate,
// auditable identity assurance).
type IdentityVerificationEvent struct {
	EventID               string    `json:"event_id"`
	TenantID              *string   `json:"tenant_id,omitempty"`
	RequestID             string    `json:"request_id"`
	Verified              bool      `json:"verified"`
	Method                string    `json:"method"`
	Note                  string    `json:"note,omitempty"`
	VerifiedByPrincipalID string    `json:"verified_by_principal_id"`
	CreatedAt             time.Time `json:"created_at"`
}

// DiscoveryManifest is append-only evidence a domain service attaches —
// §15.1: "Each domain returns a signed/hashed discovery manifest of
// candidate records, categories, owners and actionability." This service
// does not perform the search itself; it only records the manifest a
// domain adapter already produced.
type DiscoveryManifest struct {
	ManifestID             string    `json:"manifest_id"`
	TenantID               *string   `json:"tenant_id,omitempty"`
	RequestID              string    `json:"request_id"`
	Domain                 string    `json:"domain"`
	ContentHash            string    `json:"content_hash"`
	CandidateCount         int       `json:"candidate_count"`
	EvidenceRef            string    `json:"evidence_ref,omitempty"`
	SubmittedByPrincipalID string    `json:"submitted_by_principal_id"`
	CreatedAt              time.Time `json:"created_at"`
}

// ── Request DTOs ─────────────────────────────────────────────────────────────

type CreateRightsRequestRequest struct {
	TenantID     string      `json:"tenant_id,omitempty"`
	SubjectRef   string      `json:"subject_ref"`
	RightFamily  RightFamily `json:"right_family"`
	Jurisdiction string      `json:"jurisdiction,omitempty"`
	RequesterRef string      `json:"requester_ref,omitempty"`
	SubmittedVia string      `json:"submitted_via,omitempty"`
}

type RecordIdentityVerificationRequest struct {
	Verified bool   `json:"verified"`
	Method   string `json:"method"`
	Note     string `json:"note,omitempty"`
}

type AttachDiscoveryManifestRequest struct {
	Domain         string `json:"domain"`
	ContentHash    string `json:"content_hash"`
	CandidateCount int    `json:"candidate_count"`
	EvidenceRef    string `json:"evidence_ref,omitempty"`
}

type CloseRequestRequest struct {
	Outcome              Outcome `json:"outcome"`
	ResponseEvidenceHash string  `json:"response_evidence_hash,omitempty"`
	Reason               string  `json:"reason,omitempty"`
}

type AttachWFCProcessRefRequest struct {
	WFCProcessRef string `json:"wfc_process_ref"`
}

// ── sentinel errors ──────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrRequestNotFound      = errorString("rights request not found")
	ErrRequestAlreadyClosed = errorString("rights request is already closed")
	ErrIdentityNotVerified  = errorString("identity has not been verified for this request")
	ErrNoDiscoveryManifest  = errorString("no discovery manifest has been recorded for this request")
	ErrStoreUnavailable     = errorString("privacy-rights store unavailable")
)
