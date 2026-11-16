package events

import (
	"testing"
	"time"
)

type recordingObserver struct {
	calls []struct {
		consumer, eventType string
		lag                 time.Duration
		hasLag              bool
	}
}

func (r *recordingObserver) ObserveProjection(consumer, eventType string, lag time.Duration, hasLag bool) {
	r.calls = append(r.calls, struct {
		consumer, eventType string
		lag                 time.Duration
		hasLag              bool
	}{consumer, eventType, lag, hasLag})
}

func TestObserveApplied(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 2, 0, time.UTC)
	o := &recordingObserver{}
	lc := &LifecycleConsumer{}

	observeApplied(o, ObserverLifecycle, []byte(`{"event_type":"principal.status.changed","emitted_at":"2026-11-10T12:00:00Z"}`), lc.interesting, now)
	observeApplied(o, ObserverLifecycle, []byte(`{"event_type":"role.updated"}`), lc.interesting, now)
	// Not an event the lifecycle consumer acts on: not observed.
	observeApplied(o, ObserverLifecycle, []byte(`{"event_type":"invoice.created","emitted_at":"2026-11-10T12:00:00Z"}`), lc.interesting, now)
	// Undecodable: not observed.
	observeApplied(o, ObserverLifecycle, []byte(`not json`), lc.interesting, now)
	// No observer: nothing happens, nothing panics.
	observeApplied(nil, ObserverLifecycle, []byte(`{"event_type":"role.updated"}`), lc.interesting, now)

	if len(o.calls) != 2 {
		t.Fatalf("calls = %d, want 2: %+v", len(o.calls), o.calls)
	}
	if c := o.calls[0]; c.eventType != "principal.status.changed" || !c.hasLag || c.lag != 2*time.Second {
		t.Fatalf("first call = %+v, want principal.status.changed with a 2s lag", c)
	}
	if c := o.calls[1]; c.eventType != "role.updated" || c.hasLag {
		t.Fatalf("second call = %+v, want role.updated with no lag", c)
	}
}
