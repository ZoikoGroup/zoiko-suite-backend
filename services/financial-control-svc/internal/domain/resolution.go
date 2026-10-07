package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Errors specific to exception resolution.
const (
	ErrNotOwner       = errorString("only the exception's assigned owner can work it; reassign it to change owner")
	ErrNotIndependent = errorString("this decision must be taken by someone independent of the exception's owner and remediator")
)

// Actions checked against authorization-svc for exception resolution.
const (
	ActionResolve  = "FINCTRL_EXCEPTION_RESOLVE"
	ActionWaive    = "FINCTRL_EXCEPTION_WAIVE"
	ActionReperfrm = "FINCTRL_REPERFORM"
)

var carryPeriodRe = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)
var rootCauseRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,47}$`)

// ResolveExceptionRequest moves an exception along its governed lifecycle (§21).
type ResolveExceptionRequest struct {
	ToState       string `json:"to_state"`
	Reason        string `json:"reason"`
	AuthorityRef  string `json:"authority_ref"`   // waiver / carry-forward: who or what authorised it
	EvidenceRef   string `json:"evidence_ref"`    // remediation / reperformance: the adjustment or rerun
	CarryToPeriod string `json:"carry_to_period"` // carry-forward: YYYY-MM
	// Root cause: required to REMEDIATE, WAIVE or CARRY FORWARD an exception that is HIGH severity
	// or recurrent (s21). Code shape is validated; the taxonomy is a controlled decision (s34).
	RootCauseCode string `json:"root_cause_code"`
	RootCauseNote string `json:"root_cause_note"`
}

// Validate checks the request is complete for its target state. Whether the edge
// itself is legal is decided by ValidateExceptionTransition, not here.
func (r *ResolveExceptionRequest) Validate() error {
	bad := func(m string) error { return fmt.Errorf("%w: %s", ErrInvalidArgument, m) }
	to := ExceptionState(r.ToState)
	switch to {
	case ExInvestigating, ExAwaitingEvidence, ExAwaitingAdjustment, ExRemediated, ExReperformed, ExClosed, ExWaived, ExCarriedForward:
	default:
		return bad("to_state must be one of INVESTIGATING, AWAITING_EVIDENCE, AWAITING_ADJUSTMENT, REMEDIATED, REPERFORMED, CLOSED, WAIVED_UNDER_AUTHORITY, CARRIED_FORWARD_UNDER_AUTHORITY (ASSIGNED is the assign command)")
	}
	if strings.TrimSpace(r.Reason) == "" {
		return bad("reason is required")
	}
	if len(r.Reason) > 2000 || len(r.AuthorityRef) > 500 || len(r.EvidenceRef) > 500 {
		return bad("reason, authority_ref or evidence_ref is too long")
	}
	if r.RootCauseCode != "" && !rootCauseRe.MatchString(r.RootCauseCode) {
		return bad("root_cause_code must be UPPER_SNAKE_CASE, at most 48 characters")
	}
	if len(r.RootCauseNote) > 2000 {
		return bad("root_cause_note is too long")
	}
	switch to {
	case ExRemediated, ExReperformed:
		if strings.TrimSpace(r.EvidenceRef) == "" {
			return bad("evidence_ref is required: name the adjustment (remediation) or the rerun (reperformance)")
		}
	case ExWaived:
		if strings.TrimSpace(r.AuthorityRef) == "" {
			return bad("authority_ref is required: a waiver nobody owns is a way to make a control pass")
		}
	case ExCarriedForward:
		if strings.TrimSpace(r.AuthorityRef) == "" {
			return bad("authority_ref is required for a carry-forward")
		}
		if !carryPeriodRe.MatchString(r.CarryToPeriod) {
			return bad("carry_to_period must be YYYY-MM")
		}
	}
	return nil
}

// RequiredAction is the authorization action the target state demands.
func (r *ResolveExceptionRequest) RequiredAction() string {
	switch ExceptionState(r.ToState) {
	case ExWaived, ExCarriedForward:
		return ActionWaive
	case ExReperformed:
		return ActionReperfrm
	}
	return ActionResolve
}

// CheckIndependence enforces maker-checker on the resolution path:
//   - working the exception (investigate, await, remediate) is the assigned owner's job;
//   - a waiver or carry-forward cannot be granted by the owner it excuses;
//   - reperformance cannot be done by the owner or by whoever recorded the remediation.
func CheckIndependence(to ExceptionState, actor string, owner *string, remediator string) error {
	own := ""
	if owner != nil {
		own = *owner
	}
	switch to {
	case ExInvestigating, ExAwaitingEvidence, ExAwaitingAdjustment, ExRemediated:
		if own == "" || actor != own {
			return ErrNotOwner
		}
	case ExWaived, ExCarriedForward:
		if actor == own {
			return ErrNotIndependent
		}
	case ExReperformed:
		if actor == own || (remediator != "" && actor == remediator) {
			return ErrNotIndependent
		}
	}
	return nil
}

// RollupOutcome decides where a run in EXCEPTION_REVIEW goes once every exception is
// terminal: any waiver/carry-forward makes it PASS_WITH_APPROVED_EXCEPTIONS, otherwise
// every finding was fixed and reperformed and the run is PASS.
func RollupOutcome(unresolved, approved int) (ResultState, bool) {
	if unresolved > 0 {
		return ResultNotEvaluated, false
	}
	if approved > 0 {
		return ResultPassWithApprovedEx, true
	}
	return ResultPass, true
}

// ExceptionTransition is one row of an exception's append-only history.
type ExceptionTransition struct {
	From          string    `json:"from_state"`
	To            string    `json:"to_state"`
	Reason        string    `json:"reason"`
	ActorID       string    `json:"actor_id"`
	CorrelationID string    `json:"correlation_id"`
	AuthorityRef  string    `json:"authority_ref,omitempty"`
	EvidenceRef   string    `json:"evidence_ref,omitempty"`
	CarryToPeriod string    `json:"carry_to_period,omitempty"`
	RootCauseCode string    `json:"root_cause_code,omitempty"`
	RootCauseNote string    `json:"root_cause_note,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`
}

// RootCauseRequired reports whether resolving or excusing this exception needs a root cause:
// significant (HIGH severity) or recurrent exceptions do (s21).
func RootCauseRequired(to ExceptionState, sev Severity, recurrent bool) bool {
	switch to {
	case ExRemediated, ExWaived, ExCarriedForward:
		return sev == SeverityHigh || recurrent
	}
	return false
}

// CheckRootCause enforces RootCauseRequired against the request.
func (r *ResolveExceptionRequest) CheckRootCause(sev Severity, recurrent bool) error {
	if !RootCauseRequired(ExceptionState(r.ToState), sev, recurrent) {
		return nil
	}
	if r.RootCauseCode == "" || strings.TrimSpace(r.RootCauseNote) == "" {
		why := "is HIGH severity"
		if recurrent {
			why = "is a recurring exception"
		}
		return fmt.Errorf("%w: root_cause_code and root_cause_note are required: this exception %s", ErrInvalidArgument, why)
	}
	return nil
}
