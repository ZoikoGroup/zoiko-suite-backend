package store

import (
	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/events"
)

// EventsFor maps a state movement to its ZS-CONTROL-001 §26 events, in emission
// order. A movement may carry more than one contractual fact — finishing
// execution into READY_TO_CERTIFY is both "execution completed" and "ready for
// certification" — and none at all for purely internal moves.
func EventsFor(from, to domain.ControlRun) []string {
	var out []string
	switch {
	case to.LifecycleState == domain.LifecycleFailed && from.LifecycleState != domain.LifecycleFailed:
		return []string{events.RunFailed}
	case to.LifecycleState == domain.LifecycleCertified:
		return []string{events.RunCertified}
	case to.LifecycleState == domain.LifecycleSuperseded && from.LifecycleState != domain.LifecycleSuperseded:
		return []string{events.RunSuperseded}
	case to.LifecycleState == domain.LifecycleExpired && from.LifecycleState != domain.LifecycleExpired:
		return []string{events.RunExpired}
	case from.LifecycleState == domain.LifecycleReadyToCertify && to.CertificationState == domain.CertRejected:
		return []string{events.RunCertificationReject}
	case from.LifecycleState == domain.LifecycleReperformance && to.LifecycleState == domain.LifecycleExecuting:
		return []string{events.RunReperformed}
	case to.LifecycleState == domain.LifecyclePopulationFroze:
		return []string{events.PopulationFrozen}
	}
	if from.LifecycleState == domain.LifecycleExecuting &&
		(to.LifecycleState == domain.LifecycleExceptionReview || to.LifecycleState == domain.LifecycleReadyToCertify) {
		out = append(out, events.ExecutionCompleted)
	}
	if to.LifecycleState == domain.LifecycleReadyToCertify {
		out = append(out, events.RunReadyForCert)
	}
	return out
}
