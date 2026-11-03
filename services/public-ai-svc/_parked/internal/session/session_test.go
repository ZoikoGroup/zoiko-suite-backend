package session

import (
	"context"
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
	issued, err := s.Create(ctx, "idle-test-key-01", []byte("h"), "en-US", "", "")
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
