package cache_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/cache"
	"zoiko.io/authorization-svc/internal/domain"
)

// countingStore records how many times each cached read reached the inner
// store. A cache is only worth having if the round-trip actually stops
// happening, and only correct if it starts happening again after a write — so
// the call count IS the assertion here, not a proxy for it.
type countingStore struct {
	grantCalls     int
	delegateCalls  int
	sodCalls       int
	ownObjectCalls int
	abacCalls      int
	recordCalls    int

	// listDecisionCalls counts the audit read, which must NOT be cached: a
	// cached page would answer an auditor with what the log held up to a TTL
	// ago. principalStatusCalls counts layer 0, which must be.
	listDecisionCalls    int
	principalStatusCalls int
	principalStatus      string

	actions []string
	basis   string

	// failGrantsOnce makes the next FindGrantedActions fail and then clears
	// itself, for the "errors are not cached" case.
	failGrantsOnce bool
}

func (c *countingStore) FindGrantedActions(_ context.Context, _, _, _ string) ([]string, string, error) {
	c.grantCalls++
	if c.failGrantsOnce {
		c.failGrantsOnce = false
		return nil, "", domain.ErrStoreUnavailable
	}
	return c.actions, c.basis, nil
}
func (c *countingStore) FindDelegatedActions(_ context.Context, _, _, _ string) ([]string, string, error) {
	c.delegateCalls++
	return c.actions, c.basis, nil
}
func (c *countingStore) CheckSoDConflict(_ context.Context, _ []string, _, _ string) (string, bool, error) {
	c.sodCalls++
	return "", false, nil
}
func (c *countingStore) CheckOwnObjectSoD(_ context.Context, _, _ string) (bool, error) {
	c.ownObjectCalls++
	return false, nil
}
func (c *countingStore) FindABACRules(_ context.Context, _, _ string) ([]domain.ABACRule, error) {
	c.abacCalls++
	return nil, nil
}
func (c *countingStore) RecordAccessDecision(_ context.Context, _ domain.RecordAccessDecisionParams) (*domain.AccessDecisionLog, error) {
	c.recordCalls++
	return &domain.AccessDecisionLog{AccessDecisionID: "d-1"}, nil
}

// The rest are pass-throughs the decorator has to satisfy; the writes return
// nil errors so the invalidation path runs.
func (c *countingStore) CreateRole(_ context.Context, _ domain.CreateRoleParams) (*domain.Role, bool, error) {
	return &domain.Role{}, true, nil
}
func (c *countingStore) SetRoleActive(_ context.Context, _, _ string, _ bool) (*domain.Role, error) {
	return &domain.Role{}, nil
}
func (c *countingStore) FindRoleByID(_ context.Context, _ string) (*domain.Role, error) {
	return &domain.Role{}, nil
}
func (c *countingStore) CreatePermissionBundle(_ context.Context, _ domain.CreatePermissionBundleParams) (*domain.PermissionBundle, bool, error) {
	return &domain.PermissionBundle{}, true, nil
}
func (c *countingStore) ListPermissionBundles(_ context.Context, _, _ string) ([]domain.PermissionBundle, error) {
	return nil, nil
}
func (c *countingStore) SetPermissionBundleActive(_ context.Context, _, _ string, _ bool) (*domain.PermissionBundle, error) {
	return &domain.PermissionBundle{}, nil
}
func (c *countingStore) CreateRoleAssignment(_ context.Context, _ domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error) {
	return &domain.PrincipalRoleAssignment{}, nil
}
func (c *countingStore) RevokeRoleAssignment(_ context.Context, _, _ string) (*domain.PrincipalRoleAssignment, error) {
	return &domain.PrincipalRoleAssignment{}, nil
}
func (c *countingStore) ListRoleAssignments(_ context.Context, _, _, _ string, _ bool) ([]domain.PrincipalRoleAssignment, error) {
	return nil, nil
}
func (c *countingStore) ListRoles(_ context.Context, _ string, _ bool) ([]domain.Role, error) {
	return nil, nil
}
func (c *countingStore) ListDelegatedAuthorities(_ context.Context, _, _ string, _ bool) ([]domain.DelegatedAuthority, error) {
	return nil, nil
}
func (c *countingStore) CreateDelegatedAuthority(_ context.Context, _ domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error) {
	return &domain.DelegatedAuthority{}, nil
}
func (c *countingStore) FindDelegatedAuthorityByID(_ context.Context, _, _ string) (*domain.DelegatedAuthority, error) {
	return &domain.DelegatedAuthority{}, nil
}
func (c *countingStore) RevokeDelegatedAuthority(_ context.Context, _, _ string) (*domain.DelegatedAuthority, error) {
	return &domain.DelegatedAuthority{}, nil
}
func (c *countingStore) ProjectDelegation(_ context.Context, _ domain.ProjectDelegationParams) (*domain.DelegatedAuthority, error) {
	return &domain.DelegatedAuthority{}, nil
}
func (c *countingStore) RevokeProjectedDelegation(_ context.Context, _, _, _ string) (*domain.DelegatedAuthority, error) {
	return &domain.DelegatedAuthority{}, nil
}
func (c *countingStore) CreateSoDRule(_ context.Context, _ domain.CreateSoDRuleParams) (*domain.SoDRule, error) {
	return &domain.SoDRule{}, nil
}
func (c *countingStore) ListSoDRules(_ context.Context, _ string) ([]domain.SoDRule, error) {
	return nil, nil
}
func (c *countingStore) SetSoDRuleActive(_ context.Context, _, _ string, _ bool) (*domain.SoDRule, error) {
	return &domain.SoDRule{}, nil
}
func (c *countingStore) CreateABACRule(_ context.Context, _ domain.CreateABACRuleParams) (*domain.ABACRule, error) {
	return &domain.ABACRule{}, nil
}
func (c *countingStore) SetABACRuleActive(_ context.Context, _, _ string, _ bool) (*domain.ABACRule, error) {
	return &domain.ABACRule{}, nil
}
func (c *countingStore) ListABACRules(_ context.Context, _, _ string) ([]domain.ABACRule, error) {
	return nil, nil
}
func (c *countingStore) FindAccessDecisionByID(_ context.Context, _, _ string) (*domain.AccessDecisionLog, error) {
	return &domain.AccessDecisionLog{}, nil
}
func (c *countingStore) ListAccessDecisions(_ context.Context, _ string, _ domain.ListAccessDecisionsParams) (*domain.AccessDecisionPage, error) {
	c.listDecisionCalls++
	return &domain.AccessDecisionPage{}, nil
}
func (c *countingStore) ProjectPrincipalStatus(_ context.Context, params domain.ProjectPrincipalStatusParams) (*domain.PrincipalStatusProjection, error) {
	return &domain.PrincipalStatusProjection{
		PrincipalID: params.PrincipalID,
		TenantID:    params.TenantID,
		Status:      params.Status,
	}, nil
}
func (c *countingStore) FindPrincipalStatus(_ context.Context, _, _ string) (string, error) {
	c.principalStatusCalls++
	if c.principalStatus == "" {
		return domain.PrincipalStatusActive, nil
	}
	return c.principalStatus, nil
}

const (
	tenantA = "tenant-a"
	tenantB = "tenant-b"
)

func newCache(inner *countingStore, ttl time.Duration) *cache.Store {
	return cache.New(inner, ttl, zap.NewNop())
}

func TestCache_ServesRepeatedEvaluationReadsFromMemory(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}, basis: "rbac:role=X"}
	c := newCache(inner, time.Minute)

	for i := 0; i < 5; i++ {
		actions, basis, err := c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(actions) != 1 || actions[0] != "PAYMENT_APPROVE" || basis != "rbac:role=X" {
			t.Fatalf("call %d returned %v / %q", i, actions, basis)
		}
	}
	if inner.grantCalls != 1 {
		t.Fatalf("inner store called %d times for 5 identical reads, want 1", inner.grantCalls)
	}

	// Every other cached read, once each, to pin that they are wired at all.
	_, _, _ = c.FindDelegatedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindDelegatedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.CheckSoDConflict(ctx, []string{"A", "B"}, "C", tenantA)
	_, _, _ = c.CheckSoDConflict(ctx, []string{"A", "B"}, "C", tenantA)
	_, _ = c.CheckOwnObjectSoD(ctx, "PAYMENT_APPROVE", tenantA)
	_, _ = c.CheckOwnObjectSoD(ctx, "PAYMENT_APPROVE", tenantA)
	_, _ = c.FindABACRules(ctx, "PAYMENT_APPROVE", tenantA)
	_, _ = c.FindABACRules(ctx, "PAYMENT_APPROVE", tenantA)

	if inner.delegateCalls != 1 || inner.sodCalls != 1 || inner.ownObjectCalls != 1 || inner.abacCalls != 1 {
		t.Fatalf("uncached read: delegate=%d sod=%d ownObject=%d abac=%d, want 1 each",
			inner.delegateCalls, inner.sodCalls, inner.ownObjectCalls, inner.abacCalls)
	}
}

// TestCache_SoDKeyIsOrderIndependent — the same held-action set in a different
// order is the same question. Keying on argument order would miss every time
// while still filling the map.
func TestCache_SoDKeyIsOrderIndependent(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{}
	c := newCache(inner, time.Minute)

	_, _, _ = c.CheckSoDConflict(ctx, []string{"A", "B", "C"}, "D", tenantA)
	_, _, _ = c.CheckSoDConflict(ctx, []string{"C", "A", "B"}, "D", tenantA)

	if inner.sodCalls != 1 {
		t.Fatalf("inner called %d times for the same set in two orders, want 1", inner.sodCalls)
	}
}

// TestCache_DoesNotSortTheCallersSlice — the slice passed in is the handler's
// live allHeldActions, which it goes on to use. Sorting it in place to build a
// key would reorder the caller's data.
func TestCache_DoesNotSortTheCallersSlice(t *testing.T) {
	ctx := context.Background()
	c := newCache(&countingStore{}, time.Minute)

	held := []string{"C", "A", "B"}
	_, _, _ = c.CheckSoDConflict(ctx, held, "D", tenantA)

	if held[0] != "C" || held[1] != "A" || held[2] != "B" {
		t.Fatalf("caller's slice was mutated to %v", held)
	}
}

// TestCache_ReturnedSliceIsNotShared — the handler appends the delegated
// actions onto what FindGrantedActions returned. If that were the cached
// slice, an append into its spare capacity would let one request's evaluation
// corrupt what the next one reads.
func TestCache_ReturnedSliceIsNotShared(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}, basis: "rbac:role=X"}
	c := newCache(inner, time.Minute)

	first, _, _ := c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	first = append(first, "INJECTED") //nolint:staticcheck // the append IS the test
	_ = first

	second, _, _ := c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	if len(second) != 1 || second[0] != "PAYMENT_APPROVE" {
		t.Fatalf("a caller's append leaked into the cached value: %v", second)
	}
}

// TestCache_TenantsDoNotShareEntries. The cache sits in front of the queries
// whose tenant scoping IS the isolation control, so a key that ignored the
// tenant would reintroduce the cross-tenant grant leak at a layer above the
// database.
func TestCache_TenantsDoNotShareEntries(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}}
	c := newCache(inner, time.Minute)

	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantB)

	if inner.grantCalls != 2 {
		t.Fatalf("inner called %d times for two different tenants, want 2 — tenants are sharing a cache entry", inner.grantCalls)
	}
}

func TestCache_WritesInvalidate(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		// write performs the admin write, and each case asserts which
		// namespaces it had to drop.
		write            func(c *cache.Store)
		wantGrantReread  bool
		wantDelegReread  bool
		wantSoDReread    bool
		wantABACReread   bool
		wantOtherTenants bool // true when the write must invalidate every tenant
	}{
		{
			name:            "revoking an assignment re-reads grants AND delegations",
			write:           func(c *cache.Store) { _, _ = c.RevokeRoleAssignment(ctx, "a-1", tenantA) },
			wantGrantReread: true,
			// Delegation too: FindDelegatedActions resolves the delegator's
			// own assignments, so a revoked role must not stay reachable
			// through a delegation of it.
			wantDelegReread: true,
		},
		{
			name:            "retiring a role re-reads grants AND delegations",
			write:           func(c *cache.Store) { _, _ = c.SetRoleActive(ctx, "r-1", tenantA, false) },
			wantGrantReread: true,
			wantDelegReread: true,
		},
		{
			name:            "revoking a delegation re-reads delegations only",
			write:           func(c *cache.Store) { _, _ = c.RevokeDelegatedAuthority(ctx, "d-1", tenantA) },
			wantDelegReread: true,
		},
		{
			name: "an upstream projection re-reads delegations",
			write: func(c *cache.Store) {
				_, _ = c.ProjectDelegation(ctx, domain.ProjectDelegationParams{TenantID: tenantA})
			},
			wantDelegReread: true,
		},
		{
			name: "a tenant-scoped SoD rule re-reads SoD for that tenant only",
			write: func(c *cache.Store) {
				tid := tenantA
				_, _ = c.CreateSoDRule(ctx, domain.CreateSoDRuleParams{TenantID: &tid})
			},
			wantSoDReread: true,
		},
		{
			name: "a PLATFORM-WIDE SoD rule re-reads SoD for EVERY tenant",
			write: func(c *cache.Store) {
				_, _ = c.CreateSoDRule(ctx, domain.CreateSoDRuleParams{TenantID: nil})
			},
			wantSoDReread:    true,
			wantOtherTenants: true,
		},
		{
			name: "a PLATFORM-WIDE ABAC rule re-reads ABAC for EVERY tenant",
			write: func(c *cache.Store) {
				_, _ = c.CreateABACRule(ctx, domain.CreateABACRuleParams{TenantID: nil})
			},
			wantABACReread:   true,
			wantOtherTenants: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}}
			c := newCache(inner, time.Minute)

			// Warm every namespace, for two tenants.
			for _, tenant := range []string{tenantA, tenantB} {
				_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenant)
				_, _, _ = c.FindDelegatedActions(ctx, "p-1", "e-1", tenant)
				_, _, _ = c.CheckSoDConflict(ctx, []string{"A"}, "B", tenant)
				_, _ = c.FindABACRules(ctx, "PAYMENT_APPROVE", tenant)
			}
			before := *inner

			tc.write(c)

			// Re-read tenant A's entries.
			_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
			_, _, _ = c.FindDelegatedActions(ctx, "p-1", "e-1", tenantA)
			_, _, _ = c.CheckSoDConflict(ctx, []string{"A"}, "B", tenantA)
			_, _ = c.FindABACRules(ctx, "PAYMENT_APPROVE", tenantA)

			check := func(name string, got, was int, want bool) {
				reread := got > was
				if reread != want {
					t.Errorf("%s re-read = %v, want %v", name, reread, want)
				}
			}
			check("grants", inner.grantCalls, before.grantCalls, tc.wantGrantReread)
			check("delegations", inner.delegateCalls, before.delegateCalls, tc.wantDelegReread)
			check("sod", inner.sodCalls, before.sodCalls, tc.wantSoDReread)
			check("abac", inner.abacCalls, before.abacCalls, tc.wantABACReread)

			if tc.wantOtherTenants {
				beforeB := *inner
				_, _, _ = c.CheckSoDConflict(ctx, []string{"A"}, "B", tenantB)
				_, _ = c.FindABACRules(ctx, "PAYMENT_APPROVE", tenantB)
				if inner.sodCalls == beforeB.sodCalls && inner.abacCalls == beforeB.abacCalls {
					t.Error("a platform-wide rule did not invalidate other tenants — " +
						"they would keep enforcing the old rule set for up to a TTL")
				}
			}
		})
	}
}

// TestCache_TTLExpires bounds the staleness the package comment promises.
func TestCache_TTLExpires(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}}
	c := newCache(inner, 20*time.Millisecond)

	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	if inner.grantCalls != 1 {
		t.Fatalf("inner called %d times before expiry, want 1", inner.grantCalls)
	}

	time.Sleep(40 * time.Millisecond)
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	if inner.grantCalls != 2 {
		t.Fatalf("inner called %d times after expiry, want 2 — the TTL is not bounding staleness", inner.grantCalls)
	}
}

// TestCache_ZeroTTLIsATrueOffSwitch — AUTHZ_CACHE_TTL_SECONDS=0 has to mean
// "no caching", not "a zero-second TTL that still allocates and still risks
// serving a stale entry within one clock tick".
func TestCache_ZeroTTLIsATrueOffSwitch(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}}
	c := newCache(inner, 0)

	if c.Enabled() {
		t.Fatal("Enabled() is true with a zero TTL")
	}
	for i := 0; i < 3; i++ {
		_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	}
	if inner.grantCalls != 3 {
		t.Fatalf("inner called %d times with caching off, want 3", inner.grantCalls)
	}
	if _, _, entries := c.Stats(); entries != 0 {
		t.Fatalf("cache holds %d entries with caching off", entries)
	}
}

// TestCache_NeverCachesTheDecisionArtifact is the critical-constraint test:
// "no material action executes without an authorization decision artifact".
// A cache hit may remove database READS; it must never remove the write that
// records the decision, or a request would be authorized with no evidence.
func TestCache_NeverCachesTheDecisionArtifact(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{}
	c := newCache(inner, time.Minute)

	params := domain.RecordAccessDecisionParams{
		PrincipalID: "p-1", LegalEntityID: "e-1", ActionType: "PAYMENT_APPROVE",
		Outcome: "GRANTED", Basis: "rbac:role=X", TenantID: tenantA,
	}
	for i := 0; i < 3; i++ {
		if _, err := c.RecordAccessDecision(ctx, params); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if inner.recordCalls != 3 {
		t.Fatalf("RecordAccessDecision reached the store %d times for 3 identical evaluations, want 3 — "+
			"every evaluation must write its own decision artifact", inner.recordCalls)
	}
}

// TestCache_DoesNotCacheErrors — a store outage is transient, and caching it
// would turn a blip into a fail-closed window of TTL length on every affected
// key.
func TestCache_DoesNotCacheErrors(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}, basis: "rbac:role=X", failGrantsOnce: true}
	c := newCache(inner, time.Minute)

	if _, _, err := c.FindGrantedActions(ctx, "p-1", "e-1", tenantA); err == nil {
		t.Fatal("expected the inner error to surface")
	}

	actions, _, err := c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	if err != nil {
		t.Fatalf("second call still failing — the error was cached: %v", err)
	}
	if len(actions) != 1 || actions[0] != "PAYMENT_APPROVE" {
		t.Fatalf("got %v, want the recovered result", actions)
	}
	if inner.grantCalls != 2 {
		t.Fatalf("inner called %d times, want 2 — a failed read must not populate the cache", inner.grantCalls)
	}
}

// ── layer 0: the principal-status read ──────────────────────────────────────

// Read on literally every /v1/authorize call, and the answer is almost always
// "ACTIVE, no row" — the same reason FindABACRules is cached: a database
// round-trip to learn nothing changed.
func TestCache_PrincipalStatusIsCached(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{}
	c := newCache(inner, time.Minute)

	for i := 0; i < 5; i++ {
		status, err := c.FindPrincipalStatus(ctx, "p-1", tenantA)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != domain.PrincipalStatusActive {
			t.Fatalf("call %d returned %q", i, status)
		}
	}
	if inner.principalStatusCalls != 1 {
		t.Fatalf("inner store called %d times for 5 identical status reads, want 1", inner.principalStatusCalls)
	}
}

// Its own namespace, so a suspension does not throw away the tenant's whole
// cached grant map on the hot path.
func TestCache_PrincipalStatusIsKeyedPerPrincipal(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{}
	c := newCache(inner, time.Minute)

	_, _ = c.FindPrincipalStatus(ctx, "p-1", tenantA)
	_, _ = c.FindPrincipalStatus(ctx, "p-2", tenantA)
	_, _ = c.FindPrincipalStatus(ctx, "p-1", tenantB)

	if inner.principalStatusCalls != 3 {
		t.Fatalf("inner called %d times for three distinct (principal, tenant) pairs, want 3",
			inner.principalStatusCalls)
	}
}

// The property a suspension depends on. A projection must invalidate BOTH the
// named tenant's entry AND the tenantless one — FindPrincipalStatus is keyed on
// the tenant it was ASKED with, and 86 of this endpoint's callers ask with none.
// Invalidating only the named tenant would leave the majority of the platform's
// traffic reading a stale ACTIVE for a principal that was just suspended.
func TestCache_ProjectingAStatusInvalidatesBothScopedAndTenantlessReads(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{}
	c := newCache(inner, time.Minute)

	// Warm both keys: one tenant-scoped, one tenantless.
	if _, err := c.FindPrincipalStatus(ctx, "p-1", tenantA); err != nil {
		t.Fatalf("warm scoped: %v", err)
	}
	if _, err := c.FindPrincipalStatus(ctx, "p-1", ""); err != nil {
		t.Fatalf("warm tenantless: %v", err)
	}
	if inner.principalStatusCalls != 2 {
		t.Fatalf("warm-up made %d inner calls, want 2", inner.principalStatusCalls)
	}

	inner.principalStatus = "SUSPENDED"
	if _, err := c.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID: "p-1", TenantID: tenantA, Status: "SUSPENDED",
	}); err != nil {
		t.Fatalf("project: %v", err)
	}

	scoped, err := c.FindPrincipalStatus(ctx, "p-1", tenantA)
	if err != nil {
		t.Fatalf("scoped read: %v", err)
	}
	if scoped != "SUSPENDED" {
		t.Errorf("tenant-scoped read = %q after projection, want SUSPENDED", scoped)
	}

	tenantless, err := c.FindPrincipalStatus(ctx, "p-1", "")
	if err != nil {
		t.Fatalf("tenantless read: %v", err)
	}
	if tenantless != "SUSPENDED" {
		t.Errorf("tenantless read = %q after projection, want SUSPENDED — this is the caller shape 86 of 111 services use, and it must not keep a stale ACTIVE",
			tenantless)
	}
}

// ── the audit read must NOT be cached ───────────────────────────────────────

// The one place in the cache where "uncached" is a correctness requirement
// rather than a judgement about hit rates: a cached page would answer an
// auditor with what the log held up to a TTL ago, so a decision recorded
// seconds before they looked could be absent from the answer they were given.
func TestCache_AccessDecisionListingIsNeverCached(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{}
	c := newCache(inner, time.Minute)

	for i := 0; i < 4; i++ {
		if _, err := c.ListAccessDecisions(ctx, tenantA, domain.ListAccessDecisionsParams{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if inner.listDecisionCalls != 4 {
		t.Fatalf("the audit read reached the store %d times for 4 identical listings, want 4 — "+
			"a cached page would hand an auditor a log that is up to a TTL out of date",
			inner.listDecisionCalls)
	}
}

// ── cross-replica invalidation ──────────────────────────────────────────────

// The hook the lifecycle consumer calls when an authoritative service announces
// a change this replica did not make. Before it existed, the TTL was the only
// bound on how long another replica's admin write took to be seen.
func TestCache_InvalidateGrantSourcesForTenantDropsGrantsAndDelegations(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}, basis: "rbac:role=X"}
	c := newCache(inner, time.Minute)

	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindDelegatedActions(ctx, "p-1", "e-1", tenantA)
	if inner.grantCalls != 1 || inner.delegateCalls != 1 {
		t.Fatalf("warm-up: grants=%d delegations=%d, want 1 each", inner.grantCalls, inner.delegateCalls)
	}

	c.InvalidateGrantSourcesForTenant(tenantA)

	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindDelegatedActions(ctx, "p-1", "e-1", tenantA)

	// Delegations too, always: FindDelegatedActions resolves the delegator's
	// own role assignments, so a change to any role or bundle changes what
	// every delegation from that principal confers. Dropping only the grants
	// namespace would leave a revoked role still reachable through a
	// delegation of it.
	if inner.grantCalls != 2 {
		t.Errorf("grants re-read %d times, want 2", inner.grantCalls)
	}
	if inner.delegateCalls != 2 {
		t.Errorf("delegations re-read %d times, want 2 — a role change alters what every delegation from that principal confers",
			inner.delegateCalls)
	}
}

// An empty tenant means every tenant, which is what a tenantless grant-graph
// event gets. Over-invalidating costs one read; under-invalidating leaves a
// replica authorizing from a grant set that has already changed.
func TestCache_InvalidateWithNoTenantDropsEveryTenant(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}, basis: "rbac:role=X"}
	c := newCache(inner, time.Minute)

	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantB)
	if inner.grantCalls != 2 {
		t.Fatalf("warm-up made %d calls, want 2", inner.grantCalls)
	}

	c.InvalidateGrantSourcesForTenant("")

	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantB)

	if inner.grantCalls != 4 {
		t.Fatalf("grants re-read %d times after a tenantless invalidation, want 4 (both tenants)", inner.grantCalls)
	}
}

// With the cache off, every read goes through and the invalidation hooks are
// harmless no-ops rather than panics — AUTHZ_CACHE_TTL_SECONDS=0 is a real off
// switch, and the lifecycle consumer calls into it unconditionally.
func TestCache_DisabledCacheStillAcceptsInvalidation(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{}
	c := newCache(inner, 0)

	if c.Enabled() {
		t.Fatal("cache reports enabled with a zero TTL")
	}

	_, _ = c.FindPrincipalStatus(ctx, "p-1", tenantA)
	c.InvalidateGrantSourcesForTenant(tenantA)
	c.InvalidateGrantSourcesForTenant("")
	_, _ = c.FindPrincipalStatus(ctx, "p-1", tenantA)

	if inner.principalStatusCalls != 2 {
		t.Fatalf("status read %d times with caching disabled, want 2", inner.principalStatusCalls)
	}
}
