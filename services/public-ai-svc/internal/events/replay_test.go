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

type recorder struct{ got []store.OutboxEvent }

func (r *recorder) Publish(_ context.Context, evs []store.OutboxEvent) error {
	r.got = append(r.got, evs...)
	return nil
}
func (r *recorder) Close() error { return nil }

type ledger struct {
	pointers map[string]string
	revoked  []string
	flags    []store.Flag
}

func (l ledger) Flags(context.Context) ([]store.Flag, error) { return l.flags, nil }

func (l ledger) GetPointer(_ context.Context, kind string) (string, error) {
	return l.pointers[kind], nil
}
func (l ledger) Revocations(context.Context, int64) ([]store.Revocation, error) {
	var out []store.Revocation
	for _, id := range l.revoked {
		out = append(out, store.Revocation{TargetKind: "source", TargetID: id})
	}
	return out, nil
}

// CT-042: a dead-letter replay rechecks authorization and freshness; only
// fresh, allowlisted envelopes of this service go back to their topic.
func TestDLQReplayRechecks(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st := ledger{pointers: map[string]string{"kb": "kb-current"}, revoked: []string{"src-revoked-v1"}}
	payload := func(ev store.OutboxEvent, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ev.Payload
	}
	pub := &recorder{}
	r := &Replayer{Store: st, Pub: pub, Now: func() time.Time { return now }}

	fresh := payload(TurnCompleted("req_1", "I02", "ANSWERED", time.Second, "kb-current", now.Add(-time.Hour)))
	current := payload(ReleasePromoted("kb-current", "sha256:x", []string{"a", "b"}, now.Add(-time.Hour)))
	revoked := payload(SourceRevoked("src-revoked-v1", "R", now.Add(-time.Hour)))

	stale := payload(TurnCompleted("req_2", "I02", "ANSWERED", time.Second, "kb-current", now.Add(-100*time.Hour)))
	oldRelease := payload(ReleasePromoted("kb-old", "sha256:y", []string{"a"}, now.Add(-time.Hour)))
	unknownRevocation := payload(SourceRevoked("src-never-revoked-v1", "R", now.Add(-time.Hour)))
	// A tampered envelope that tries to smuggle chat text through a replay.
	var env map[string]any
	_ = json.Unmarshal(fresh, &env)
	env["data"].(map[string]any)["message"] = "my payroll id is 12345"
	env["id"] = "tampered"
	tampered, _ := json.Marshal(env)
	foreign := []byte(`{"specversion":"1.0","id":"x1","source":"urn:other","type":"chat.turn.completed.v1","time":"2026-10-29T11:00:00Z","data":{}}`)

	if _, err := r.Replay(ctx, ReplayAuth{Actor: "sre-1"}, [][]byte{fresh}); !errors.Is(err, ErrReplayUnauthorized) {
		t.Fatalf("replay without approval must be refused, got %v", err)
	}
	if len(pub.got) != 0 {
		t.Fatal("nothing may be published without authorization")
	}

	rep, err := r.Replay(ctx, ReplayAuth{Actor: "sre-1", Approval: "CHG-42"},
		[][]byte{fresh, current, revoked, stale, oldRelease, unknownRevocation, tampered, foreign, []byte("not json")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Replayed) != 3 || len(pub.got) != 3 {
		t.Fatalf("want 3 replayed, got %v (rejected %+v)", rep.Replayed, rep.Rejected)
	}
	for _, e := range pub.got {
		if strings.HasSuffix(e.Topic, ".dlq") || e.Topic == "" {
			t.Fatalf("replayed to %q, want the original topic", e.Topic)
		}
	}
	reasons := map[string]string{}
	for _, rj := range rep.Rejected {
		reasons[rj.EventID] = rj.Reason
	}
	if len(rep.Rejected) != 6 {
		t.Fatalf("want 6 rejected, got %+v", rep.Rejected)
	}
	if !strings.Contains(reasons["tampered"], `field "message" is not allowed`) {
		t.Errorf("tampered envelope: %q", reasons["tampered"])
	}
	if !strings.Contains(reasons["x1"], "not an envelope produced by this service") {
		t.Errorf("foreign envelope: %q", reasons["x1"])
	}
	for _, r := range rep.Rejected {
		if strings.Contains(r.Reason, "12345") {
			t.Fatalf("a rejection reason echoes payload text: %q", r.Reason)
		}
	}
	want := []string{"stale: older than", "release is no longer the active one", "revocation is not in the ledger", "not a CloudEvents JSON envelope"}
	all := ""
	for _, r := range rep.Rejected {
		all += r.Reason + "|"
	}
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("missing rejection %q in %s", w, all)
		}
	}
}

// Doc 05 §22: a kill-switch event older than the flag's current change is
// never re-announced.
func TestReplayRejectsOlderControlEpoch(t *testing.T) {
	now := time.Now().UTC()
	old, _ := KillSwitchChanged("public_ai_enabled", "a", "b", "test", now.Add(-time.Hour))
	st := ledger{flags: []store.Flag{{Name: "public_ai_enabled", UpdatedAt: now.Add(-time.Minute)}}}
	r := &Replayer{Store: st}
	if _, _, err := r.check(context.Background(), old.Payload, now, 24*time.Hour); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("older control event replayed: %v", err)
	}
	current, _ := KillSwitchChanged("public_ai_enabled", "a", "b", "test", now.Add(-time.Minute))
	if _, _, err := r.check(context.Background(), current.Payload, now, 24*time.Hour); err != nil {
		t.Fatalf("current control event refused: %v", err)
	}
}

func TestEventIDsAreUUIDv4AndSourcesPerProducer(t *testing.T) {
	ev, _ := KillSwitchChanged("f", "a", "b", "e", time.Now())
	var env Envelope
	if err := json.Unmarshal(ev.Payload, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.ID) != 36 || env.ID[14] != '4' || env.Source != sourceControl {
		t.Fatalf("id %q source %q", env.ID, env.Source)
	}
	for i := 1; i <= 6; i++ {
		if d := Backoff(time.Second, i); d <= 0 || d > 2*time.Minute {
			t.Fatalf("backoff %d: %s", i, d)
		}
	}
}
