package inbox

import (
	"github.com/jackc/pgx/v5"
	"context"
	"errors"
	"testing"
)

func replayMsg(id, run string) Message {
	m := msg(id)
	m.ReplayRunID = run
	return m
}

// EV-T11: a live (irreversible) consumer that receives a replay stream rejects
// it: the handler never runs and nothing is written. This is the default.
func TestReplayRejectedByDefault_EV_T11(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	res, err := in.Process(context.Background(), replayMsg("evt-r1", "run-1"), handler)
	if !errors.Is(err, ErrReplayRejected) || res.Outcome != Rejected {
		t.Fatalf("want ErrReplayRejected/Rejected, got %v / %v", err, res.Outcome)
	}
	if h.effects(t) != 0 {
		t.Fatal("a rejected replay must not run the handler")
	}
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_inbox`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a rejected replay must leave no receipt, got %d (%v)", n, err)
	}
}

// A rejected replay must not poison the live path: the same event arriving
// live afterwards is processed normally.
func TestRejectedReplayDoesNotAffectLive(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	_, _ = in.Process(context.Background(), replayMsg("evt-r2", "run-1"), handler)
	res, err := in.Process(context.Background(), msg("evt-r2"), handler)
	if err != nil || res.Outcome != Processed || h.effects(t) != 1 {
		t.Fatalf("live delivery after a rejected replay: %v %v effects=%d", res.Outcome, err, h.effects(t))
	}
}

// An opted-in read-model consumer applies the replay in an isolated namespace:
// replaying an event the live path already processed runs the handler again,
// and the live receipt is untouched.
func TestReplayRunsInIsolatedNamespace(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	in.AllowReplay()
	ctx := context.Background()
	if r, err := in.Process(ctx, msg("evt-r3"), handler); err != nil || r.Outcome != Processed {
		t.Fatal(r.Outcome, err)
	}
	r, err := in.Process(ctx, replayMsg("evt-r3", "run-1"), handler)
	if err != nil || r.Outcome != Processed {
		t.Fatalf("replay should be processed in its own namespace: %v %v", r.Outcome, err)
	}
	if h.effects(t) != 2 {
		t.Fatalf("expected live + replay effect, got %d", h.effects(t))
	}
	// The same run is deduplicated against itself.
	r, _ = in.Process(ctx, replayMsg("evt-r3", "run-1"), handler)
	if r.Outcome != Duplicate || h.effects(t) != 2 {
		t.Fatalf("a redelivered replay must dedupe within its run: %v effects=%d", r.Outcome, h.effects(t))
	}
	// A different run is a different namespace.
	r, _ = in.Process(ctx, replayMsg("evt-r3", "run-2"), handler)
	if r.Outcome != Processed {
		t.Fatalf("a second run must process again, got %v", r.Outcome)
	}
	// The live receipt is still the live one.
	rec, err := in.Get(ctx, "tenant-a", "evt-r3")
	if err != nil || rec.State != StateCompleted || rec.DuplicateCount != 0 {
		t.Fatalf("live receipt changed by replay: %+v %v", rec, err)
	}
}

// The handler can tell it is running under replay.
func TestReceiptCarriesReplayRun(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	in.AllowReplay()
	var got string
	_, err := in.Process(context.Background(), replayMsg("evt-r4", "run-7"), func(ctx context.Context, tx pgx.Tx, r Receipt) (string, error) {
		got = r.ReplayRunID
		return "", nil
	})
	if err != nil || got != "run-7" {
		t.Fatalf("receipt replay run = %q (%v)", got, err)
	}
}

func TestInvalidReplayRunRefused(t *testing.T) {
	in, err := New(PoolRunner{}, "c", DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	in.AllowReplay()
	for _, bad := range []string{"run 1", "../x", "a#b", "-x", string(make([]byte, 65))} {
		if _, err := in.Process(context.Background(), replayMsg("e", bad), handler); !errors.Is(err, ErrInvalidReplayRun) {
			t.Errorf("run %q: want ErrInvalidReplayRun, got %v", bad, err)
		}
	}
}

// Rejection needs no database at all.
func TestReplayRejectionNeedsNoDatabase(t *testing.T) {
	in, _ := New(PoolRunner{}, "c", DefaultConfig())
	res, err := in.Process(context.Background(), replayMsg("e", "run-1"), handler)
	if !errors.Is(err, ErrReplayRejected) || res.Outcome != Rejected {
		t.Fatalf("got %v / %v", res.Outcome, err)
	}
}
