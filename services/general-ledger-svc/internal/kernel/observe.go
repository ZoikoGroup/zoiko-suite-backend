package kernel

import "time"

// Operational signals of the accounting kernel (standard §38). The kernel reports what happened through an Observer so the
// signals can go to any metrics backend without the accounting code knowing which; nil means nothing is observed.
//
// An accounting-control failure (a refused posting, a quarantined event, a reconciliation break) is a different thing from an
// infrastructure failure (a dependency that is down, a database error), and the two are never counted under the same outcome.

// Command outcomes.
const (
	OutcomeOK         = "ok"         // the command did its work
	OutcomeExisting   = "existing"   // an idempotent repeat: the earlier result was returned (a duplicate-prevention hit)
	OutcomeRefused    = "refused"    // an accounting control said no: nothing was written
	OutcomeDependency = "dependency" // a dependency was unavailable: nothing was written, retry
	OutcomeError      = "error"      // an unexpected failure
)

// Observer receives the kernel's operational signals.
type Observer interface {
	// Command records one kernel command (register_event, post_event, reconciliation_run, period_seal, ...), its outcome and duration.
	Command(name, outcome string, d time.Duration)
	// Exception records an exception of the posting taxonomy (standard §34) by family and code.
	Exception(family, code string)
	// ReconciliationResult records the status a reconciliation run ended in.
	ReconciliationResult(reconType, status string)
}

// NopObserver observes nothing.
type NopObserver struct{}

func (NopObserver) Command(string, string, time.Duration) {}
func (NopObserver) Exception(string, string)              {}
func (NopObserver) ReconciliationResult(string, string)   {}
