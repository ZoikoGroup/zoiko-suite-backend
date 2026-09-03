// Package cache puts a short-lived, self-invalidating read cache in front of
// the authorization store's evaluation queries.
//
// ── WHY ──────────────────────────────────────────────────────────────────────
//
// POST /v1/authorize is called on nearly every mutating request across ~60
// services and, before this, it went to the database every time — five
// separate transactions per evaluation on the worst path (grants, delegations,
// static SoD, own-object SoD, then the decision-log insert), with the
// delegation lookup itself running one more transaction per delegator. A
// single authorize call was measured at 1.07s. Nothing on the platform's
// hottest path was cached at all.
//
// ── WHAT IS AND IS NOT CACHED ────────────────────────────────────────────────
//
// Cached: the four evaluation READS — FindGrantedActions, FindDelegatedActions,
// CheckSoDConflict, CheckOwnObjectSoD — plus FindABACRules. All are pure
// functions of (their arguments, the current state of the tables they read).
//
// NOT cached, and this is the important half:
//
//	RecordAccessDecision   Every evaluation writes its own decision artifact.
//	                       The critical constraint is that no material action
//	                       executes without one, so this insert happens on
//	                       every call, cache hit or not. A cached decision id
//	                       returned twice would be a request with no evidence.
//	the DECISION itself    Only the inputs are cached. The layers still run,
//	                       the outcome is still computed, the event is still
//	                       published, the SIEM signal is still streamed.
//
// So a cache hit removes database round-trips, never an obligation.
//
// ── STALENESS, STATED PLAINLY ────────────────────────────────────────────────
//
// This is an in-process cache, so each replica has its own. A write through
// THIS process invalidates immediately and exactly (see the generation
// counters below); a write through ANOTHER replica, or directly against the
// database, is not seen until the entry expires.
//
// That makes the TTL the real bound on how long a revoked grant can still
// authorize, and it is why the default is deliberately small — 5 seconds — and
// why AUTHZ_CACHE_TTL_SECONDS=0 disables the cache entirely rather than merely
// shortening it. A deployment that cannot accept a 5-second window on
// revocation should set 0 and pay the round-trips; a deployment that wants
// more speed should raise it knowingly, with that sentence in mind.
//
// Revocation of a SESSION, as opposed to a grant, does not depend on this at
// all — identity-context-svc evicts sessions on the authority.revoked event.
package cache

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// Inner is the store shape the cache wraps. It is the full set of methods the
// handler needs, because the decorator has to satisfy the same interface —
// most are pass-throughs, and the ones that are not are grouped below.
type Inner interface {
	CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error)
	SetRoleActive(ctx context.Context, roleID, tenantID string, active bool) (*domain.Role, error)
	FindRoleByID(ctx context.Context, roleID string) (*domain.Role, error)
	CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, error)
	CreateRoleAssignment(ctx context.Context, params domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error)
	RevokeRoleAssignment(ctx context.Context, assignmentID, tenantID string) (*domain.PrincipalRoleAssignment, error)
	ListRoleAssignments(ctx context.Context, tenantID, principalID, roleID string, activeOnly bool) ([]domain.PrincipalRoleAssignment, error)
	CreateDelegatedAuthority(ctx context.Context, params domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error)
	FindDelegatedAuthorityByID(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error)
	RevokeDelegatedAuthority(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error)
	ProjectDelegation(ctx context.Context, params domain.ProjectDelegationParams) (*domain.DelegatedAuthority, error)
	RevokeProjectedDelegation(ctx context.Context, sourceService, sourceDelegationID, tenantID string) (*domain.DelegatedAuthority, error)
	CreateSoDRule(ctx context.Context, params domain.CreateSoDRuleParams) (*domain.SoDRule, error)
	ListSoDRules(ctx context.Context, tenantID string) ([]domain.SoDRule, error)
	CreateABACRule(ctx context.Context, params domain.CreateABACRuleParams) (*domain.ABACRule, error)
	SetABACRuleActive(ctx context.Context, abacRuleID, tenantID string, active bool) (*domain.ABACRule, error)
	ListABACRules(ctx context.Context, tenantID, actionType string) ([]domain.ABACRule, error)
	FindABACRules(ctx context.Context, actionType, tenantID string) ([]domain.ABACRule, error)
	FindGrantedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error)
	FindDelegatedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error)
	CheckSoDConflict(ctx context.Context, grantedActions []string, candidateAction, tenantID string) (string, bool, error)
	CheckOwnObjectSoD(ctx context.Context, actionType, tenantID string) (bool, error)
	RecordAccessDecision(ctx context.Context, params domain.RecordAccessDecisionParams) (*domain.AccessDecisionLog, error)
	FindAccessDecisionByID(ctx context.Context, accessDecisionID, tenantID string) (*domain.AccessDecisionLog, error)
}

// The cache namespaces. A write invalidates whole namespaces for a tenant
// rather than computing which individual keys it affected: working out that a
// new permission bundle changes the grant set of every principal holding any
// assignment to that role means running the very queries the cache exists to
// avoid. Namespace granularity is coarse, correct, and cheap.
const (
	nsGrants     = "grants"
	nsDelegation = "delegation"
	nsSoD        = "sod"
	nsABAC       = "abac"
)

// DefaultTTL is the staleness bound when none is configured. Small on
// purpose — see the package comment on what the TTL actually bounds.
const DefaultTTL = 5 * time.Second

// maxEntries bounds the map. The key space is (principal × entity × tenant),
// which on this platform is large and mostly cold, so an unbounded map here
// would be the memory leak version of the problem partitioning solved for
// access_decision_log. On overflow, expired entries are dropped first and the
// whole map only if that was not enough — a cleared cache costs latency, a
// growing one costs the process.
const maxEntries = 50_000

type entry struct {
	value     any
	expiresAt time.Time
}

// Store decorates Inner with a TTL cache on the evaluation reads and
// invalidates the affected namespace on every write it passes through.
type Store struct {
	inner Inner
	ttl   time.Duration
	log   *zap.Logger

	mu      sync.Mutex
	entries map[string]entry

	// gen is the generation counter per (namespace, tenant). It is part of
	// every cache key, so bumping it makes every existing entry for that
	// namespace and tenant unreachable in O(1) without walking the map. The
	// orphaned entries are reclaimed by the overflow sweep.
	//
	// A write with no tenant, and a write to a PLATFORM-WIDE rule (SoD or
	// ABAC with tenant_id NULL), bump the namespace's global counter instead,
	// which is also in every key — a platform-wide rule binds every tenant,
	// so invalidating only the author's scope would leave every other
	// tenant's cached decisions unaware of a control that now applies to them.
	gen       map[string]uint64
	globalGen map[string]uint64

	hits   uint64
	misses uint64
}

// New wraps inner. A ttl of zero or less disables caching completely: every
// read goes through, and the write paths still invalidate (harmlessly), so
// AUTHZ_CACHE_TTL_SECONDS=0 is a true off switch rather than a 0-second TTL that
// still allocates.
func New(inner Inner, ttl time.Duration, log *zap.Logger) *Store {
	return &Store{
		inner:     inner,
		ttl:       ttl,
		log:       log,
		entries:   make(map[string]entry),
		gen:       make(map[string]uint64),
		globalGen: make(map[string]uint64),
	}
}

// Enabled reports whether reads are actually cached.
func (s *Store) Enabled() bool { return s.ttl > 0 }

// Stats returns cumulative hit/miss counts and the current entry count, for
// the /metrics surface and for tests that need to prove a hit was a hit
// rather than a fast miss.
func (s *Store) Stats() (hits, misses uint64, entries int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits, s.misses, len(s.entries)
}

func (s *Store) key(ns, tenantID string, parts ...string) string {
	s.mu.Lock()
	g := s.gen[ns+"|"+tenantID]
	gg := s.globalGen[ns]
	s.mu.Unlock()

	var b strings.Builder
	b.WriteString(ns)
	b.WriteByte('|')
	b.WriteString(tenantID)
	b.WriteByte('|')
	b.WriteString(strconv.FormatUint(g, 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatUint(gg, 10))
	for _, p := range parts {
		b.WriteByte('|')
		b.WriteString(p)
	}
	return b.String()
}

func (s *Store) load(key string) (any, bool) {
	if !s.Enabled() {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || time.Now().After(e.expiresAt) {
		s.misses++
		return nil, false
	}
	s.hits++
	return e.value, true
}

func (s *Store) save(key string, value any) {
	if !s.Enabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= maxEntries {
		s.sweepLocked()
	}
	s.entries[key] = entry{value: value, expiresAt: time.Now().Add(s.ttl)}
}

// sweepLocked drops expired entries, and everything if that was not enough.
// Caller holds s.mu.
func (s *Store) sweepLocked() {
	now := time.Now()
	for k, e := range s.entries {
		if now.After(e.expiresAt) {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= maxEntries {
		s.log.Warn("authorization cache full of live entries — clearing",
			zap.Int("entries", len(s.entries)), zap.Int("max", maxEntries))
		s.entries = make(map[string]entry)
	}
}

// invalidate makes every cached entry in ns for tenantID unreachable. An empty
// tenantID means the write was not attributable to one tenant — a
// platform-wide rule, or a caller that sent no tenant — so the whole namespace
// goes, across every tenant.
func (s *Store) invalidate(ns, tenantID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tenantID == "" {
		s.globalGen[ns]++
		return
	}
	s.gen[ns+"|"+tenantID]++
}

// ── cached reads ─────────────────────────────────────────────────────────────

type grantResult struct {
	actions []string
	basis   string
}

// FindGrantedActions caches per (tenant, principal, entity).
//
// The returned slice is COPIED on the way out. The cached value is shared by
// every caller that hits the same key, and the handler appends to what it gets
// back (`allHeldActions = append(rbacActions, delegated...)`) — an append into
// a shared slice's spare capacity would let one request's evaluation mutate
// what the next one reads.
func (s *Store) FindGrantedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error) {
	k := s.key(nsGrants, tenantID, principalID, legalEntityID)
	if v, ok := s.load(k); ok {
		r := v.(grantResult)
		return copyOf(r.actions), r.basis, nil
	}
	actions, basis, err := s.inner.FindGrantedActions(ctx, principalID, legalEntityID, tenantID)
	if err != nil {
		// Errors are never cached. A store outage is transient and caching it
		// would turn a blip into a fail-closed window of TTL length on every
		// affected key.
		return nil, "", err
	}
	s.save(k, grantResult{actions: copyOf(actions), basis: basis})
	return actions, basis, nil
}

// FindDelegatedActions caches per (tenant, principal, entity), same shape.
func (s *Store) FindDelegatedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error) {
	k := s.key(nsDelegation, tenantID, principalID, legalEntityID)
	if v, ok := s.load(k); ok {
		r := v.(grantResult)
		return copyOf(r.actions), r.basis, nil
	}
	actions, basis, err := s.inner.FindDelegatedActions(ctx, principalID, legalEntityID, tenantID)
	if err != nil {
		return nil, "", err
	}
	s.save(k, grantResult{actions: copyOf(actions), basis: basis})
	return actions, basis, nil
}

type sodResult struct {
	conflicting string
	hasConflict bool
}

// CheckSoDConflict caches per (tenant, candidate action, held-action set).
//
// The held set is SORTED into the key, not used in argument order: the same
// set arriving in a different order is the same question, and keying on order
// would miss the cache for every request while still filling it.
func (s *Store) CheckSoDConflict(ctx context.Context, grantedActions []string, candidateAction, tenantID string) (string, bool, error) {
	k := s.key(nsSoD, tenantID, candidateAction, sortedJoin(grantedActions))
	if v, ok := s.load(k); ok {
		r := v.(sodResult)
		return r.conflicting, r.hasConflict, nil
	}
	conflicting, hasConflict, err := s.inner.CheckSoDConflict(ctx, grantedActions, candidateAction, tenantID)
	if err != nil {
		return "", false, err
	}
	s.save(k, sodResult{conflicting: conflicting, hasConflict: hasConflict})
	return conflicting, hasConflict, nil
}

// CheckOwnObjectSoD caches per (tenant, action). The narrowest key space of
// the four and the highest hit rate: it is a property of the action alone.
func (s *Store) CheckOwnObjectSoD(ctx context.Context, actionType, tenantID string) (bool, error) {
	k := s.key(nsSoD, tenantID, "own_object", actionType)
	if v, ok := s.load(k); ok {
		return v.(bool), nil
	}
	forbidden, err := s.inner.CheckOwnObjectSoD(ctx, actionType, tenantID)
	if err != nil {
		return false, err
	}
	s.save(k, forbidden)
	return forbidden, nil
}

// FindABACRules caches per (tenant, action). Almost always the empty slice,
// and that empty result is exactly what is worth caching — it is a database
// round-trip to learn that no rule guards this action.
func (s *Store) FindABACRules(ctx context.Context, actionType, tenantID string) ([]domain.ABACRule, error) {
	k := s.key(nsABAC, tenantID, actionType)
	if v, ok := s.load(k); ok {
		return v.([]domain.ABACRule), nil
	}
	rules, err := s.inner.FindABACRules(ctx, actionType, tenantID)
	if err != nil {
		return nil, err
	}
	// Stored and returned as-is: the evaluator only reads it, and every caller
	// gets the same read-only view.
	s.save(k, rules)
	return rules, nil
}

// ── writes: pass through, then invalidate ────────────────────────────────────

func (s *Store) CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error) {
	role, created, err := s.inner.CreateRole(ctx, params)
	if err == nil {
		s.invalidateGrantSources(params.TenantID)
	}
	return role, created, err
}

func (s *Store) SetRoleActive(ctx context.Context, roleID, tenantID string, active bool) (*domain.Role, error) {
	role, err := s.inner.SetRoleActive(ctx, roleID, tenantID, active)
	if err == nil {
		s.invalidateGrantSources(tenantID)
	}
	return role, err
}

func (s *Store) CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, error) {
	bundle, err := s.inner.CreatePermissionBundle(ctx, params)
	if err == nil {
		// The bundle knows its role, not its tenant. Rather than resolve the
		// role's tenant with another query, invalidate across every tenant:
		// bundles change rarely, and a bundle is the definition of what a role
		// grants, so a stale one is the worst kind of stale entry to keep.
		s.invalidateGrantSources("")
	}
	return bundle, err
}

func (s *Store) CreateRoleAssignment(ctx context.Context, params domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error) {
	assignment, err := s.inner.CreateRoleAssignment(ctx, params)
	if err == nil {
		s.invalidateGrantSources("")
	}
	return assignment, err
}

func (s *Store) RevokeRoleAssignment(ctx context.Context, assignmentID, tenantID string) (*domain.PrincipalRoleAssignment, error) {
	assignment, err := s.inner.RevokeRoleAssignment(ctx, assignmentID, tenantID)
	if err == nil {
		s.invalidateGrantSources(tenantID)
	}
	return assignment, err
}

func (s *Store) CreateDelegatedAuthority(ctx context.Context, params domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error) {
	d, err := s.inner.CreateDelegatedAuthority(ctx, params)
	if err == nil {
		s.invalidate(nsDelegation, params.TenantID)
	}
	return d, err
}

func (s *Store) RevokeDelegatedAuthority(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error) {
	d, err := s.inner.RevokeDelegatedAuthority(ctx, delegatedAuthorityID, tenantID)
	if err == nil {
		s.invalidate(nsDelegation, tenantID)
	}
	return d, err
}

func (s *Store) ProjectDelegation(ctx context.Context, params domain.ProjectDelegationParams) (*domain.DelegatedAuthority, error) {
	d, err := s.inner.ProjectDelegation(ctx, params)
	if err == nil {
		s.invalidate(nsDelegation, params.TenantID)
	}
	return d, err
}

func (s *Store) RevokeProjectedDelegation(ctx context.Context, sourceService, sourceDelegationID, tenantID string) (*domain.DelegatedAuthority, error) {
	d, err := s.inner.RevokeProjectedDelegation(ctx, sourceService, sourceDelegationID, tenantID)
	if err == nil {
		s.invalidate(nsDelegation, tenantID)
	}
	return d, err
}

func (s *Store) CreateSoDRule(ctx context.Context, params domain.CreateSoDRuleParams) (*domain.SoDRule, error) {
	rule, err := s.inner.CreateSoDRule(ctx, params)
	if err == nil {
		// A nil TenantID is a PLATFORM-WIDE rule: it binds every tenant, so
		// passing "" here invalidates the SoD namespace across all of them.
		// Scoping this to the author's tenant would leave every other tenant
		// enforcing the old rule set for up to a TTL.
		s.invalidate(nsSoD, derefOrEmpty(params.TenantID))
	}
	return rule, err
}

func (s *Store) CreateABACRule(ctx context.Context, params domain.CreateABACRuleParams) (*domain.ABACRule, error) {
	rule, err := s.inner.CreateABACRule(ctx, params)
	if err == nil {
		s.invalidate(nsABAC, derefOrEmpty(params.TenantID))
	}
	return rule, err
}

func (s *Store) SetABACRuleActive(ctx context.Context, abacRuleID, tenantID string, active bool) (*domain.ABACRule, error) {
	rule, err := s.inner.SetABACRuleActive(ctx, abacRuleID, tenantID, active)
	if err == nil {
		s.invalidate(nsABAC, tenantID)
	}
	return rule, err
}

// invalidateGrantSources drops BOTH the grants and the delegation namespaces.
//
// Delegation too, always: FindDelegatedActions resolves the delegator's own
// role assignments, so a change to any role, bundle or assignment changes what
// every delegation from that principal confers. Invalidating only nsGrants
// would leave a revoked role still reachable through a delegation of it —
// which is the same hole, one layer down.
func (s *Store) invalidateGrantSources(tenantID string) {
	s.invalidate(nsGrants, tenantID)
	s.invalidate(nsDelegation, tenantID)
}

// ── uncached pass-throughs ───────────────────────────────────────────────────
//
// Reads whose callers are admin/console paths rather than the evaluation hot
// path, plus the two access_decision_log methods. RecordAccessDecision in
// particular MUST NOT be cached in any form — see the package comment.

func (s *Store) FindRoleByID(ctx context.Context, roleID string) (*domain.Role, error) {
	return s.inner.FindRoleByID(ctx, roleID)
}

func (s *Store) ListRoleAssignments(ctx context.Context, tenantID, principalID, roleID string, activeOnly bool) ([]domain.PrincipalRoleAssignment, error) {
	return s.inner.ListRoleAssignments(ctx, tenantID, principalID, roleID, activeOnly)
}

func (s *Store) FindDelegatedAuthorityByID(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error) {
	return s.inner.FindDelegatedAuthorityByID(ctx, delegatedAuthorityID, tenantID)
}

func (s *Store) ListSoDRules(ctx context.Context, tenantID string) ([]domain.SoDRule, error) {
	return s.inner.ListSoDRules(ctx, tenantID)
}

func (s *Store) ListABACRules(ctx context.Context, tenantID, actionType string) ([]domain.ABACRule, error) {
	return s.inner.ListABACRules(ctx, tenantID, actionType)
}

func (s *Store) RecordAccessDecision(ctx context.Context, params domain.RecordAccessDecisionParams) (*domain.AccessDecisionLog, error) {
	return s.inner.RecordAccessDecision(ctx, params)
}

func (s *Store) FindAccessDecisionByID(ctx context.Context, accessDecisionID, tenantID string) (*domain.AccessDecisionLog, error) {
	return s.inner.FindAccessDecisionByID(ctx, accessDecisionID, tenantID)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func copyOf(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// sortedJoin renders a set of actions as an order-independent key fragment.
// The input is copied first — the caller's slice is the handler's live
// allHeldActions, and sorting it in place would reorder what the handler goes
// on to use.
func sortedJoin(in []string) string {
	if len(in) == 0 {
		return ""
	}
	cp := copyOf(in)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
