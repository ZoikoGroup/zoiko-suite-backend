package certwatch

import "time"

// Observer receives the certification watch's operational signals. It is defined here, where the signals arise, and implemented by
// internal/telemetry (as the kernel's Observer is); a nil Observer is a no-op, so a caller that does not care wires nothing.
//
// What is measured, and why each is worth an alert (deployments/prometheus-rules.yml, group gl-certification-watch):
//
//   - the schedule: that it RUNS (last success against the configured interval), and whether it is being refused (a principal not
//     granted the sweep permission) or failing (a store error, a certification service that cannot be reached);
//   - the flags: that some were raised (journals rest on revoked evidence and a person must decide), and how long the oldest has waited;
//   - the event consumer: that events are being handled, and that the broker is readable.
//
// Nothing here carries a tenant, an entity, a certification or a journal: labels are closed sets, so cardinality cannot grow with data.
type Observer interface {
	// SweepScheduled reports the configured interval once at start (zero: the schedule is off).
	SweepScheduled(interval time.Duration)
	// SweepRun is one scheduled pass: outcome is ok (nothing skipped or unchecked), partial (a scope skipped or a certification could
	// not be checked), error (the pass itself could not run: lock, scan) or locked (another replica holds the sweep).
	SweepRun(outcome string, elapsed time.Duration)
	// SweepScope is one scope of a pass: swept, unauthorized (the principal may not sweep it) or error (its sweep failed).
	SweepScope(result string)
	// CertificationsUnchecked counts certifications a pass could not check.
	CertificationsUnchecked(n int)
	// FlagsRaised counts new flags, by the route that found them: schedule, event or manual.
	FlagsRaised(trigger string, n int)
	// WithdrawalEvent is one delivery: handled, irrelevant, malformed (this service's but unreadable) or failed (after the retries).
	WithdrawalEvent(result string)
	// WithdrawalReaction is one entity's response to a withdrawal event: swept, unauthorized, unwatched or failed.
	WithdrawalReaction(result string)
	// ConsumerError is a broker problem: read or commit.
	ConsumerError(kind string)
}

// Outcomes, results and triggers: the closed label sets.
const (
	RunOK      = "ok"
	RunPartial = "partial"
	RunError   = "error"
	RunLocked  = "locked"

	ScopeSwept        = "swept"
	ScopeUnauthorized = "unauthorized"
	ScopeError        = "error"

	TriggerSchedule = "schedule"
	TriggerEvent    = "event"
	TriggerManual   = "manual"

	EventHandled    = "handled"
	EventIrrelevant = "irrelevant"
	EventMalformed  = "malformed"
	EventFailed     = "failed"

	ReactionSwept        = "swept"
	ReactionUnauthorized = "unauthorized"
	ReactionUnwatched    = "unwatched"
	ReactionFailed       = "failed"

	ErrRead   = "read"
	ErrCommit = "commit"
)

type noopObserver struct{}

func (noopObserver) SweepScheduled(time.Duration)   {}
func (noopObserver) SweepRun(string, time.Duration) {}
func (noopObserver) SweepScope(string)              {}
func (noopObserver) CertificationsUnchecked(int)    {}
func (noopObserver) FlagsRaised(string, int)        {}
func (noopObserver) WithdrawalEvent(string)         {}
func (noopObserver) WithdrawalReaction(string)      {}
func (noopObserver) ConsumerError(string)           {}

// orNoop makes a nil Observer safe to call.
func orNoop(o Observer) Observer {
	if o == nil {
		return noopObserver{}
	}
	return o
}

// OpenFlagStats is what the open-flag gauges are read from at scrape time.
type OpenFlagStats struct {
	Open   int
	Oldest time.Duration // zero when nothing is open
}
