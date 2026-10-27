package idenv

import "time"

// Gate outcomes reported to an Observer.
const (
	OutcomeEnvelope    = "admitted_envelope"
	OutcomePeer        = "admitted_peer"
	OutcomeAsserted    = "admitted_asserted"
	OutcomeExempt      = "exempt"
	OutcomeInvalid     = "refused_invalid"
	OutcomeAudience    = "refused_audience"
	OutcomeRevoked     = "refused_revoked"
	OutcomeMismatch    = "refused_mismatch"
	OutcomeMissing     = "refused_missing"
	OutcomeUnbound     = "refused_sender_constraint"
	OutcomeUnavailable = "refused_unavailable"
)

// Observer receives the gate's measurements so a service can export them
// with its own metrics registry (IAM §26: revocation lag, token replay
// indicators). Every method must be safe for concurrent use.
type Observer interface {
	// Decision is one gate outcome (Outcome* constants).
	Decision(outcome string)
	// RevocationLag is the time from a session's revocation at
	// identity-context to the first refusal here of a session this relying
	// party had been admitting.
	RevocationLag(d time.Duration)
	// RevocationUnavailable is one revocation check identity-context could
	// not answer.
	RevocationUnavailable()
}

// NopObserver discards everything.
type NopObserver struct{}

func (NopObserver) Decision(string)             {}
func (NopObserver) RevocationLag(time.Duration) {}
func (NopObserver) RevocationUnavailable()      {}

// FuncObserver adapts funcs to Observer; nil members are skipped.
type FuncObserver struct {
	OnDecision    func(outcome string)
	OnLag         func(d time.Duration)
	OnUnavailable func()
}

func (f FuncObserver) Decision(o string) {
	if f.OnDecision != nil {
		f.OnDecision(o)
	}
}

func (f FuncObserver) RevocationLag(d time.Duration) {
	if f.OnLag != nil {
		f.OnLag(d)
	}
}

func (f FuncObserver) RevocationUnavailable() {
	if f.OnUnavailable != nil {
		f.OnUnavailable()
	}
}
