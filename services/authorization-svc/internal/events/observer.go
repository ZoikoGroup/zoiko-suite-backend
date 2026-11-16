package events

import (
	"encoding/json"
	"time"
)

// ProjectionObserver receives one call per consumed event this service applied
// (ZS-IAM-001 §26 "revocation lag": the time from an upstream disable/revoke to
// the PDP knowing about it). *telemetry.AuthzMetrics implements it.
type ProjectionObserver interface {
	ObserveProjection(consumer, eventType string, lag time.Duration, hasLag bool)
}

// Consumer names on the projection metrics.
const (
	ObserverDelegation = "delegation"
	ObserverLifecycle  = "lifecycle"
)

// observeApplied reports an applied message to o when its event type is one
// the consumer acts on. The lag is measured from the envelope's emitted_at (or
// occurred_at), when the producer stamped one.
func observeApplied(o ProjectionObserver, consumer string, raw []byte, interesting func(string) bool, now time.Time) {
	if o == nil {
		return
	}
	var env struct {
		EventType  string     `json:"event_type"`
		EmittedAt  *time.Time `json:"emitted_at,omitempty"`
		OccurredAt *time.Time `json:"occurred_at,omitempty"`
	}
	if json.Unmarshal(raw, &env) != nil || !interesting(env.EventType) {
		return
	}
	at := env.EmittedAt
	if at == nil {
		at = env.OccurredAt
	}
	if at == nil || at.IsZero() {
		o.ObserveProjection(consumer, env.EventType, 0, false)
		return
	}
	o.ObserveProjection(consumer, env.EventType, now.Sub(*at), true)
}

// SetObserver wires projection telemetry into the delegation consumer.
func (c *Consumer) SetObserver(o ProjectionObserver) { c.observer = o }

// SetObserver wires projection telemetry into the lifecycle consumer.
func (c *LifecycleConsumer) SetObserver(o ProjectionObserver) { c.observer = o }

func (c *LifecycleConsumer) interesting(eventType string) bool {
	return principalStatusEvents[eventType] || grantGraphEvents[eventType] || eventType == EventSubjectMerged
}
