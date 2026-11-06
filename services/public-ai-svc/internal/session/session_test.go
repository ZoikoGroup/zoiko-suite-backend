package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/store"
)

// Doc 06 §17: a session ends after its idle period even before the fixed
// maximum, and a completed turn keeps it alive.
func TestIdleExpiry(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	clock := time.Now().UTC()
	s := New(st, time.Hour).WithIdleTTL(10 * time.Minute)
	s.now = func() time.Time { return clock }
	issued, err := s.Create(ctx, "idle-test-key-01", []byte("h"), "en-US", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(9 * time.Minute)
	if _, err := s.Authenticate(ctx, issued.Grant); err != nil {
		t.Fatalf("within idle period: %v", err)
	}
	clock = clock.Add(2 * time.Minute)
	if _, err := s.Authenticate(ctx, issued.Grant); err != ErrExpired {
		t.Fatalf("idle session must expire, got %v", err)
	}
}

// Doc 05 §12: replaying a create-session Idempotency-Key is not a
// credential. It returns a grant only from the original origin and only
// within the idempotency window.
func TestIdempotentReplayBoundToOriginAndWindow(t *testing.T) {
	ctx := context.Background()
	const site, key = "https://www.zoikosuite.example", "replay-key-0001"
	cases := []struct {
		name   string
		origin string
		after  time.Duration
		want   error
	}{
		{"same origin, in window", site, time.Minute, nil},
		{"other origin", "https://evil.example", time.Minute, ErrMismatch},
		{"no origin", "", time.Minute, ErrMismatch},
		{"window passed", site, 3 * time.Hour, ErrMismatch},
	}
	for _, c := range cases {
		st := store.NewMemory()
		clock := time.Now().UTC()
		s := New(st, 4*time.Hour).WithIdempotencyTTL(2 * time.Hour)
		s.now = func() time.Time { return clock }
		first, err := s.Create(ctx, key, []byte("h"), "en-US", "", "", site)
		if err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(c.after)
		again, err := s.Create(ctx, key, []byte("h"), "en-US", "", "", c.origin)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
			continue
		}
		if c.want != nil {
			// The original grant still works: a refused replay rotates nothing.
			if _, err := s.Authenticate(ctx, first.Grant); err != nil {
				t.Errorf("%s: refused replay must not rotate the grant: %v", c.name, err)
			}
		} else if again.Session.SessionID != first.Session.SessionID {
			t.Errorf("%s: replay returned another session", c.name)
		}
	}
}
