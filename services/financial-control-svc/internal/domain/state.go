package domain

import "fmt"

// The run carries three ORTHOGONAL state dimensions (ZS-CONTROL-001 §7).
// Lifecycle says how far the control has progressed; Result says what the
// control found; Certification says whether an accountable owner signed off.
// None of them is derived from another, except where an invariant says so.

type LifecycleState string

const (
	LifecycleScheduled       LifecycleState = "SCHEDULED"
	LifecyclePreparing       LifecycleState = "PREPARING"
	LifecyclePopulationFroze LifecycleState = "POPULATION_FROZEN"
	LifecycleExecuting       LifecycleState = "EXECUTING"
	LifecycleExceptionReview LifecycleState = "EXCEPTION_REVIEW"
	LifecycleRemediation     LifecycleState = "REMEDIATION"
	LifecycleReperformance   LifecycleState = "REPERFORMANCE"
	LifecycleReadyToCertify  LifecycleState = "READY_TO_CERTIFY"
	LifecycleCertified       LifecycleState = "CERTIFIED"
	LifecycleFailed          LifecycleState = "FAILED"
	LifecycleExpired         LifecycleState = "EXPIRED"
	LifecycleSuperseded      LifecycleState = "SUPERSEDED"
)

type ResultState string

const (
	ResultNotEvaluated       ResultState = "NOT_EVALUATED"
	ResultPass               ResultState = "PASS"
	ResultPassWithApprovedEx ResultState = "PASS_WITH_APPROVED_EXCEPTIONS"
	ResultFail               ResultState = "FAIL"
	ResultIndeterminate      ResultState = "INDETERMINATE"
)

type CertificationState string

const (
	CertNotRequired CertificationState = "NOT_REQUIRED"
	CertPending     CertificationState = "PENDING"
	CertCertified   CertificationState = "CERTIFIED"
	CertRejected    CertificationState = "REJECTED"
	CertRevoked     CertificationState = "REVOKED"
	CertSuperseded  CertificationState = "SUPERSEDED"
)

// lifecycleEdges is the complete set of legal lifecycle transitions. Anything
// not listed is refused: there is no "set status" escape hatch (Invariant 10 —
// no front-end or admin state edit can move a run).
var lifecycleEdges = map[LifecycleState][]LifecycleState{
	LifecycleScheduled:       {LifecyclePreparing, LifecycleExpired, LifecycleFailed},
	LifecyclePreparing:       {LifecyclePopulationFroze, LifecycleExpired, LifecycleFailed},
	LifecyclePopulationFroze: {LifecycleExecuting, LifecycleFailed},
	LifecycleExecuting:       {LifecycleExceptionReview, LifecycleReadyToCertify, LifecycleFailed},
	LifecycleExceptionReview: {LifecycleRemediation, LifecycleReadyToCertify, LifecycleFailed},
	LifecycleRemediation:     {LifecycleReperformance, LifecycleExceptionReview, LifecycleFailed},
	LifecycleReperformance:   {LifecycleExecuting, LifecycleFailed},
	// A rejected certification returns the run to controlled workflow (§26).
	LifecycleReadyToCertify: {LifecycleCertified, LifecycleExceptionReview, LifecycleSuperseded, LifecycleFailed},
	// A certified run is never edited; it can only be superseded (Invariant 2).
	LifecycleCertified:  {LifecycleSuperseded},
	LifecycleFailed:     {},
	LifecycleExpired:    {},
	LifecycleSuperseded: {},
}

// TerminalLifecycle reports whether no further lifecycle movement is possible.
func TerminalLifecycle(s LifecycleState) bool { return len(lifecycleEdges[s]) == 0 }

// ValidateLifecycleTransition rejects illegal edges and enforces the
// cross-dimension invariants for the target state.
func ValidateLifecycleTransition(from, to LifecycleState, result ResultState, cert CertificationState) error {
	allowed, known := lifecycleEdges[from]
	if !known {
		return fmt.Errorf("%w: unknown lifecycle state %q", ErrInvalidTransition, from)
	}
	ok := false
	for _, a := range allowed {
		if a == to {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("%w: lifecycle %s -> %s", ErrInvalidTransition, from, to)
	}
	switch to {
	case LifecycleCertified:
		// Certification moves only through the certify command, over a passing result.
		if cert != CertCertified {
			return fmt.Errorf("%w: cannot enter CERTIFIED without certification_state CERTIFIED", ErrInvalidTransition)
		}
		if result != ResultPass && result != ResultPassWithApprovedEx {
			return fmt.Errorf("%w: cannot certify result %s", ErrInvalidTransition, result)
		}
	case LifecycleReadyToCertify:
		// Invariant 5/18: a run whose result is Fail, Indeterminate or Not Evaluated is not certifiable.
		if result != ResultPass && result != ResultPassWithApprovedEx {
			return fmt.Errorf("%w: cannot be READY_TO_CERTIFY with result %s", ErrInvalidTransition, result)
		}
	}
	return nil
}

// ValidateResultTransition governs the finding. A result is set by execution
// or reperformance and may be revised by reperformance; PASS is unreachable
// from a technical failure (scenario 09: outage => Indeterminate/Failed, never Pass).
func ValidateResultTransition(lifecycle LifecycleState, from, to ResultState) error {
	if from == to {
		return fmt.Errorf("%w: result already %s", ErrInvalidTransition, from)
	}
	switch lifecycle {
	case LifecycleFailed, LifecycleExpired, LifecycleSuperseded, LifecycleCertified:
		return fmt.Errorf("%w: result is frozen in lifecycle %s", ErrInvalidTransition, lifecycle)
	case LifecycleScheduled, LifecyclePreparing, LifecyclePopulationFroze:
		return fmt.Errorf("%w: no result may be recorded before execution (%s)", ErrInvalidTransition, lifecycle)
	}
	if to == ResultNotEvaluated {
		return fmt.Errorf("%w: a result cannot be reset to NOT_EVALUATED", ErrInvalidTransition)
	}
	return nil
}

var certificationEdges = map[CertificationState][]CertificationState{
	CertNotRequired: {CertPending},
	CertPending:     {CertCertified, CertRejected},
	CertRejected:    {CertPending},
	CertCertified:   {CertRevoked, CertSuperseded},
	CertRevoked:     {},
	CertSuperseded:  {},
}

func ValidateCertificationTransition(from, to CertificationState) error {
	for _, a := range certificationEdges[from] {
		if a == to {
			return nil
		}
	}
	return fmt.Errorf("%w: certification %s -> %s", ErrInvalidTransition, from, to)
}

// ─── Exception state model (§7) — used from Wave 1 onward ────────────────────

type ExceptionState string

const (
	ExOpen               ExceptionState = "OPEN"
	ExAssigned           ExceptionState = "ASSIGNED"
	ExInvestigating      ExceptionState = "INVESTIGATING"
	ExAwaitingEvidence   ExceptionState = "AWAITING_EVIDENCE"
	ExAwaitingAdjustment ExceptionState = "AWAITING_ADJUSTMENT"
	ExRemediated         ExceptionState = "REMEDIATED"
	ExReperformed        ExceptionState = "REPERFORMED"
	ExWaived             ExceptionState = "WAIVED_UNDER_AUTHORITY"
	ExCarriedForward     ExceptionState = "CARRIED_FORWARD_UNDER_AUTHORITY"
	ExClosed             ExceptionState = "CLOSED"
)

var exceptionEdges = map[ExceptionState][]ExceptionState{
	ExOpen:               {ExAssigned},
	ExAssigned:           {ExInvestigating, ExAssigned},
	ExInvestigating:      {ExAwaitingEvidence, ExAwaitingAdjustment, ExWaived, ExCarriedForward},
	ExAwaitingEvidence:   {ExInvestigating},
	ExAwaitingAdjustment: {ExRemediated, ExInvestigating},
	// Invariant/§21: an exception is control-resolved only after reperformance
	// (or a documented alternate procedure); there is no Remediated -> Closed edge.
	ExRemediated:     {ExReperformed, ExInvestigating},
	ExReperformed:    {ExClosed, ExInvestigating},
	ExWaived:         {},
	ExCarriedForward: {},
	ExClosed:         {},
}

func ValidateExceptionTransition(from, to ExceptionState) error {
	for _, a := range exceptionEdges[from] {
		if a == to {
			return nil
		}
	}
	return fmt.Errorf("%w: exception %s -> %s", ErrInvalidTransition, from, to)
}
