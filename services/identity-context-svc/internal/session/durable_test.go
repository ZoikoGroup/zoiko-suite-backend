package session_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/session"
)

// DurableCache is where this service's session tenancy lives, and until now it
// had no tests at all — the package sat at 2% because only the Redis key
// helpers were covered.
//
// That is the wrong 2%. The key helpers are string formatting; DurableCache is
// the security boundary. Every method taking a session id also takes the
// caller's verified tenant, because a Redis session key names no owner, and
// before that existed GET /v1/context/session/{id} returned ANY tenant's signed
// envelope to ANY caller holding the id. These tests pin that it stays shut.

// ── Fakes ────────────────────────────────────────────────────────────────────

type fakeHot struct {
	mu sync.Mutex

	jwts        map[string]string
	contexts    map[string]*domain.SessionContext
	byPrincipal map[string][]string
	byEntity    map[string][]string

	evicted          []string
	clearedPrincipal []string
	clearedEntity    []string

	getCtxErr    error
	putErr       error
	persistErr   error
	principalErr error
	entityErr    error
}

func newFakeHot() *fakeHot {
	return &fakeHot{
		jwts:        map[string]string{},
		contexts:    map[string]*domain.SessionContext{},
		byPrincipal: map[string][]string{},
		byEntity:    map[string][]string{},
	}
}

func (f *fakeHot) Put(_ context.Context, id, jwt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.jwts[id] = jwt
	return nil
}

func (f *fakeHot) Get(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.jwts[id]; ok {
		return v, nil
	}
	return "", errors.New("redis: nil")
}

func (f *fakeHot) Evict(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evicted = append(f.evicted, id)
	delete(f.jwts, id)
	return nil
}

func (f *fakeHot) PersistSessionContext(_ context.Context, sc domain.SessionContext) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.persistErr != nil {
		return f.persistErr
	}
	cp := sc
	f.contexts[sc.SessionContextID] = &cp
	return nil
}

func (f *fakeHot) GetSessionContext(_ context.Context, id string) (*domain.SessionContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getCtxErr != nil {
		return nil, f.getCtxErr
	}
	return f.contexts[id], nil
}

func (f *fakeHot) Invalidate(_ context.Context, id string, reason domain.InvalidationReason, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sc, ok := f.contexts[id]; ok {
		sc.InvalidatedAt = &at
		sc.InvalidationReason = &reason
	}
	return nil
}

func (f *fakeHot) SessionIDsForPrincipal(_ context.Context, principalID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.principalErr != nil {
		return nil, f.principalErr
	}
	return f.byPrincipal[principalID], nil
}

func (f *fakeHot) ClearPrincipalIndex(_ context.Context, principalID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearedPrincipal = append(f.clearedPrincipal, principalID)
	return nil
}

func (f *fakeHot) SessionIDsForEntity(_ context.Context, entityID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entityErr != nil {
		return nil, f.entityErr
	}
	return f.byEntity[entityID], nil
}

func (f *fakeHot) ClearEntityIndex(_ context.Context, entityID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearedEntity = append(f.clearedEntity, entityID)
	return nil
}

type fakeStore struct {
	mu sync.Mutex

	contexts map[string]*domain.SessionContext
	events   []outbox.Record

	insertErr     error
	withEventErr  error
	findErr       error
	livePrincipal map[string][]string
	liveTenant    map[string][]string
	livePrincErr  error
	liveTenantErr error
	invalidated   []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		contexts:      map[string]*domain.SessionContext{},
		livePrincipal: map[string][]string{},
		liveTenant:    map[string][]string{},
	}
}

func (f *fakeStore) InsertSessionContext(_ context.Context, sc domain.SessionContext) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	cp := sc
	f.contexts[sc.SessionContextID] = &cp
	return nil
}

func (f *fakeStore) InsertSessionContextWithEvent(_ context.Context, sc domain.SessionContext, rec outbox.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.withEventErr != nil {
		return f.withEventErr
	}
	cp := sc
	f.contexts[sc.SessionContextID] = &cp
	f.events = append(f.events, rec)
	return nil
}

func (f *fakeStore) MarkSessionInvalidated(_ context.Context, id, tenantID string, reason domain.InvalidationReason, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, id)
	if sc, ok := f.contexts[id]; ok && sc.TenantID == tenantID {
		sc.InvalidatedAt = &at
		sc.InvalidationReason = &reason
	}
	return nil
}

func (f *fakeStore) FindSessionContext(_ context.Context, id, tenantID string) (*domain.SessionContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findErr != nil {
		return nil, f.findErr
	}
	sc, ok := f.contexts[id]
	// The real store is RLS-scoped, so a foreign tenant reads back absent.
	if !ok || sc.TenantID != tenantID {
		return nil, nil
	}
	return sc, nil
}

func (f *fakeStore) FindLiveSessionIDsForPrincipal(_ context.Context, principalID, tenantID string, _ time.Time) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.livePrincErr != nil {
		return nil, f.livePrincErr
	}
	return f.livePrincipal[principalID], nil
}

func (f *fakeStore) FindLiveSessionIDsForTenant(_ context.Context, tenantID string, _ time.Time) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.liveTenantErr != nil {
		return nil, f.liveTenantErr
	}
	return f.liveTenant[tenantID], nil
}

// ── Harness ──────────────────────────────────────────────────────────────────

type fixture struct {
	hot   *fakeHot
	store *fakeStore
	dc    *session.DurableCache
}

func newFixture() *fixture {
	hot := newFakeHot()
	store := newFakeStore()
	return &fixture{hot: hot, store: store, dc: session.NewDurableCache(hot, store, zap.NewNop())}
}

func (f *fixture) seed(id, tenantID, principalID string) domain.SessionContext {
	sc := domain.SessionContext{
		SessionContextID: id,
		TenantID:         tenantID,
		PrincipalID:      principalID,
		LegalEntityID:    "entity-1",
		IssuedAt:         time.Now().UTC(),
		ExpiresAt:        time.Now().UTC().Add(5 * time.Minute),
	}
	f.hot.contexts[id] = &sc
	f.store.contexts[id] = &sc
	f.hot.jwts[id] = "jwt-" + id
	return sc
}

// ── Tenancy: the boundary this type exists for ──────────────────────────────

// TestGet_ForeignTenantCannotObtainTheEnvelope is the regression test for the
// most severe defect the service has had.
//
// What Get returns is the signed envelope itself — the credential every other
// service on the platform trusts. Before DurableCache carried the tenant,
// knowing a session id was enough to obtain a working credential for that
// identity in any tenant. That is credential theft, not a data leak.
func TestGet_ForeignTenantCannotObtainTheEnvelope(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")

	_, err := f.dc.Get(context.Background(), "sc-1", "tenant-b")

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrSessionNotFound,
		"a foreign session must read as ABSENT — 'wrong tenant' would confirm the id exists")
}

func TestGet_OwnTenantSucceeds(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")

	jwt, err := f.dc.Get(context.Background(), "sc-1", "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, "jwt-sc-1", jwt)
}

// TestGet_RecordWithoutAJWTIsAnExpiredSession, not a store failure.
//
// The context record outlives the envelope: Postgres holds it for the retention
// window, Redis holds the JWT for the session window. Reporting the second as
// an error would send an operator looking at infrastructure for an expiry.
func TestGet_RecordWithoutAJWTIsExpiredNotBroken(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")
	delete(f.hot.jwts, "sc-1")

	_, err := f.dc.Get(context.Background(), "sc-1", "tenant-a")
	assert.ErrorIs(t, err, domain.ErrSessionNotFound)
}

func TestGetSessionContext_ForeignTenantReadsAsAbsent(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")

	sc, err := f.dc.GetSessionContext(context.Background(), "sc-1", "tenant-b")
	require.NoError(t, err, "a foreign session is a real answer, not a failure")
	assert.Nil(t, sc)
}

// TestGetSessionContext_FallsBackToPostgresWhenTheCacheHasAgedOut.
//
// Without the fallback, revoking a session that had merely aged out of Redis
// reported success and wrote nothing — the resolver treats a missing record as
// an idempotent no-op.
func TestGetSessionContext_FallsBackToTheDurableStore(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")
	delete(f.hot.contexts, "sc-1") // aged out of Redis, still in Postgres

	sc, err := f.dc.GetSessionContext(context.Background(), "sc-1", "tenant-a")
	require.NoError(t, err)
	require.NotNil(t, sc)
	assert.Equal(t, "p-1", sc.PrincipalID)
}

// TestGetSessionContext_CacheFailureStillFallsBack: a Redis error must degrade
// to Postgres rather than fail the read, because Redis is the optimisation and
// Postgres is the record.
func TestGetSessionContext_CacheFailureFallsBackRatherThanFailing(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")
	f.hot.getCtxErr = errors.New("redis down")

	sc, err := f.dc.GetSessionContext(context.Background(), "sc-1", "tenant-a")
	require.NoError(t, err)
	require.NotNil(t, sc)
}

func TestGetSessionContext_StoreFailureIsReported(t *testing.T) {
	f := newFixture()
	f.hot.getCtxErr = errors.New("redis down")
	f.store.findErr = errors.New("postgres down")

	_, err := f.dc.GetSessionContext(context.Background(), "sc-1", "tenant-a")
	require.Error(t, err, "with neither store readable there is no answer to give")
}

// ── Atomic evidence ─────────────────────────────────────────────────────────

// TestPersistWithEvent_PostgresFirstAndFatal pins the ORDER, which is the point.
//
// Postgres holds the only copy of the event since the outbox landed, so if it
// does not commit the caller must not issue an envelope. Redis is warmed second
// and best-effort: the envelope is independently verifiable from the JWKS, so a
// missing cache entry costs a re-resolve rather than a lost session.
func TestPersistWithEvent_PostgresFailureIsFatal(t *testing.T) {
	f := newFixture()
	f.store.withEventErr = errors.New("postgres gone")

	err := f.dc.PersistSessionContextWithEvent(context.Background(),
		domain.SessionContext{SessionContextID: "sc-1", TenantID: "tenant-a"},
		domain.ContextResolvedEvent{TenantID: "tenant-a", SessionContextID: "sc-1"})

	require.Error(t, err)
	assert.Empty(t, f.hot.contexts, "the cache must not be warmed for a session that was never recorded")
}

func TestPersistWithEvent_RedisFailureIsNotFatal(t *testing.T) {
	f := newFixture()
	f.hot.persistErr = errors.New("redis gone")

	err := f.dc.PersistSessionContextWithEvent(context.Background(),
		domain.SessionContext{SessionContextID: "sc-1", TenantID: "tenant-a"},
		domain.ContextResolvedEvent{TenantID: "tenant-a", SessionContextID: "sc-1"})

	require.NoError(t, err, "a warm-cache miss costs a re-resolve, not a resolution")
	assert.Len(t, f.store.contexts, 1)
}

func TestPersistWithEvent_RendersTheEventWithItsEvidenceId(t *testing.T) {
	f := newFixture()

	require.NoError(t, f.dc.PersistSessionContextWithEvent(context.Background(),
		domain.SessionContext{SessionContextID: "sc-1", TenantID: "tenant-a"},
		domain.ContextResolvedEvent{
			PrincipalID: "p-1", TenantID: "tenant-a", LegalEntityID: "e-1",
			SessionContextID: "sc-1", EvidenceID: "ev-1", CorrelationID: "corr-1",
		}))

	require.Len(t, f.store.events, 1)
	rec := f.store.events[0]
	assert.Equal(t, "identity.context.resolved", rec.EventType)
	assert.Equal(t, "sc-1", rec.PartitionKey, "ordering is per session, so the key is the session")
	assert.Contains(t, string(rec.Payload), "ev-1", "the event must cite the evidence the caller was given")
}

// ── Invalidation ────────────────────────────────────────────────────────────

func TestInvalidate_ForeignTenantIsANoOp(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")

	require.NoError(t, f.dc.Invalidate(context.Background(), "sc-1", "tenant-b",
		domain.InvalidationReasonAdminRevoke, time.Now().UTC()))

	assert.Empty(t, f.store.invalidated,
		"a session in another tenant must not be revocable by id alone")
	assert.Nil(t, f.store.contexts["sc-1"].InvalidatedAt)
}

func TestInvalidate_OwnTenantMarksBothStores(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")

	require.NoError(t, f.dc.Invalidate(context.Background(), "sc-1", "tenant-a",
		domain.InvalidationReasonLogout, time.Now().UTC()))

	assert.Equal(t, []string{"sc-1"}, f.store.invalidated)
	require.NotNil(t, f.store.contexts["sc-1"].InvalidatedAt)
}

func TestInvalidate_AbsentSessionIsANoOpNotAnError(t *testing.T) {
	f := newFixture()
	require.NoError(t, f.dc.Invalidate(context.Background(), "sc-nope", "tenant-a",
		domain.InvalidationReasonLogout, time.Now().UTC()))
}

// ── Principal-wide revocation ───────────────────────────────────────────────

// TestEvictAllForPrincipal_RevokesTheUnionOfBothSources.
//
// Neither source alone is sufficient: Redis loses sessions on a flush or an
// eviction under memory pressure, and Postgres is slower to reflect a session
// issued moments ago. A revocation that missed sessions because one source was
// stale is the failure that matters here.
func TestEvictAllForPrincipal_RevokesTheUnionOfBothSources(t *testing.T) {
	f := newFixture()
	f.seed("sc-cached", "tenant-a", "p-1")
	f.seed("sc-durable", "tenant-a", "p-1")

	f.hot.byPrincipal["p-1"] = []string{"sc-cached"}
	f.store.livePrincipal["p-1"] = []string{"sc-durable"}

	n, err := f.dc.EvictAllForPrincipal(context.Background(), "p-1", "tenant-a",
		domain.InvalidationReasonDelegationRevoked)

	require.NoError(t, err)
	assert.Equal(t, 2, n)

	sort.Strings(f.store.invalidated)
	assert.Equal(t, []string{"sc-cached", "sc-durable"}, f.store.invalidated)
}

// TestEvictAllForPrincipal_SkipsForeignTenantIdsInTheRedisIndex.
//
// The Redis reverse index is not tenant-scoped, so an id from another tenant
// can appear in it. Invalidate re-checks tenancy per session, which is what
// stops a principal-wide revocation reaching across the boundary.
func TestEvictAllForPrincipal_SkipsForeignIdsFromTheRedisIndex(t *testing.T) {
	f := newFixture()
	f.seed("sc-ours", "tenant-a", "p-1")
	f.seed("sc-theirs", "tenant-b", "p-1")
	f.hot.byPrincipal["p-1"] = []string{"sc-ours", "sc-theirs"}

	n, err := f.dc.EvictAllForPrincipal(context.Background(), "p-1", "tenant-a",
		domain.InvalidationReasonAdminRevoke)

	require.NoError(t, err)
	assert.Equal(t, 2, n, "both ids are walked")
	assert.Equal(t, []string{"sc-ours"}, f.store.invalidated,
		"but only the caller's tenant is actually revoked")
	assert.Nil(t, f.store.contexts["sc-theirs"].InvalidatedAt)
}

// TestEvictAllForPrincipal_PartialRevocationBeatsNone.
//
// If the durable lookup fails but Redis gave us ids, revoke those. Failing
// outright would leave every session live because one of two sources was down.
func TestEvictAllForPrincipal_DurableFailureStillRevokesTheCachedSet(t *testing.T) {
	f := newFixture()
	f.seed("sc-cached", "tenant-a", "p-1")
	f.hot.byPrincipal["p-1"] = []string{"sc-cached"}
	f.store.livePrincErr = errors.New("postgres down")

	n, err := f.dc.EvictAllForPrincipal(context.Background(), "p-1", "tenant-a",
		domain.InvalidationReasonRiskEscalation)

	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestEvictAllForPrincipal_BothSourcesFailingIsAnError(t *testing.T) {
	f := newFixture()
	f.hot.principalErr = errors.New("redis down")
	f.store.livePrincErr = errors.New("postgres down")

	_, err := f.dc.EvictAllForPrincipal(context.Background(), "p-1", "tenant-a",
		domain.InvalidationReasonAdminRevoke)

	require.Error(t, err, "with neither source readable a revocation cannot claim to have run")
}

func TestEvictAllForPrincipal_ClearsTheReverseIndex(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")
	f.hot.byPrincipal["p-1"] = []string{"sc-1"}

	_, err := f.dc.EvictAllForPrincipal(context.Background(), "p-1", "tenant-a",
		domain.InvalidationReasonAdminRevoke)
	require.NoError(t, err)

	assert.Equal(t, []string{"p-1"}, f.hot.clearedPrincipal)
}

// ── Entity-wide revocation ──────────────────────────────────────────────────

func TestEvictAllForEntity_RevokesEntityScopedSessions(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")
	f.hot.byEntity["entity-1"] = []string{"sc-1"}

	n, err := f.dc.EvictAllForEntity(context.Background(), "entity-1", "tenant-a",
		domain.InvalidationReasonAdminRevoke)

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"entity-1"}, f.hot.clearedEntity)
}

func TestEvictAllForEntity_NoSessionsIsNotAnError(t *testing.T) {
	f := newFixture()
	n, err := f.dc.EvictAllForEntity(context.Background(), "entity-nobody", "tenant-a",
		domain.InvalidationReasonAdminRevoke)
	require.NoError(t, err)
	assert.Zero(t, n)
}

// ── Tenant-wide revocation ──────────────────────────────────────────────────

// TestEvictAllForTenant_ReadsTheDurableStoreAlone.
//
// There is no Redis reverse index by tenant, and that is fine here: the durable
// write is the one that cannot be skipped since the outbox landed, so the
// durable list is complete for this purpose.
func TestEvictAllForTenant_RevokesEveryLiveSession(t *testing.T) {
	f := newFixture()
	f.seed("sc-1", "tenant-a", "p-1")
	f.seed("sc-2", "tenant-a", "p-2")
	f.seed("sc-other", "tenant-b", "p-3")
	f.store.liveTenant["tenant-a"] = []string{"sc-1", "sc-2"}

	n, err := f.dc.EvictAllForTenant(context.Background(), "tenant-a",
		domain.InvalidationReasonAdminRevoke)

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Nil(t, f.store.contexts["sc-other"].InvalidatedAt,
		"a tenant-wide revocation must stop at its own tenant")
}

func TestEvictAllForTenant_StoreFailureIsReported(t *testing.T) {
	f := newFixture()
	f.store.liveTenantErr = errors.New("postgres down")

	_, err := f.dc.EvictAllForTenant(context.Background(), "tenant-a",
		domain.InvalidationReasonAdminRevoke)
	require.Error(t, err)
}

func TestEvictAllForTenant_NothingLiveIsZeroNotAnError(t *testing.T) {
	f := newFixture()
	n, err := f.dc.EvictAllForTenant(context.Background(), "tenant-quiet",
		domain.InvalidationReasonAdminRevoke)
	require.NoError(t, err)
	assert.Zero(t, n)
}

// ── Put ─────────────────────────────────────────────────────────────────────

// TestPut_IsNotTenantScoped documents a deliberate asymmetry.
//
// Tenancy is enforced on READ, not here: the id is generated by the resolver in
// the same call that persisted the context record, so at Put time there is
// nothing yet to compare against.
func TestPut_StoresWithoutATenantArgument(t *testing.T) {
	f := newFixture()
	require.NoError(t, f.dc.Put(context.Background(), "sc-1", "jwt-1"))
	assert.Equal(t, "jwt-1", f.hot.jwts["sc-1"])
}

var _ session.HotCache = (*fakeHot)(nil)
var _ session.ContextStore = (*fakeStore)(nil)
