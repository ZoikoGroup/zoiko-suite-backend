package domain

import "time"

// AIG-04 Human Oversight, Output Disposition & Decision Boundary
// Service (ZS-SVC-X-001 §7).
//
// OutputDisposition is the ONLY source of disposition authority for an
// AI output (NP-39): the model's own claimed status is never read or
// stored here, and the only way a disposition reaches ACCEPTED/
// REJECTED is an explicit DecideDisposition call by a real reviewer.
// This package never executes anything downstream of a decision —
// that absence is what makes INV-20 ("human acceptance does not
// itself bypass domain/WFC authorization") structurally true.

// OversightClass is §7.1's O0-O4 scale, determined from the owning use
// case's OperationalClass (plus an optional caller-asserted dual-
// control flag for the highest-impact slice of A3) at disposition
// creation time.
type OversightClass string

const (
	OversightNone            OversightClass = "O0" // auto quality monitoring only
	OversightUserReview      OversightClass = "O1" // identified user sees status/sources/limitations, can reject/edit
	OversightQualifiedReview OversightClass = "O2" // qualified reviewer, explicit accept/reject, no self-bypass
	OversightDualControl     OversightClass = "O3" // dual/control review, highest-impact
	OversightProhibited      OversightClass = "O4" // AI prohibited: technical block
)

// DetermineOversightClass maps a use case's operational class to the
// oversight class its outputs require. requireDualControl lets the
// caller assert that this specific output is the highest-impact slice
// of an A3 use case — there is no way to derive that distinction from
// the use case alone, so the caller (who knows the specific
// execution's materiality) attests it, the same pattern already used
// for AIG-02's caller-attested release gates.
func DetermineOversightClass(opClass OperationalClass, requireDualControl bool) OversightClass {
	switch opClass {
	case OperationalClassA0, OperationalClassA1:
		return OversightNone
	case OperationalClassA2:
		return OversightUserReview
	case OperationalClassA3:
		if requireDualControl {
			return OversightDualControl
		}
		return OversightQualifiedReview
	case OperationalClassA4:
		return OversightProhibited
	default:
		return OversightQualifiedReview
	}
}

// DispositionState is §7.3 Figure 7's state machine. BLOCKED is the
// validation-failure branch (unsafe/schema-invalid/policy-breach
// output) — independent of oversight class and terminal; a blocked
// output is never reviewable. DRAFT_ASSISTIVE/REVIEW_REQUIRED are the
// pre-decision states (O0/O1 vs O2/O3); SUPERSEDED means a later
// disposition replaced this one.
type DispositionState string

const (
	DispositionBlocked        DispositionState = "BLOCKED"
	DispositionDraftAssistive DispositionState = "DRAFT_ASSISTIVE"
	DispositionReviewRequired DispositionState = "REVIEW_REQUIRED"
	DispositionAccepted       DispositionState = "ACCEPTED"
	DispositionRejected       DispositionState = "REJECTED"
	DispositionSuperseded     DispositionState = "SUPERSEDED"
)

// InitialDispositionState returns the state a newly created disposition
// starts in, given its oversight class.
func InitialDispositionState(oc OversightClass) DispositionState {
	switch oc {
	case OversightNone, OversightUserReview:
		return DispositionDraftAssistive
	default:
		return DispositionReviewRequired
	}
}

// OutputDisposition is §7's append-only disposition record for one AI
// output. execution_ref is a caller-attested pointer to the output
// under review, not a FK to a real execution log — no live
// model-inference gateway exists in this codebase to generate one
// (AIG-03 was deliberately not built), so this service accepts the
// reference as evidence rather than inventing a fake integration.
type OutputDisposition struct {
	DispositionID             string           `json:"disposition_id"`
	TenantID                  string           `json:"tenant_id"`
	UseCaseID                 string           `json:"use_case_id"`
	ExecutionRef              string           `json:"execution_ref"`
	OversightClass            OversightClass   `json:"oversight_class"`
	DispositionState          DispositionState `json:"disposition_state"`
	ValidationFailureReason   string           `json:"validation_failure_reason,omitempty"`
	RequiredReviewerRole      string           `json:"required_reviewer_role,omitempty"`
	ReviewerPrincipalID       string           `json:"reviewer_principal_id,omitempty"`
	DecisionReason            string           `json:"decision_reason,omitempty"`
	DownstreamActionRef       string           `json:"downstream_action_ref,omitempty"`
	RapidDecisionFlag         bool             `json:"rapid_decision_flag"`
	SupersededByDispositionID string           `json:"superseded_by_disposition_id,omitempty"`
	CreatedAt                 time.Time        `json:"created_at"`
	CreatedByPrincipalID      string           `json:"created_by_principal_id"`
	DecidedAt                 *time.Time       `json:"decided_at,omitempty"`
}

// CreateDispositionRequest submits an AI output for disposition.
// ValidationFailureReason, when non-empty, routes the disposition
// straight to BLOCKED regardless of oversight class — the output
// failed validation (unsafe/schema-invalid/policy-breach) before
// oversight routing was even relevant. RequireDualControl is only
// meaningful for an A3 use case (see DetermineOversightClass).
type CreateDispositionRequest struct {
	UseCaseID               string `json:"use_case_id"`
	ExecutionRef            string `json:"execution_ref"`
	ValidationFailureReason string `json:"validation_failure_reason,omitempty"`
	RequireDualControl      bool   `json:"require_dual_control,omitempty"`
	RequiredReviewerRole    string `json:"required_reviewer_role,omitempty"`
}

// DecideDispositionRequest is a reviewer's explicit accept/reject of a
// disposition. This is the ONLY code path that can move a disposition
// to ACCEPTED or REJECTED — there is no time-based or caller-text-
// based transition into either state (NP-40: a review deadline
// expiring never auto-accepts; NP-39: the model's own claimed status
// is never read here).
type DecideDispositionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// SupersedeDispositionRequest replaces a decided disposition with a
// fresh one — e.g. the reviewed output needs re-review after a
// material change — never an in-place rewrite of the original
// decision.
type SupersedeDispositionRequest struct {
	ExecutionRef            string `json:"execution_ref"`
	ValidationFailureReason string `json:"validation_failure_reason,omitempty"`
	RequireDualControl      bool   `json:"require_dual_control,omitempty"`
	RequiredReviewerRole    string `json:"required_reviewer_role,omitempty"`
}

// ── errors ───────────────────────────────────────────────────────────────────

const (
	// AIG-04
	ErrDispositionNotFound        = errorString("output disposition not found")
	ErrDispositionNotDecidable    = errorString("output disposition is not DRAFT_ASSISTIVE or REVIEW_REQUIRED")
	ErrDispositionNotSupersedable = errorString("output disposition must be ACCEPTED or REJECTED to be superseded")
	ErrInvalidDispositionDecision = errorString("decision must be ACCEPTED or REJECTED")
	// ErrReviewerCannotBeSubmitter is §7.2's "no self-bypass" for a
	// qualified/dual-control review: the reviewer must differ from
	// whoever submitted the output for disposition.
	ErrReviewerCannotBeSubmitter = errorString("reviewer must differ from the principal who submitted this output for disposition")
	ErrUseCaseNotActiveForOutput = errorString("use case must be ACTIVE or LIMITED to submit an output for disposition")
)
