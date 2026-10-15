package retention_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/retention"
)

// purgingStore adds the 000031 purge capability to the fake.
type purgingStore struct {
	*fakeStore
	outboxWindows, idempotencyWindows []time.Duration
}

func (p *purgingStore) PurgePublishedOutbox(_ context.Context, olderThan time.Duration) (int64, error) {
	p.outboxWindows = append(p.outboxWindows, olderThan)
	return 3, nil
}

func (p *purgingStore) PurgeIdempotencyKeys(_ context.Context, olderThan time.Duration) (int64, error) {
	p.idempotencyWindows = append(p.idempotencyWindows, olderThan)
	return 2, nil
}

// N-14: a sweep purges published outbox rows and old idempotency keys with the
// configured windows.
func TestSweep_PurgesWithConfiguredWindows(t *testing.T) {
	s := &purgingStore{fakeStore: newFake()}
	r := retention.New(s, zap.NewNop(), time.Hour, 3, 24)
	r.SetPurge(7*24*time.Hour, 48*time.Hour)
	r.Sweep(context.Background())

	if len(s.outboxWindows) != 1 || s.outboxWindows[0] != 7*24*time.Hour {
		t.Errorf("outbox purge windows = %v, want one call with 168h", s.outboxWindows)
	}
	if len(s.idempotencyWindows) != 1 || s.idempotencyWindows[0] != 48*time.Hour {
		t.Errorf("idempotency purge windows = %v, want one call with 48h", s.idempotencyWindows)
	}
}

// Zero keeps a table; not configuring purge at all keeps both.
func TestSweep_ZeroWindowSkipsThatPurge(t *testing.T) {
	s := &purgingStore{fakeStore: newFake()}
	r := retention.New(s, zap.NewNop(), time.Hour, 3, 24)
	r.SetPurge(0, time.Hour)
	r.Sweep(context.Background())
	if len(s.outboxWindows) != 0 {
		t.Errorf("outbox purged with a zero window: %v", s.outboxWindows)
	}
	if len(s.idempotencyWindows) != 1 {
		t.Errorf("idempotency keys not purged: %v", s.idempotencyWindows)
	}

	s2 := &purgingStore{fakeStore: newFake()}
	retention.New(s2, zap.NewNop(), time.Hour, 3, 24).Sweep(context.Background())
	if len(s2.outboxWindows)+len(s2.idempotencyWindows) != 0 {
		t.Error("purged without SetPurge")
	}
}

// The purge runs under the sweep lock like the rest of the sweep: a replica
// that does not hold it purges nothing.
func TestSweep_NoPurgeWithoutTheLock(t *testing.T) {
	s := &purgingStore{fakeStore: newFake()}
	s.lockGranted = false
	r := retention.New(s, zap.NewNop(), time.Hour, 3, 24)
	r.SetPurge(time.Hour, time.Hour)
	r.Sweep(context.Background())
	if len(s.outboxWindows)+len(s.idempotencyWindows) != 0 {
		t.Error("purged without holding the sweep lock")
	}
}
