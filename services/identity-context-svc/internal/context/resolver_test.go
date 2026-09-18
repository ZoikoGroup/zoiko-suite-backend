package context_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/config"
	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/siem"
)

// ── Test fixtures ─────────────────────────────────────────────────────────────

var activePrincipal = &domain.Principal{
	PrincipalID:             "01HXXXPRINCIPALID",
	TenantID:                "01HXXXTENANTID",
	PrincipalType:           domain.PrincipalTypeHuman,
	IdentityProviderSubject: "auth0|testuser",
	Email:                   "test@zoiko.io",
	DisplayName:             "Test User",
	Status:                  domain.PrincipalStatusActive,
	CreatedAt:               time.Now(),
}

var validClaims = &domain.VerifiedClaims{
	Subject:  "auth0|testuser",
	TenantID: "01HXXXTENANTID",
	MFADone:  false,
}

var baseRequest = domain.ResolveRequest{
	BearerToken:   "mock-token",
	LegalEntityID: "01HXXXENTITYID",
	CorrelationID: "01HXXXCORRELID",
}

var testCfg = &config.Config{
	JWTIssuer:             "identity-context-svc",
	JWTAudienceInternal:   "zoiko-internal",
	EnvelopeJWTTTLSeconds: 300,
}

// ── Mock implementations ──────────────────────────────────────────────────────

// mockPrincipalStore
type mockPrincipalStore struct {
	principal   *domain.Principal
	findErr     error
	assignments []domain.PrincipalRoleAssignment
	delegations []domain.DelegatedAuthority
}

func (m *mockPrincipalStore) FindByIDPSubject(_ context.Context, _, _ string) (*domain.Principal, error) {
	return m.principal, m.findErr
}
func (m *mockPrincipalStore) FindByID(_ context.Context, _, _ string) (*domain.Principal, error) {
	return m.principal, m.findErr
}
func (m *mockPrincipalStore) FindActiveRoleAssignments(_ context.Context, _, _ string, _ *string) ([]domain.PrincipalRoleAssignment, error) {
	return m.assignments, nil
}
func (m *mockPrincipalStore) FindActiveDelegations(_ context.Context, _, _ string) ([]domain.DelegatedAuthority, error) {
	return m.delegations, nil
}
func (m *mockPrincipalStore) UpdateStatus(_ context.Context, _, _ string, _ domain.PrincipalStatus, _, _ string) error {
	return nil
}

// mockSessionCache
//
// GUARDED BY A MUTEX. The concurrency profile in nfr_test.go drives sixteen
// goroutines through Resolve at once, and the real DurableCache is safe there
// because its two backing stores are — Postgres and Redis clients both are.
// A bare Go map is not, and the unguarded version failed with "concurrent map
// writes" the moment the load test ran, which is a defect in the fixture
// rather than in what it stands for.
type mockSessionCache struct {
	mu          sync.Mutex
	stored      map[string]string
	storedCtx   map[string]*domain.SessionContext
	invalidated []string

	// persistErr forces the atomic evidence write to fail.
	persistErr error
	// resolvedEvents records the identity.context.resolved events that were
	// written alongside each session, so a test can assert the two really do
	// travel together rather than merely that the row landed.
	resolvedEvents []domain.ContextResolvedEvent
}

func newMockSessionCache() *mockSessionCache {
	return &mockSessionCache{
		stored:    map[string]string{},
		storedCtx: map[string]*domain.SessionContext{},
	}
}
func (m *mockSessionCache) Put(_ context.Context, id, jwt string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stored[id] = jwt
	return nil
}
func (m *mockSessionCache) Get(_ context.Context, id, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.stored[id]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}
func (m *mockSessionCache) Evict(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.stored, id)
	return nil
}
func (m *mockSessionCache) PersistSessionContext(_ context.Context, sc domain.SessionContext) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storedCtx[sc.SessionContextID] = &sc
	return nil
}

// persistErr, when set, makes the atomic evidence write fail — which the
// resolver now treats as fatal to the resolution. See
// TestResolveRefusesWhenEvidenceCannotBeRecorded.
func (m *mockSessionCache) PersistSessionContextWithEvent(
	_ context.Context,
	sc domain.SessionContext,
	ev domain.ContextResolvedEvent,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.persistErr != nil {
		return m.persistErr
	}
	m.storedCtx[sc.SessionContextID] = &sc
	m.resolvedEvents = append(m.resolvedEvents, ev)
	return nil
}
func (m *mockSessionCache) GetSessionContext(_ context.Context, id, _ string) (*domain.SessionContext, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.storedCtx[id], nil
}
func (m *mockSessionCache) Invalidate(_ context.Context, id, _ string, reason domain.InvalidationReason, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invalidated = append(m.invalidated, id)
	if sc, ok := m.storedCtx[id]; ok {
		sc.InvalidatedAt = &at
		sc.InvalidationReason = &reason
	}
	return nil
}
func (m *mockSessionCache) EvictAllForPrincipal(_ context.Context, _, _ string, _ domain.InvalidationReason) (int, error) {
	return 0, nil
}

// mockRiskSignalCache
type mockRiskSignalCache struct {
	signal *domain.RiskSignalCache
	err    error
}

func (m *mockRiskSignalCache) GetLatestSignal(_ context.Context, _ string) (*domain.RiskSignalCache, error) {
	return m.signal, m.err
}

// mockUpstreamRegistry
type mockUpstreamRegistry struct {
	tenantActive            bool
	tenantErr               error
	entityAuthz             bool
	entityErr               error
	entityResidencyPolicyID string
	permBundles             []string
}

func defaultUpstream() *mockUpstreamRegistry {
	return &mockUpstreamRegistry{tenantActive: true, entityAuthz: true}
}
func (m *mockUpstreamRegistry) IsTenantActive(_ context.Context, _ string) (bool, error) {
	return m.tenantActive, m.tenantErr
}
func (m *mockUpstreamRegistry) ResolveEntityScope(_ context.Context, _, _, _ string) (*domain.EntityScope, error) {
	if m.entityErr != nil {
		return nil, m.entityErr
	}
	return &domain.EntityScope{
		Authorized:            m.entityAuthz,
		DataResidencyPolicyID: m.entityResidencyPolicyID,
	}, nil
}
func (m *mockUpstreamRegistry) ResolvePermissionBundles(_ context.Context, _ string, _ []string) ([]string, error) {
	return m.permBundles, nil
}

// mockEventPublisher is a thread-safe event publisher for tests.
//
// The production Resolver launches publish calls in fire-and-forget goroutines
// (e.g. go r.events.PublishContextResolved(...)). Those goroutines write to
// this mock concurrently with the test goroutine reading the counters — a data
// race without synchronization.
//
// Two fixes applied:
//  1. sync.Mutex protects every counter read and write.
//  2. Each publish method sends on a buffered channel so tests can block until
//     the goroutine has actually completed, instead of using time.Sleep which
//     only hides the race intermittently.
type mockEventPublisher struct {
	mu              sync.Mutex
	resolved        int
	failed          int
	invalidated     int
	riskUnavailable int

	// Buffered channels (capacity 10) — each successful publish sends one token.
	// Tests receive from these instead of sleeping.
	ResolvedCh        chan struct{}
	FailedCh          chan struct{}
	InvalidatedCh     chan struct{}
	RiskUnavailableCh chan struct{}
}

func newMockEventPublisher() *mockEventPublisher {
	return &mockEventPublisher{
		ResolvedCh:        make(chan struct{}, 10),
		FailedCh:          make(chan struct{}, 10),
		InvalidatedCh:     make(chan struct{}, 10),
		RiskUnavailableCh: make(chan struct{}, 10),
	}
}

// waitResolved blocks until n resolved events have been published or the
// test deadline is exceeded. Returns the count observed.
func (m *mockEventPublisher) waitResolved(t *testing.T, n int) int {
	t.Helper()
	return waitN(t, m.ResolvedCh, n)
}
func (m *mockEventPublisher) waitFailed(t *testing.T, n int) int {
	t.Helper()
	return waitN(t, m.FailedCh, n)
}
func (m *mockEventPublisher) waitRiskUnavailable(t *testing.T, n int) int {
	t.Helper()
	return waitN(t, m.RiskUnavailableCh, n)
}

// waitN drains n tokens from ch or fails the test on timeout.
func waitN(t *testing.T, ch <-chan struct{}, n int) int {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for goroutine event (got %d of %d)", i, n)
		}
	}
	return n
}

// Resolved returns the current resolved count, safe for concurrent access.
func (m *mockEventPublisher) Resolved() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolved
}
func (m *mockEventPublisher) Failed() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failed
}
func (m *mockEventPublisher) RiskUnavailable() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.riskUnavailable
}

// notify sends a completion token without ever blocking.
//
// The channels are buffered at 10, which is plenty for a test asserting on a
// handful of publishes and NOT enough for the load profile in nfr_test.go,
// which drives thousands of resolutions. A blocking send there wedged the
// publish goroutines forever and made Drain time out — reported as a goroutine
// leak in the RESOLVER, which was wrong: the resolver was fine and the fixture
// was the thing that could not keep up.
//
// Dropping a token when nobody is waiting is the correct behaviour for what
// these channels are: a synchronisation aid for tests that call waitN, not an
// accounting record. The counters above are the accounting record, and they
// are taken under the mutex before this is reached.
func (m *mockEventPublisher) notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (m *mockEventPublisher) PublishContextResolved(_ context.Context, _, _, _, _, _ string) error {
	m.mu.Lock()
	m.resolved++
	m.mu.Unlock()
	m.notify(m.ResolvedCh)
	return nil
}
func (m *mockEventPublisher) PublishResolutionFailed(_ context.Context, _, _, _ string) error {
	m.mu.Lock()
	m.failed++
	m.mu.Unlock()
	m.notify(m.FailedCh)
	return nil
}
func (m *mockEventPublisher) PublishSessionInvalidated(_ context.Context, _, _ string, _ domain.InvalidationReason, _ string) error {
	m.mu.Lock()
	m.invalidated++
	m.mu.Unlock()
	m.notify(m.InvalidatedCh)
	return nil
}
func (m *mockEventPublisher) PublishRiskSignalUnavailable(_ context.Context, _, _ string) error {
	m.mu.Lock()
	m.riskUnavailable++
	m.mu.Unlock()
	m.notify(m.RiskUnavailableCh)
	return nil
}
func (m *mockEventPublisher) PublishPrincipalStatusChanged(_ context.Context, _, _ string, _ domain.PrincipalStatus, _, _ string) error {
	return nil
}

// mockTokenVerifier
type mockTokenVerifier struct {
	claims *domain.VerifiedClaims
	err    error
}

func (m *mockTokenVerifier) VerifyBearer(_ context.Context, _ string) (*domain.VerifiedClaims, error) {
	return m.claims, m.err
}

// mockEnvelopeSigner
type mockEnvelopeSigner struct{ err error }

func (m *mockEnvelopeSigner) Sign(_ *domain.IdentityContextEnvelope) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return "signed-envelope-jwt", nil
}

// ── Builder helper ────────────────────────────────────────────────────────────

type resolverFixture struct {
	principals  *mockPrincipalStore
	sessions    *mockSessionCache
	riskSignals *mockRiskSignalCache
	upstream    *mockUpstreamRegistry
	events      *mockEventPublisher
	verifier    *mockTokenVerifier
	signer      *mockEnvelopeSigner
}

func defaultFixture() *resolverFixture {
	return &resolverFixture{
		principals:  &mockPrincipalStore{principal: activePrincipal},
		sessions:    newMockSessionCache(),
		riskSignals: &mockRiskSignalCache{signal: nil}, // cache miss → STANDARD
		upstream:    defaultUpstream(),
		events:      newMockEventPublisher(),
		verifier:    &mockTokenVerifier{claims: validClaims},
		signer:      &mockEnvelopeSigner{},
	}
}

func (f *resolverFixture) build() *identityctx.Resolver {
	return identityctx.NewResolver(
		testCfg,
		zap.NewNop(),
		f.principals,
		f.sessions,
		f.riskSignals,
		f.upstream,
		f.events,
		f.verifier,
		f.signer,
		siem.New("", "identity-context-svc", zap.NewNop()),
	)
}

// ── Test suite ────────────────────────────────────────────────────────────────

func TestResolve_AllSixDimensionsSuccess(t *testing.T) {
	f := defaultFixture()
	result, err := f.build().Resolve(context.Background(), baseRequest)

	require.NoError(t, err)
	assert.Equal(t, "signed-envelope-jwt", result.EnvelopeJWT)

	// The spec's envelope section requires an evidence_id on every material
	// governance decision, and issuing a signed identity envelope is the most
	// material one this service makes. Resolve used to return a bare JWT, so
	// a caller had no way to cite the decision that granted it.
	assert.NotEmpty(t, result.EvidenceID, "resolution must return an evidence id")
	assert.NotEmpty(t, result.SessionContextID)
	assert.True(t, result.ExpiresAt.After(time.Now()), "envelope must not be issued already expired")
}

// TestResolve_WritesContextResolvedEventAtomically pins the transactional
// outbox at the one place it matters.
//
// The event is NO LONGER published from a goroutine. It is written into
// event_outbox in the same transaction as the session_contexts row, so this
// asserts on the atomic write rather than waiting for a fire-and-forget
// publish that will never arrive.
//
// That is the behaviour change the outbox exists for: before it, the row and
// the event could fail independently, and both "a session with no event" and
// "an event for a session that failed to persist" were reachable states.
func TestResolve_WritesContextResolvedEventAtomically(t *testing.T) {
	f := defaultFixture()
	result, err := f.build().Resolve(context.Background(), baseRequest)
	require.NoError(t, err)

	require.Len(t, f.sessions.resolvedEvents, 1,
		"the context.resolved event must be written with the session row, not published separately")

	ev := f.sessions.resolvedEvents[0]
	assert.Equal(t, activePrincipal.PrincipalID, ev.PrincipalID)
	assert.Equal(t, result.SessionContextID, ev.SessionContextID)
	assert.Equal(t, result.EvidenceID, ev.EvidenceID,
		"the event must cite the same evidence id the caller was given")

	// And nothing went out through the old fire-and-forget path.
	assert.Equal(t, 0, f.events.Resolved(),
		"context.resolved must not also be published out-of-band — that would double-emit")
}

// TestResolveRefusesWhenEvidenceCannotBeRecorded pins the changed stance on
// evidence failure.
//
// This used to log-and-continue: a resolution that succeeded on all six
// dimensions was not failed by an evidence-store hiccup. That was right when
// Postgres held a mere duplicate of the Redis cache. It is wrong now that the
// same transaction holds the ONLY copy of the event — a failure means no
// evidence exists anywhere, and the service would be issuing a signed platform
// credential with no record that it did so.
func TestResolveRefusesWhenEvidenceCannotBeRecorded(t *testing.T) {
	f := defaultFixture()
	f.sessions.persistErr = errors.New("postgres is gone")

	result, err := f.build().Resolve(context.Background(), baseRequest)

	require.Error(t, err)
	assert.Nil(t, result, "no envelope may be handed out when its issuance was not recorded")
	assert.ErrorIs(t, err, identityctx.ErrUpstreamUnavailable,
		"an unrecordable decision is a 503, not a credential")
}

func TestResolve_PersistsSessionContext(t *testing.T) {
	f := defaultFixture()
	_, err := f.build().Resolve(context.Background(), baseRequest)
	require.NoError(t, err)
	assert.Len(t, f.sessions.storedCtx, 1)
	for _, sc := range f.sessions.storedCtx {
		assert.Equal(t, activePrincipal.PrincipalID, sc.PrincipalID)
		assert.Equal(t, baseRequest.LegalEntityID, sc.LegalEntityID)
		assert.Nil(t, sc.InvalidatedAt) // never invalidated at creation
	}
}

// ── Dimension 1: Authenticated principal (fail-closed) ───────────────────────

func TestResolve_FailsClosed_TokenInvalid(t *testing.T) {
	f := defaultFixture()
	f.verifier = &mockTokenVerifier{err: errors.New("bad signature")}
	_, err := f.build().Resolve(context.Background(), baseRequest)

	require.ErrorIs(t, err, identityctx.ErrTokenInvalid)
	f.events.waitFailed(t, 1)
	assert.Equal(t, 1, f.events.Failed())
}

func TestResolve_FailsClosed_PrincipalSuspended(t *testing.T) {
	f := defaultFixture()
	f.principals = &mockPrincipalStore{principal: &domain.Principal{Status: domain.PrincipalStatusSuspended}}
	_, err := f.build().Resolve(context.Background(), baseRequest)

	require.ErrorIs(t, err, identityctx.ErrPrincipalInactive)
	f.events.waitFailed(t, 1)
	assert.Equal(t, 1, f.events.Failed())
}

func TestResolve_FailsClosed_PrincipalNotFound(t *testing.T) {
	f := defaultFixture()
	f.principals = &mockPrincipalStore{principal: nil}
	_, err := f.build().Resolve(context.Background(), baseRequest)
	require.ErrorIs(t, err, identityctx.ErrPrincipalInactive)
}

// ── Dimension 2: Tenant (fail-closed) ─────────────────────────────────────────

func TestResolve_FailsClosed_TenantInactive(t *testing.T) {
	f := defaultFixture()
	f.upstream = &mockUpstreamRegistry{tenantActive: false, entityAuthz: true}
	_, err := f.build().Resolve(context.Background(), baseRequest)
	require.ErrorIs(t, err, identityctx.ErrTenantInactive)
}

func TestResolve_FailsClosed_TenantRegistryUnreachable(t *testing.T) {
	f := defaultFixture()
	f.upstream = &mockUpstreamRegistry{tenantErr: errors.New("network timeout"), entityAuthz: true}
	_, err := f.build().Resolve(context.Background(), baseRequest)
	require.ErrorIs(t, err, identityctx.ErrUpstreamUnavailable)
}

// ── Dimension 3: Entity scope (fail-closed) ───────────────────────────────────

func TestResolve_FailsClosed_EntityUnauthorized(t *testing.T) {
	f := defaultFixture()
	f.upstream = &mockUpstreamRegistry{tenantActive: true, entityAuthz: false}
	_, err := f.build().Resolve(context.Background(), baseRequest)
	require.ErrorIs(t, err, identityctx.ErrEntityUnauthorized)
}

func TestResolve_FailsClosed_EntityRegistryUnreachable(t *testing.T) {
	f := defaultFixture()
	f.upstream = &mockUpstreamRegistry{tenantActive: true, entityErr: errors.New("conn refused")}
	_, err := f.build().Resolve(context.Background(), baseRequest)
	require.ErrorIs(t, err, identityctx.ErrUpstreamUnavailable)
}

// ── Dimension 6: Trust posture (fail-closed on BLOCKED) ───────────────────────

func TestResolve_FailsClosed_TrustPostureBlocked(t *testing.T) {
	f := defaultFixture()
	f.riskSignals = &mockRiskSignalCache{signal: &domain.RiskSignalCache{
		RiskSignalID: "sig1",
		SignalValue:  85, // >= 80 → BLOCKED
		SignalSource: "RULES_ENGINE",
		ValidTo:      time.Now().Add(time.Hour),
	}}
	_, err := f.build().Resolve(context.Background(), baseRequest)

	require.ErrorIs(t, err, identityctx.ErrTrustPostureBlocked)
	f.events.waitFailed(t, 1)
	assert.Equal(t, 1, f.events.Failed())
}

// ── Q3: Risk signal cache unavailability (hot-path isolation) ─────────────────

func TestResolve_RiskCacheUnavailable_DefaultsToStandard_DoesNotBlock(t *testing.T) {
	f := defaultFixture()
	// nil signal → cache miss — resolver must default to STANDARD and succeed
	f.riskSignals = &mockRiskSignalCache{signal: nil}

	result, err := f.build().Resolve(context.Background(), baseRequest)
	require.NoError(t, err)
	assert.Equal(t, "signed-envelope-jwt", result.EnvelopeJWT)

	f.events.waitRiskUnavailable(t, 1)
	assert.Equal(t, 1, f.events.RiskUnavailable())
}

func TestResolve_RiskCacheErrors_DefaultsToStandard_DoesNotBlock(t *testing.T) {
	f := defaultFixture()
	f.riskSignals = &mockRiskSignalCache{err: errors.New("redis timeout")}

	result, err := f.build().Resolve(context.Background(), baseRequest)
	require.NoError(t, err)
	assert.Equal(t, "signed-envelope-jwt", result.EnvelopeJWT)
}

// ── Mutual exclusivity of token inputs ────────────────────────────────────────

func TestResolve_FailsWhenNeitherTokenProvided(t *testing.T) {
	f := defaultFixture()
	req := domain.ResolveRequest{LegalEntityID: "eid", CorrelationID: "cid"}
	_, err := f.build().Resolve(context.Background(), req)
	require.ErrorIs(t, err, identityctx.ErrNoToken)
}

func TestResolve_FailsWhenBothTokensProvided(t *testing.T) {
	f := defaultFixture()
	req := domain.ResolveRequest{
		BearerToken: "tok", SAMLAssertion: "saml",
		LegalEntityID: "eid", CorrelationID: "cid",
	}
	_, err := f.build().Resolve(context.Background(), req)
	require.ErrorIs(t, err, identityctx.ErrNoToken)
}

// ── InvalidateSession idempotency ────────────────────────────────────────────

func TestInvalidateSession_Idempotent_AlreadyInvalidated(t *testing.T) {
	f := defaultFixture()
	r := f.build()

	// Resolve to create a session
	_, err := r.Resolve(context.Background(), baseRequest)
	require.NoError(t, err)

	// Retrieve the session context ID (one item in storedCtx)
	var sessionID string
	for id := range f.sessions.storedCtx {
		sessionID = id
	}
	require.NotEmpty(t, sessionID)

	// First invalidation
	err = r.InvalidateSession(context.Background(), sessionID, "01HXXXTENANTID", domain.InvalidationReasonLogout, "admin", "corr1")
	require.NoError(t, err)
	assert.Len(t, f.sessions.invalidated, 1)

	// Second invalidation — idempotent; Invalidate must NOT be called again
	err = r.InvalidateSession(context.Background(), sessionID, "01HXXXTENANTID", domain.InvalidationReasonLogout, "admin", "corr2")
	require.NoError(t, err)
	assert.Len(t, f.sessions.invalidated, 1) // still 1, not 2
}

func TestInvalidateSession_NoOp_WhenSessionNotFound(t *testing.T) {
	f := defaultFixture()
	err := f.build().InvalidateSession(context.Background(), "nonexistent-id", "01HXXXTENANTID", domain.InvalidationReasonLogout, "admin", "corr1")
	require.NoError(t, err) // must not error on missing session
	assert.Empty(t, f.sessions.invalidated)
}
