package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/store"
)

func TestEnvelopeShapeAndRedaction(t *testing.T) {
	ev, err := TurnCompleted("req_1", "I05", "ANSWERED", 3*time.Second, "kb-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(ev.Payload, &env); err != nil {
		t.Fatal(err)
	}
	if env["specversion"] != "1.0" || env["type"] != TypeTurnCompleted || env["id"] != ev.EventID || ev.Topic != TopicTelemetry {
		t.Fatalf("envelope: %v", env)
	}
	data := env["data"].(map[string]any)
	if data["latency_bucket"] != "2s-5s" || len(data) != 5 {
		t.Fatalf("data must hold exactly the 5 allowlisted fields: %v", data)
	}
}

func TestOnlySixEventTypes(t *testing.T) {
	now := time.Now()
	var types []string
	for _, f := range []func() (store.OutboxEvent, error){
		func() (store.OutboxEvent, error) { return TurnCompleted("r", "I01", "ANSWERED", 0, "kb", now) },
		func() (store.OutboxEvent, error) { return HandoffClicked("demo", "", false, now) },
		func() (store.OutboxEvent, error) { return ReleasePromoted("kb", "d", nil, now) },
		func() (store.OutboxEvent, error) { return SourceRevoked("s", "R", now) },
		func() (store.OutboxEvent, error) { return KillSwitchChanged("f", "a", "b", "e", now) },
		func() (store.OutboxEvent, error) { return SecuritySignal("PROMPT_INJECTION", "r", "MEDIUM", now) },
	} {
		ev, _ := f()
		types = append(types, ev.EventType)
	}
	want := "chat.turn.completed.v1,chat.handoff.clicked.v1,kb.release.promoted.v1,kb.source.revoked.v1,publicai.kill_switch.changed.v1,chat.security.signal.v1"
	if strings.Join(types, ",") != want {
		t.Fatalf("got %v", types)
	}
}

type flaky struct {
	fail  bool
	got   []store.OutboxEvent
	calls int
}

func (f *flaky) Publish(_ context.Context, evs []store.OutboxEvent) error {
	f.calls++
	if f.fail && !strings.HasSuffix(evs[0].Topic, ".dlq") {
		return errors.New("broker down")
	}
	f.got = append(f.got, evs...)
	return nil
}
func (f *flaky) Close() error { return nil }

func TestRelayDeliversOnceAndDivertsToDLQ(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	ev, _ := SourceRevoked("s1", "R", time.Now())
	_, _ = st.AddRevocation(ctx, store.Revocation{TargetKind: "source", TargetID: "s1", ReasonCode: "R"}, []store.OutboxEvent{ev})

	pub := &flaky{}
	r := &Relay{Store: st, Pub: pub, MaxAttempts: 3}
	if n, err := r.Once(ctx); err != nil || n != 1 {
		t.Fatalf("deliver: %d %v", n, err)
	}
	if n, _ := r.Once(ctx); n != 0 {
		t.Fatal("published events must not be re-sent")
	}

	ev2, _ := SourceRevoked("s2", "R", time.Now())
	_, _ = st.AddRevocation(ctx, store.Revocation{TargetKind: "source", TargetID: "s2", ReasonCode: "R"}, []store.OutboxEvent{ev2})
	pub.fail = true
	for i := 0; i < 3; i++ {
		_, _ = r.Once(ctx)
	}
	last := pub.got[len(pub.got)-1]
	if last.EventID != ev2.EventID || last.Topic != TopicGovernance+".dlq" {
		t.Fatalf("expected DLQ diversion, got %+v", last)
	}
	if pending, _ := st.UnpublishedEvents(ctx, 10); len(pending) != 0 {
		t.Fatal("DLQ'd event must leave the outbox")
	}
}
