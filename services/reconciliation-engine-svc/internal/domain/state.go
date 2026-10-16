package domain

import (
	"fmt"
	"strings"
	"time"
)

// A run carries three ORTHOGONAL state dimensions (ZS-CONTROL-001 §7, the
// pattern of financial-control-svc's state model). Lifecycle (Status) says how
// far the run has progressed — DATA-07's own sequence, Planned → Running →
// ExceptionsOpen → Reperformed → Certified / Failed, plus Superseded. Result
// says what the reconciliation found (ZS-SVC-Z-001 DQC-03). Certification says
// whether the result is sealed (ZS-SVC-Z-001 §5 certification dimension).
// None is derived from another except where an invariant below says so.

type ResultState string

const (
	ResultNotRun        ResultState = "NOT_RUN"
	ResultMatch         ResultState = "MATCH"
	ResultVariance      ResultState = "VARIANCE"
	ResultIndeterminate ResultState = "INDETERMINATE"
	// ResultPartial is part of the DQC-03 vocabulary but cannot be entered: no
	// spec defines what makes a run's population a "defined subset" (A16).
	ResultPartial    ResultState = "PARTIAL"
	ResultSuperseded ResultState = "SUPERSEDED"
)

type CertificationState string

// REVOKED is deliberately absent: revocation is A12 and out of scope here.
const (
	CertUncertified CertificationState = "UNCERTIFIED"
	CertEligible    CertificationState = "ELIGIBLE"
	CertCertified   CertificationState = "CERTIFIED"
	CertSuperseded  CertificationState = "SUPERSEDED"
)

// FailureCode says why a run could not determine a result. Stable codes, so a
// consumer never has to parse the reason text.
type FailureCode string

const (
	FailureSourceUnavailable  FailureCode = "SOURCE_UNAVAILABLE"
	FailureSourceUnregistered FailureCode = "SOURCE_UNREGISTERED"
	FailureSourceInconsistent FailureCode = "SOURCE_INCONSISTENT"
	FailurePopulationTooLarge FailureCode = "POPULATION_TOO_LARGE"
	FailurePopulationInvalid  FailureCode = "POPULATION_INVALID"
)

// RunFailure is why a run could not determine a result.
type RunFailure struct {
	Code   FailureCode
	Reason string
}

func (f RunFailure) Validate() error {
	switch f.Code {
	case FailureSourceUnavailable, FailureSourceUnregistered, FailureSourceInconsistent,
		FailurePopulationTooLarge, FailurePopulationInvalid:
	default:
		return fmt.Errorf("%w: unknown failure code %q", ErrInvalidArgument, f.Code)
	}
	if strings.TrimSpace(f.Reason) == "" {
		return fmt.Errorf("%w: a failure needs a reason", ErrInvalidArgument)
	}
	return nil
}

// lifecycleEdges is the complete set of legal lifecycle moves; anything not
// listed is refused. A self-edge is not listed: a command that leaves the
// lifecycle where it is (and changes only the result) is not a lifecycle move.
var lifecycleEdges = map[RunStatus][]RunStatus{
	// Planned → Failed: the populations could not be proven complete (scenario 09).
	RunPlanned:        {RunRunning, RunFailed},
	RunRunning:        {RunExceptionsOpen, RunReperformed, RunCertified, RunFailed},
	RunExceptionsOpen: {RunRunning, RunReperformed, RunFailed},
	RunReperformed:    {RunRunning, RunExceptionsOpen, RunCertified, RunFailed},
	// A certified or failed run is never edited; it can only be superseded.
	RunCertified:  {RunSuperseded},
	RunFailed:     {RunSuperseded},
	RunSuperseded: {},
}

// TerminalStatus reports whether a run can still be worked on: Certified and
// Failed can only be superseded, Superseded not at all.
func TerminalStatus(s RunStatus) bool {
	return s == RunCertified || s == RunFailed || s == RunSuperseded
}

// ValidateLifecycleTransition rejects illegal edges (from == to is "no move"
// and always allowed) and enforces the cross-dimension invariants of the
// target state.
func ValidateLifecycleTransition(from, to RunStatus, result ResultState, cert CertificationState) error {
	allowed, known := lifecycleEdges[from]
	if !known {
		return fmt.Errorf("%w: unknown run status %q", ErrInvalidTransition, from)
	}
	if from != to {
		ok := false
		for _, a := range allowed {
			if a == to {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%w: status %s -> %s", ErrInvalidTransition, from, to)
		}
	}
	// INV-26 / DQC-03: only a MATCH can be eligible for or carry certification.
	if (cert == CertEligible || cert == CertCertified) && result != ResultMatch {
		return fmt.Errorf("%w: certification %s requires result MATCH, not %s", ErrInvalidTransition, cert, result)
	}
	switch to {
	case RunPlanned:
		if result != ResultNotRun || cert != CertUncertified {
			return fmt.Errorf("%w: a Planned run has no result", ErrInvalidTransition)
		}
	case RunExceptionsOpen:
		if result != ResultVariance {
			return fmt.Errorf("%w: ExceptionsOpen requires result VARIANCE, not %s", ErrInvalidTransition, result)
		}
	case RunCertified:
		if cert != CertCertified {
			return fmt.Errorf("%w: cannot enter Certified without certification CERTIFIED", ErrInvalidTransition)
		}
	case RunFailed:
		// A run that could not determine a result is never a MATCH (INV-05, scenario 09).
		if result != ResultIndeterminate {
			return fmt.Errorf("%w: a Failed run must be INDETERMINATE, not %s", ErrInvalidTransition, result)
		}
	case RunSuperseded:
		if result != ResultSuperseded {
			return fmt.Errorf("%w: a Superseded run's result is SUPERSEDED", ErrInvalidTransition)
		}
	}
	if to != RunSuperseded && (result == ResultSuperseded || cert == CertSuperseded) {
		return fmt.Errorf("%w: SUPERSEDED requires status Superseded", ErrInvalidTransition)
	}
	if to != RunCertified && cert == CertCertified {
		return fmt.Errorf("%w: certification CERTIFIED requires status Certified", ErrInvalidTransition)
	}
	return nil
}

// ValidateResultTransition governs the finding. from == to is "no change".
// fromStatus is the run's status before the move.
func ValidateResultTransition(fromStatus RunStatus, from, to ResultState) error {
	if from == to {
		return nil
	}
	switch to {
	case ResultNotRun:
		return fmt.Errorf("%w: a result cannot be reset to NOT_RUN", ErrInvalidTransition)
	case ResultPartial:
		return fmt.Errorf("%w: PARTIAL has no defined subset semantics yet (SPEC_DEVIATIONS A16)", ErrInvalidTransition)
	case ResultSuperseded:
		// Supersession is the only move out of a terminal run.
		return nil
	}
	if TerminalStatus(fromStatus) {
		return fmt.Errorf("%w: the result of a %s run is frozen", ErrInvalidTransition, fromStatus)
	}
	// INV-05: INDETERMINATE can never be coerced to MATCH (or anything but SUPERSEDED).
	if from == ResultIndeterminate {
		return fmt.Errorf("%w: INDETERMINATE cannot become %s", ErrInvalidTransition, to)
	}
	return nil
}

var certificationEdges = map[CertificationState][]CertificationState{
	CertUncertified: {CertEligible},
	CertEligible:    {CertUncertified, CertCertified},
	CertCertified:   {CertSuperseded},
	CertSuperseded:  {},
}

// ValidateCertificationTransition: from == to is "no change".
func ValidateCertificationTransition(from, to CertificationState) error {
	if from == to {
		return nil
	}
	for _, a := range certificationEdges[from] {
		if a == to {
			return nil
		}
	}
	return fmt.Errorf("%w: certification %s -> %s", ErrInvalidTransition, from, to)
}

// EvaluatedResult is the result of an evaluated, still-open run: MATCH when no
// exception is open, otherwise VARIANCE. Every population of an open run was
// proven complete when it was frozen (B7), so "evidence is complete" holds.
func EvaluatedResult(openExceptions int64) ResultState {
	if openExceptions > 0 {
		return ResultVariance
	}
	return ResultMatch
}

// CertificationFor is the certification state an open run's result implies:
// ELIGIBLE exactly while the result is MATCH.
func CertificationFor(result ResultState) CertificationState {
	if result == ResultMatch {
		return CertEligible
	}
	return CertUncertified
}

// RunTransition is one append-only history row: one dimension that changed.
type RunTransition struct {
	TransitionID int64     `json:"transition_id"`
	RunID        string    `json:"run_id"`
	Dimension    string    `json:"dimension"`
	FromState    string    `json:"from_state"`
	ToState      string    `json:"to_state"`
	Reason       string    `json:"reason"`
	Actor        string    `json:"actor"`
	OccurredAt   time.Time `json:"occurred_at"`
}
