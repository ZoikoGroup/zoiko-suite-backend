package inbox

import (
	"errors"
	"fmt"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/policy"
)

// ErrDeliveryDenied means the residency policy refused to deliver the event to
// this consumer (ZS-EVENT-001 section 14, E-13, EV-T10). Nothing was written
// and the handler did not run.
var ErrDeliveryDenied = errors.New("inbox: delivery denied by the residency policy")

type deliveryGate struct {
	policy         *policy.Residency
	consumerRegion string
	audit          func(policy.Decision)
}

// SetDeliveryPolicy makes this consumer refuse an event that may not cross into
// its residency region. consumerRegion is the region this consumer runs in.
// audit, when set, is called with the Decision for every refusal so the control
// evidence is recorded (EV-T10); it must not block. With no policy set the
// inbox does no residency check, so an adopting service opts in. Call before
// serving.
func (i *Inbox) SetDeliveryPolicy(p *policy.Residency, consumerRegion string, audit func(policy.Decision)) {
	if p == nil {
		i.delivery = nil
		return
	}
	i.delivery = &deliveryGate{policy: p, consumerRegion: consumerRegion, audit: audit}
}

func (i *Inbox) checkDelivery(m Message) error {
	g := i.delivery
	if g == nil {
		return nil
	}
	d := g.policy.Decide(i.consumer, m.EventType, m.TenantID, m.Region, g.consumerRegion, envelope.Classification(m.Classification))
	if d.Allowed {
		return nil
	}
	if g.audit != nil {
		g.audit(d)
	}
	return fmt.Errorf("%w: %s", ErrDeliveryDenied, d.Reason)
}

// Observation is one finished delivery, for metrics (ZS-EVENT-001 section 17:
// consumer dedupe hit rate, failures, quarantines, denied and rejected
// deliveries). It carries no payload and no business data.
type Observation struct {
	Consumer  string
	EventType string
	TenantID  string
	Outcome   Outcome // zero when Err explains the failure
	// Replay is true for a delivery under a replay run.
	Replay bool
	// Quarantined is true when this delivery moved the receipt to quarantine.
	Quarantined bool
	// Denied is true when the residency policy refused it.
	Denied bool
	Err    error
}

// Observer receives every Process result. It runs on the delivery goroutine
// and must not block or panic.
type Observer func(Observation)

// SetObserver registers a metrics hook (for example a Prometheus counter
// increment). Call before serving.
func (i *Inbox) SetObserver(o Observer) { i.observer = o }

func (i *Inbox) observe(m Message, r Result, err error) {
	if i.observer == nil {
		return
	}
	i.observer(Observation{
		Consumer:    i.consumer,
		EventType:   m.EventType,
		TenantID:    m.TenantID,
		Outcome:     r.Outcome,
		Replay:      m.ReplayRunID != "",
		Quarantined: r.FailureState == StateQuarantine,
		Denied:      errors.Is(err, ErrDeliveryDenied),
		Err:         err,
	})
}
