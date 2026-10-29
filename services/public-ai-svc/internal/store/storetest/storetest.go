// Package storetest is the conformance suite every store.Store
// implementation must pass. Both Memory and Postgres run it.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/store"
)

// Run executes the suite. newStore must return an empty store each call.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("Sessions", func(t *testing.T) { testSessions(t, newStore(t)) })
	t.Run("Turns", func(t *testing.T) { testTurns(t, newStore(t)) })
	t.Run("Releases", func(t *testing.T) { testReleases(t, newStore(t)) })
	t.Run("Revocations", func(t *testing.T) { testRevocations(t, newStore(t)) })
	t.Run("FlagsUsageOutbox", func(t *testing.T) { testFlagsUsageOutbox(t, newStore(t)) })
	t.Run("FeedbackPurge", func(t *testing.T) { testFeedbackPurge(t, newStore(t)) })
}

var ctx = context.Background()

func mkSession(t *testing.T, s store.Store, id string) store.Session {
	t.Helper()
	sess := store.Session{
		SessionID: id, GrantHash: []byte("grant-" + id),
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
		ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond),
		Locale:    "en-US",
	}
	if err := s.CreateSession(ctx, sess, "idem-"+id, []byte("h-"+id)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

func testSessions(t *testing.T, s store.Store) {
	sess := mkSession(t, s, "pub_s_aaaaaaaaaaaa")

	got, err := s.SessionByGrant(ctx, sess.GrantHash)
	if err != nil || got.SessionID != sess.SessionID {
		t.Fatalf("by grant: %v %+v", err, got)
	}
	byKey, hash, err := s.SessionByIdempotencyKey(ctx, "idem-"+sess.SessionID)
	if err != nil || byKey.SessionID != sess.SessionID || !bytes.Equal(hash, []byte("h-"+sess.SessionID)) {
		t.Fatalf("by idempotency key: %v", err)
	}
	if _, _, err := s.SessionByIdempotencyKey(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}

	if err := s.RotateGrant(ctx, sess.SessionID, []byte("grant-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByGrant(ctx, sess.GrantHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("old grant must stop working after rotation")
	}
	if _, err := s.SessionByGrant(ctx, []byte("grant-2")); err != nil {
		t.Fatal("new grant must work")
	}

	if err := s.SetPendingContext(ctx, sess.SessionID, []byte(`{"intent":"I05"}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SessionByGrant(ctx, []byte("grant-2")); string(got.PendingContext) != `{"intent":"I05"}` {
		t.Fatalf("pending context: %q", got.PendingContext)
	}
	if err := s.DeleteSession(ctx, sess.SessionID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if del, _ := s.SessionByGrant(ctx, []byte("grant-2")); del.PendingContext != nil {
		t.Fatal("delete must wipe the pending context")
	}
	if del, err := s.SessionByGrant(ctx, []byte("grant-2")); err != nil || del.DeletedAt == nil || del.Locale != "" {
		t.Fatalf("deleted session must be reported as deleted with context wiped: %v %+v", err, del)
	}
	if err := s.RotateGrant(ctx, sess.SessionID, []byte("grant-3")); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a deleted session's grant must not be rotated")
	}
	if _, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_d", "cd", "hd"), time.Now().Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a deleted session must not start turns")
	}
	if err := s.DeleteSession(ctx, sess.SessionID, time.Now()); err != nil {
		t.Fatalf("second delete of an existing session must succeed: %v", err)
	}
	if err := s.DeleteSession(ctx, "pub_s_missing00000", time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete unknown: %v", err)
	}
}

func turn(sess, turnID, client string, hash string) store.Turn {
	return store.Turn{TurnID: turnID, SessionID: sess, ClientTurnID: client, RequestHash: []byte(hash), CreatedAt: time.Now().UTC()}
}

func testTurns(t *testing.T, s store.Store) {
	sess := mkSession(t, s, "pub_s_bbbbbbbbbbbb")
	lock := time.Now().Add(time.Minute)

	r1, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_1", "c1", "h1"), lock)
	if err != nil || r1.Replay || r1.Turn.Sequence != 1 {
		t.Fatalf("first turn: %v %+v", err, r1)
	}
	// CT-011: a second turn while one is active conflicts.
	if _, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_2", "c2", "h2"), lock); !errors.Is(err, store.ErrTurnConflict) {
		t.Fatalf("concurrent turn: %v", err)
	}
	// Same key while still running also conflicts.
	if _, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_1b", "c1", "h1"), lock); !errors.Is(err, store.ErrTurnConflict) {
		t.Fatalf("same key while running: %v", err)
	}
	done := r1.Turn
	done.Status, done.IntentID, done.Response = "ABSTAIN", "I29", []byte(`{"status":"ABSTAIN"}`)
	ev := store.OutboxEvent{EventID: "ev-1", Topic: "publicai.telemetry.v1", EventType: "chat.turn.completed.v1", Payload: []byte(`{}`), CreatedAt: time.Now()}
	if err := s.CompleteTurn(ctx, done, []string{"day:x"}, 42, []store.OutboxEvent{ev}); err != nil {
		t.Fatal(err)
	}
	// Replay: same key + same payload returns the stored response, no new turn.
	r2, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_9", "c1", "h1"), lock)
	if err != nil || !r2.Replay || r2.Turn.TurnID != "pub_t_1" || string(r2.Turn.Response) != `{"status":"ABSTAIN"}` {
		t.Fatalf("replay: %v %+v", err, r2)
	}
	// Same key + different payload is a mismatch.
	if _, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_9", "c1", "DIFFERENT"), lock); !errors.Is(err, store.ErrIdempotencyMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	// Next new turn gets sequence 2.
	r3, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_3", "c3", "h3"), lock)
	if err != nil || r3.Turn.Sequence != 2 {
		t.Fatalf("second turn: %v %+v", err, r3)
	}
	// Abort releases the slot and forgets the turn so the same key can retry.
	if err := s.AbortTurn(ctx, sess.SessionID, "pub_t_3"); err != nil {
		t.Fatal(err)
	}
	r4, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_4", "c3", "h3"), lock)
	if err != nil || r4.Replay {
		t.Fatalf("retry after abort: %v %+v", err, r4)
	}
	// An expired lock does not block a new turn.
	if err := s.AbortTurn(ctx, sess.SessionID, "pub_t_4"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_5", "c5", "h5"), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_6", "c6", "h6"), lock); err != nil {
		t.Fatalf("expired lock should not block: %v", err)
	}
	// Turns on unknown sessions are rejected.
	if _, err := s.BeginTurn(ctx, turn("pub_s_nope00000000", "pub_t_x", "cx", "hx"), lock); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	if n, _ := s.Usage(ctx, "day:x"); n != 42 {
		t.Fatalf("usage = %d", n)
	}
}

func testFeedbackPurge(t *testing.T, s store.Store) {
	sess := mkSession(t, s, "pub_s_ffffffffffff")
	b, err := s.BeginTurn(ctx, turn(sess.SessionID, "pub_t_f1", "cf1", "h"), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetFeedback(ctx, sess.SessionID, "pub_t_f1", "useful", "", time.Now()); err == nil {
		t.Fatal("feedback on an unfinished turn must be refused")
	}
	done := b.Turn
	done.Status, done.Response = "ANSWERED", []byte(`{}`)
	if err := s.CompleteTurn(ctx, done, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFeedback(ctx, sess.SessionID, "pub_t_f1", "not_useful", "incorrect", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFeedback(ctx, "pub_s_other0000000", "pub_t_f1", "useful", "", time.Now()); err == nil {
		t.Fatal("another session must not rate this turn")
	}
	if err := s.AppendEvents(ctx, []store.OutboxEvent{{EventID: "ev-h", Topic: "t", EventType: "chat.handoff.clicked.v1", Payload: []byte(`{}`), CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	// Retention: a session expired before the cutoff is removed with its turns.
	if n, err := s.PurgeSessions(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("live session purged: %d %v", n, err)
	}
	if n, err := s.PurgeSessions(ctx, time.Now().Add(2*time.Hour)); err != nil || n != 1 {
		t.Fatalf("expired session not purged: %d %v", n, err)
	}
	if _, err := s.SessionByGrant(ctx, sess.GrantHash); err == nil {
		t.Fatal("purged session still present")
	}
}

func testReleases(t *testing.T, s store.Store) {
	kb := store.Release{ReleaseID: "kb-1", Kind: "kb", Manifest: []byte("m1"), Signature: []byte("s1"), CreatedAt: time.Now()}
	if err := s.PutRelease(ctx, kb); err != nil {
		t.Fatal(err)
	}
	// Immutable: a second write with the same id does not change content.
	_ = s.PutRelease(ctx, store.Release{ReleaseID: "kb-1", Kind: "kb", Manifest: []byte("tampered"), Signature: []byte("x"), CreatedAt: time.Now()})
	got, err := s.GetRelease(ctx, "kb-1")
	if err != nil || string(got.Manifest) != "m1" {
		t.Fatalf("release immutability: %v %q", err, got.Manifest)
	}
	if err := s.SetPointer(ctx, "claims", "kb-1", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("pointer must not point at a release of another kind")
	}
	if _, err := s.GetPointer(ctx, "kb"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("no pointer yet")
	}
	if err := s.SetPointer(ctx, "kb", "kb-1", nil); err != nil {
		t.Fatal(err)
	}
	_ = s.PutRelease(ctx, store.Release{ReleaseID: "kb-2", Kind: "kb", Manifest: []byte("m2"), Signature: []byte("s2"), CreatedAt: time.Now()})
	if err := s.SetPointer(ctx, "kb", "kb-2", nil); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.GetPointer(ctx, "kb"); id != "kb-2" {
		t.Fatalf("pointer = %s", id)
	}
}

func testRevocations(t *testing.T, s store.Store) {
	var last int64
	for i := 0; i < 3; i++ {
		wm, err := s.AddRevocation(ctx, store.Revocation{TargetKind: "source", TargetID: fmt.Sprintf("src-%d", i), ReasonCode: "TEST", EffectiveAt: time.Now(), CreatedAt: time.Now()}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if wm <= last {
			t.Fatalf("watermark not monotonic: %d after %d", wm, last)
		}
		last = wm
	}
	all, _ := s.Revocations(ctx, 0)
	if len(all) != 3 {
		t.Fatalf("got %d revocations", len(all))
	}
	newer, _ := s.Revocations(ctx, all[0].Watermark)
	if len(newer) != 2 {
		t.Fatalf("after first watermark: %d", len(newer))
	}
}

func testFlagsUsageOutbox(t *testing.T, s store.Store) {
	ev := func(id string) store.OutboxEvent {
		return store.OutboxEvent{EventID: id, Topic: "publicai.control.v1", EventType: "publicai.kill_switch.changed.v1", Payload: []byte(`{}`), CreatedAt: time.Now()}
	}
	f := store.Flag{Name: "public_ai_enabled", Value: []byte(`false`), ActorRef: "sre-1", ApprovalRef: "CHG-1", UpdatedAt: time.Now()}
	if err := s.SetFlag(ctx, f, []store.OutboxEvent{ev("e1")}); err != nil {
		t.Fatal(err)
	}
	f.Value = []byte(`true`)
	if err := s.SetFlag(ctx, f, []store.OutboxEvent{ev("e2"), ev("e1")}); err != nil {
		t.Fatal(err)
	}
	flags, _ := s.Flags(ctx)
	if len(flags) != 1 || string(flags[0].Value) != "true" {
		t.Fatalf("flags = %+v", flags)
	}
	pending, _ := s.UnpublishedEvents(ctx, 10)
	if len(pending) != 2 {
		t.Fatalf("duplicate event id must be stored once; got %d", len(pending))
	}
	if err := s.MarkAttempt(ctx, []int64{pending[0].OutboxID}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPublished(ctx, []int64{pending[0].OutboxID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.UnpublishedEvents(ctx, 10)
	if len(pending) != 1 || pending[0].EventID != "e2" {
		t.Fatalf("after publish: %+v", pending)
	}
	if n, _ := s.Usage(ctx, "never-used"); n != 0 {
		t.Fatal("unknown bucket must be 0")
	}
}
