package rotation

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

// stubCore records PerformRotation calls against a closure-controlled
// result, standing in for *handler.Handler.
type stubCore struct {
	err             error
	calls           atomic.Int32
	lastPolicyID    string
	lastRequestID   string
	lastActorID     string
	rotateOK        bool
	expectedPath    string
}

func (c *stubCore) PerformRotation(_ context.Context, secretPolicyID, rotatedByPrincipalID string, _ *string, requestID, _ string) (string, int, time.Time, error) {
	c.calls.Add(1)
	c.lastPolicyID = secretPolicyID
	c.lastRequestID = requestID
	c.lastActorID = rotatedByPrincipalID
	if c.err != nil {
		return "", 0, time.Time{}, c.err
	}
	return c.expectedPath, 0, time.Now(), nil
}

type stubStore struct {
	due      []*domain.DueRotation
	updates  atomic.Int32
	lastNext time.Time
	skipUpd  bool
	claimed  map[string]time.Time
}

func (s *stubStore) ListDueRotations(_ context.Context, _ time.Time) ([]*domain.DueRotation, error) {
	return s.due, nil
}
func (s *stubStore) ClaimDueRotation(_ context.Context, versionID string, listedNext, _ time.Time) (bool, error) {
	if s.claimed == nil {
		s.claimed = map[string]time.Time{}
	}
	if prev, ok := s.claimed[versionID]; ok && prev.Equal(listedNext) {
		return false, nil // already claimed at this due time
	}
	s.claimed[versionID] = listedNext
	return true, nil
}
func (s *stubStore) UpdateRotationSchedule(_ context.Context, _ string, next time.Time) error {
	if s.skipUpd {
		return domain.ErrRotationScheduleNotFound
	}
	s.updates.Add(1)
	s.lastNext = next
	return nil
}

func TestSweeper_RotatesDueAndReschedules(t *testing.T) {
	core := &stubCore{expectedPath: "kv/db", rotateOK: true}
	store := &stubStore{due: []*domain.DueRotation{
		{SecretPolicyID: "p1", SecretPolicyVersionID: "v1", SecretPath: "kv/db", SecretClass: "DATABASE_CREDENTIAL", IntervalSeconds: 3600},
	}}
	s := New(store, core, zap.NewNop(), time.Hour, "system:rotation-sweeper")
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if core.calls.Load() != 1 {
		t.Fatalf("expected 1 rotation call, got %d", core.calls.Load())
	}
	if core.lastPolicyID != "p1" {
		t.Fatalf("rotated policy %q, want p1", core.lastPolicyID)
	}
	if core.lastActorID != "system:rotation-sweeper" {
		t.Fatalf("actor %q, want system identity", core.lastActorID)
	}
	if store.updates.Load() != 1 {
		t.Fatalf("expected 1 reschedule, got %d", store.updates.Load())
	}
	// next_rotation_at must be roughly now + 3600s.
	delta := store.lastNext.Sub(time.Now())
	if delta < 3590*time.Second || delta > 3700*time.Second {
		t.Fatalf("reschedule delta = %v, want ~1h", delta)
	}
}

func TestSweeper_ContinuesOnPerSecretError(t *testing.T) {
	core := &stubCore{err: domain.ErrSecretPolicyNotFound}
	store := &stubStore{due: []*domain.DueRotation{
		{SecretPolicyID: "gone", SecretPolicyVersionID: "v1", SecretPath: "kv/gone", SecretClass: "X", IntervalSeconds: 60},
	}}
	s := New(store, core, zap.NewNop(), time.Hour, "system:rotation-sweeper")
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep must not fail when one rotate fails; got %v", err)
	}
	// Gone policy is skipped, not rescheduled.
	if store.updates.Load() != 0 {
		t.Fatalf("rescheduled a skipped rotation")
	}
}

func TestSweeper_UpdateFailureDoesNotFailSweep(t *testing.T) {
	core := &stubCore{expectedPath: "kv/db"}
	store := &stubStore{due: []*domain.DueRotation{
		{SecretPolicyID: "p1", SecretPolicyVersionID: "v1", SecretPath: "kv/db", SecretClass: "X", IntervalSeconds: 60},
	}, skipUpd: true}
	s := New(store, core, zap.NewNop(), time.Hour, "system:rotation-sweeper")
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep must not fail on reschedule error; got %v", err)
	}
	if core.calls.Load() != 1 {
		t.Fatalf("expected rotation to have completed despite reschedule failure")
	}
}
// Two sweeps over the same due listing (two replicas, or one slow pass
// re-listing) must rotate once. The request id used to embed time.Now().
func TestSweeper_SameDueSlotRotatesOnceAcrossSweepers(t *testing.T) {
	due := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	core := &stubCore{expectedPath: "kv/db"}
	store := &stubStore{due: []*domain.DueRotation{
		{SecretPolicyID: "p1", SecretPolicyVersionID: "v1", SecretPath: "kv/db", IntervalSeconds: 3600, NextRotationAt: due},
	}}
	a := New(store, core, zap.NewNop(), time.Hour, "system:rotation-sweeper")
	b := New(store, core, zap.NewNop(), time.Hour, "system:rotation-sweeper")
	_ = a.Sweep(context.Background())
	_ = b.Sweep(context.Background())
	if n := core.calls.Load(); n != 1 {
		t.Fatalf("a due slot must be rotated exactly once, got %d", n)
	}
	want := "sweep:v1:" + strconv.FormatInt(due.Unix(), 10)
	if core.lastRequestID != want {
		t.Fatalf("request id = %q, want deterministic %q", core.lastRequestID, want)
	}
}
