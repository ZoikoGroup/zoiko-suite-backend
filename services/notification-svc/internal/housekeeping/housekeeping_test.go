package housekeeping_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/housekeeping"
)

type mockHousekeepingStore struct {
	expiredTokens   map[string]int64
	purgedTokens    map[string]int64
	staleIntents    map[string]int64
	purgedLedger    map[string]int64
	failDiscover    bool
	failMutate      bool
}

func newMockHousekeepingStore() *mockHousekeepingStore {
	return &mockHousekeepingStore{
		expiredTokens: make(map[string]int64),
		purgedTokens:  make(map[string]int64),
		staleIntents:  make(map[string]int64),
		purgedLedger:  make(map[string]int64),
	}
}

func (m *mockHousekeepingStore) FindTenantsWithExpiredActionTokens(_ context.Context, _ time.Time, _ int) ([]string, error) {
	if m.failDiscover {
		return nil, errors.New("discovery failure")
	}
	var tenants []string
	for t := range m.expiredTokens {
		tenants = append(tenants, t)
	}
	return tenants, nil
}

func (m *mockHousekeepingStore) ExpireActionTokensForTenant(_ context.Context, tenantID string, _ time.Time) (int64, error) {
	if m.failMutate {
		return 0, errors.New("mutation failure")
	}
	c := m.expiredTokens[tenantID]
	m.expiredTokens[tenantID] = 0 // Transitioned
	return c, nil
}

func (m *mockHousekeepingStore) PurgeActionTokensForTenant(_ context.Context, tenantID string, _ time.Time) (int64, error) {
	if m.failMutate {
		return 0, errors.New("mutation failure")
	}
	c := m.purgedTokens[tenantID]
	m.purgedTokens[tenantID] = 0
	return c, nil
}

func (m *mockHousekeepingStore) FindTenantsWithPurgeableActionTokens(_ context.Context, _ time.Time, _ int) ([]string, error) {
	if m.failDiscover {
		return nil, errors.New("discovery failure")
	}
	var tenants []string
	for t := range m.purgedTokens {
		tenants = append(tenants, t)
	}
	return tenants, nil
}

func (m *mockHousekeepingStore) FindTenantsWithStaleIntents(_ context.Context, _ time.Time, _ int) ([]string, error) {
	if m.failDiscover {
		return nil, errors.New("discovery failure")
	}
	var tenants []string
	for t := range m.staleIntents {
		tenants = append(tenants, t)
	}
	return tenants, nil
}

func (m *mockHousekeepingStore) ExpireStaleIntentsForTenant(_ context.Context, tenantID string, _ time.Time) (int64, error) {
	if m.failMutate {
		return 0, errors.New("mutation failure")
	}
	c := m.staleIntents[tenantID]
	m.staleIntents[tenantID] = 0
	return c, nil
}

func (m *mockHousekeepingStore) FindTenantsWithCompletedIntents(_ context.Context, _ time.Time, _ int) ([]string, error) {
	if m.failDiscover {
		return nil, errors.New("discovery failure")
	}
	var tenants []string
	for t := range m.purgedLedger {
		tenants = append(tenants, t)
	}
	return tenants, nil
}

func (m *mockHousekeepingStore) PurgeCompletedLedgerRecordsForTenant(_ context.Context, tenantID string, _ time.Time) (int64, error) {
	if m.failMutate {
		return 0, errors.New("mutation failure")
	}
	c := m.purgedLedger[tenantID]
	m.purgedLedger[tenantID] = 0
	return c, nil
}

func TestHousekeepingWorker_FullLifecyclePass(t *testing.T) {
	store := newMockHousekeepingStore()
	store.expiredTokens["tenant-alpha"] = 15
	store.purgedTokens["tenant-alpha"] = 8
	store.staleIntents["tenant-beta"] = 4
	store.purgedLedger["tenant-gamma"] = 25

	opts := housekeeping.Options{
		Interval:             10 * time.Millisecond,
		BatchSize:            100,
		TokenRetention:       30 * 24 * time.Hour,
		LedgerRetention:      90 * 24 * time.Hour,
		StaleIntentThreshold: 24 * time.Hour,
	}

	worker := housekeeping.NewWorker(store, opts, zap.NewNop())

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once failed: %v", err)
	}

	if stats.ExpiredTokens != 15 {
		t.Errorf("expected 15 expired tokens, got %d", stats.ExpiredTokens)
	}
	if stats.PurgedTokens != 8 {
		t.Errorf("expected 8 purged tokens, got %d", stats.PurgedTokens)
	}
	if stats.StaleIntents != 4 {
		t.Errorf("expected 4 stale intents, got %d", stats.StaleIntents)
	}
	if stats.PurgedLedgerRows != 25 {
		t.Errorf("expected 25 purged ledger rows, got %d", stats.PurgedLedgerRows)
	}
}

func TestHousekeepingWorker_Idempotent_SecondRunProducesZero(t *testing.T) {
	store := newMockHousekeepingStore()
	store.expiredTokens["tenant-idemp"] = 10
	store.staleIntents["tenant-idemp"] = 5

	worker := housekeeping.NewWorker(store, housekeeping.Options{}, zap.NewNop())

	// First pass
	stats1, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	if stats1.ExpiredTokens != 10 || stats1.StaleIntents != 5 {
		t.Errorf("first run unexpected stats: %+v", stats1)
	}

	// Second pass: records are already processed, should produce 0 mutations
	stats2, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if stats2.ExpiredTokens != 0 || stats2.StaleIntents != 0 {
		t.Errorf("second run must be idempotent with 0 mutations, got %+v", stats2)
	}
}

func TestHousekeepingWorker_DiscoveryFailure_HandledGracefully(t *testing.T) {
	store := newMockHousekeepingStore()
	store.failDiscover = true

	worker := housekeeping.NewWorker(store, housekeeping.Options{}, zap.NewNop())

	_, err := worker.RunOnce(context.Background())
	if err == nil {
		t.Fatalf("expected discovery error to be surfaced")
	}
}

func TestHousekeepingWorker_TenantMutationFailure_ContinuesOtherTenants(t *testing.T) {
	store := newMockHousekeepingStore()
	store.expiredTokens["tenant-fail"] = 5
	store.failMutate = true

	worker := housekeeping.NewWorker(store, housekeeping.Options{}, zap.NewNop())

	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("per-tenant mutation failure should be logged and not abort the entire worker run: %v", err)
	}
	if stats.ExpiredTokens != 0 {
		t.Errorf("expected 0 successfully expired tokens due to mutation error, got %d", stats.ExpiredTokens)
	}
}
