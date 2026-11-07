package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/store"
)

func dataOf(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	return env.Data
}

// Doc 05 §18/§22: a revocation event carries the ledger watermark the store
// assigned in the same transaction; a promotion carries its kind and epoch.
func TestEventsCarryEpochsAndKind(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	_, _ = st.AddRevocation(ctx, store.Revocation{TargetKind: "source", TargetID: "s0"}, nil)
	ev, _ := SourceRevoked("s1-v1", "R", time.Now())
	wm, err := st.AddRevocation(ctx, store.Revocation{TargetKind: "source", TargetID: "s1-v1", ReasonCode: "R"}, []store.OutboxEvent{ev})
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := st.UnpublishedEvents(ctx, 10)
	if len(pending) != 1 || dataOf(t, pending[0].Payload)["revocation_epoch"] != float64(wm) || wm != 2 {
		t.Fatalf("revocation_epoch not stamped: wm=%d %s", wm, pending[0].Payload)
	}

	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	p, _ := ReleasePromotedFor("claims", "rel-7", "sha256:x", []string{"a", "b"}, at)
	d := dataOf(t, p.Payload)
	if d["kind"] != "claims" || d["release_epoch"] != float64(at.UnixNano()) {
		t.Fatalf("promotion data: %v", d)
	}
}

// Doc 05 §22: replay reads the release kind from the event, not the id
// prefix, and a revocation replays only against its own ledger entry.
func TestReplayUsesKindAndEpoch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st := store.NewMemory()
	_ = st.PutRelease(ctx, store.Release{ReleaseID: "rel-7", Kind: "claims", Manifest: []byte("{}")})
	if err := st.SetPointer(ctx, "claims", "rel-7", nil); err != nil { // id with no "claims" prefix
		t.Fatal(err)
	}
	wm, _ := st.AddRevocation(ctx, store.Revocation{TargetKind: "source", TargetID: "src-v1"}, nil)
	r := &Replayer{Store: st, Pub: &recorder{}, Now: func() time.Time { return now }}
	auth := ReplayAuth{Actor: "sre", Approval: "CHG-1"}

	promoted, _ := ReleasePromotedFor("claims", "rel-7", "d", []string{"a", "b"}, now.Add(-time.Hour))
	wrongKind, _ := ReleasePromotedFor("kb", "rel-7", "d", []string{"a", "b"}, now.Add(-time.Hour))
	badKind, _ := ReleasePromotedFor("other", "rel-7", "d", []string{"a", "b"}, now.Add(-time.Hour))
	rev, _ := SourceRevoked("src-v1", "R", now.Add(-time.Hour))
	setEpoch := func(payload []byte, wm int64) []byte {
		return []byte(strings.Replace(string(payload), `"revocation_epoch":0`, `"revocation_epoch":`+itoa(wm), 1))
	}
	cases := []struct {
		name    string
		payload []byte
		ok      bool
	}{
		{"kind from event", promoted.Payload, true},
		{"kind mismatch", wrongKind.Payload, false},
		{"unknown kind", badKind.Payload, false},
		{"own ledger entry", setEpoch(rev.Payload, wm), true},
		{"other ledger entry", setEpoch(rev.Payload, wm+5), false},
	}
	for _, c := range cases {
		rep, err := r.Replay(ctx, auth, [][]byte{c.payload})
		if err != nil {
			t.Fatal(err)
		}
		if (len(rep.Replayed) == 1) != c.ok {
			t.Errorf("%s: replayed=%v rejected=%v", c.name, rep.Replayed, rep.Rejected)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// Relay backoff is jittered within [d/2, d).
func TestJitter(t *testing.T) {
	d := 40 * time.Second
	for _, u := range []float64{0, 0.25, 0.999} {
		if j := Jitter(d, u); j < d/2 || j >= d {
			t.Fatalf("Jitter(%s, %v) = %s", d, u, j)
		}
	}
	if Jitter(d, 0) == Jitter(d, 0.5) {
		t.Fatal("jitter must vary with u")
	}
}
