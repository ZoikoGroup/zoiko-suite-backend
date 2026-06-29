package context_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
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
	principal  *domain.Principal
	findErr    error
	assignments []domain.PrincipalRoleAssignment
	delegations []domain.DelegatedAuthority
}

func (m *mockPrincipalStore) FindByIDPSubject(_ context.Context, _, _ string) (*domain.Principal, error) {
	return m.principal, m.findErr
}
func (m *mockPrincipalStore) FindByID(_ context.Context, _ string) (*domain.Principal, error) {
	return m.principal, m.findErr
}
func (m *mockPrincipalStore) FindActiveRoleAssignments(_ context.Context, _ string, _ *string) ([]domain.PrincipalRoleAssignment, error) {
	return m.assignments, nil
}
func (m *mockPrincipalStore) FindActiveDelegations(_ context.Context, _ string) ([]domain.DelegatedAuthority, error) {
	return m.delegations, nil
}
func (m *mockPrincipalStore) UpdateStatus(_ context.Context, _ string, _ domain.PrincipalStatus, _, _ string) error {
	return nil
}

// mockSessionCache
type mockSessionCache struct {
	stored      map[string]string
	storedCtx   map[string]*domain.SessionContext
	invalidated []string
}

func newMockSessionCache() *mockSessionCache {
	return &mockSessionCache{
		stored:    map[string]string{},
		storedCtx: map[string]*domain.SessionContext{},
	}
}
func (m *mockSessionCache) Put(_ context.Context, id, jwt string) error {
	m.stored[id] = jwt
	return nil
}
func (m *mockSessionCache) Get(_ context.Context, id string) (string, error) {
	if v, ok := m.stored[id]; ok {
		return v, nil
	}
	return "", errors.New("not found")
}
func (m *mockSessionCache) Evict(_ context.Context, id string) error {
	delete(m.stored, id)
	return nil
}
func (m *mockSessionCache) PersistSessionContext(_ context.Context, sc domain.SessionContext) error {
	m.storedCtx[sc.SessionContextID] = &sc
	return nil
}
func (m *mockSessionCache) GetSessionContext(_ context.Context, id string) (*domain.SessionContext, error) {
	return m.storedCtx[id], nil
}
func (m *mockSessionCache) Invalidate(_ context.Context, id string, reason domain.InvalidationReason, at time.Time) error {
	m.invalidated = append(m.invalidated, id)
	if sc, ok := m.storedCtx[id]; ok {
		sc.InvalidatedAt = &at
		sc.InvalidationReason = &reason
	}
	return nil
}
func (m *mockSessionCache) EvictAllForPrincipal(_ context.Context, _ string) error { return nil }

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
	tenantActive   bool
	tenantErr      error
	entityAuthz    bool
	entityErr      error
	permBundles    []string
	delegations    []domain.DelegatedAuthority
	delegationsErr error
}

func defaultUpstream() *mockUpstreamRegistry {
	return &mockUpstreamRegistry{tenantActive: true, entityAuthz: true}
}
func (m *mockUpstreamRegistry) IsTenantActive(_ context.Context, _ string) (bool, error) {
	return m.tenantActive, m.tenantErr
}
func (m *mockUpstreamRegistry) IsPrincipalAuthorizedForEntity(_ context.Context, _, _ string) (bool, error) {
	return m.entityAuthz, m.entityErr
}
func (m *mockUpstreamRegistry) ResolvePermissionBundles(_ context.Context, _ []string) ([]string, error) {
	return m.permBundles, nil
}
func (m *mockUpstreamRegistry) FetchActiveDelegations(_ context.Context, _, _ string) ([]domain.DelegatedAuthority, error) {
	return m.delegations, m.delegationsErr
}

// mockEventPublisher
type mockEventPublisher struct {
	resolved        int
	failed          int
	invalidated     int
	riskUnavailable int
}

func (m *mockEventPublisher) PublishContextResolved(_ context.Context, _, _, _, _, _ string) {
	m.resolved++
}
func (m *mockEventPublisher) PublishResolutionFailed(_ context.Context, _, _, _ string) { m.failed++ }
func (m *mockEventPublisher) PublishSessionInvalidated(_ context.Context, _, _ string, _ domain.InvalidationReason, _ string) {
	m.invalidated++
}
func (m *mockEventPublisher) PublishRiskSignalUnavailable(_ context.Context, _, _ string) {
	m.riskUnavailable++
}
func (m *mockEventPublisher) PublishPrincipalStatusChanged(_ context.Context, _, _ string, _ domain.PrincipalStatus, _, _ string) {
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
		events:      &mockEventPublisher{},
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
	)
}

// ── Test suite ────────────────────────────────────────────────────────────────

func TestResolve_AllSixDimensionsSuccess(t *testing.T) {
	f := defaultFixture()
	jwt, err := f.build().Resolve(context.Background(), baseRequest)

	require.NoError(t, err)
	assert.Equal(t, "signed-envelope-jwt", jwt)
}

func TestResolve_PublishesContextResolvedEvent(t *testing.T) {
	f := defaultFixture()
	_, err := f.build().Resolve(context.Background(), baseRequest)
	require.NoError(t, err)
	// Allow goroutine to fire
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 1, f.events.resolved)
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
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 1, f.events.failed) // failure event published
}

func TestResolve_FailsClosed_PrincipalSuspended(t *testing.T) {
	f := defaultFixture()
	f.principals = &mockPrincipalStore{principal: &domain.Principal{Status: domain.PrincipalStatusSuspended}}
	_, err := f.build().Resolve(context.Background(), baseRequest)

	require.ErrorIs(t, err, identityctx.ErrPrincipalInactive)
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 1, f.events.failed)
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
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 1, f.events.failed)
}

// ── Q3: Risk signal cache unavailability (hot-path isolation) ─────────────────

func TestResolve_RiskCacheUnavailable_DefaultsToStandard_DoesNotBlock(t *testing.T) {
	f := defaultFixture()
	// nil signal → cache miss — resolver must default to STANDARD and succeed
	f.riskSignals = &mockRiskSignalCache{signal: nil}

	jwt, err := f.build().Resolve(context.Background(), baseRequest)
	require.NoError(t, err)
	assert.Equal(t, "signed-envelope-jwt", jwt)

	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 1, f.events.riskUnavailable) // event emitted
}

func TestResolve_RiskCacheErrors_DefaultsToStandard_DoesNotBlock(t *testing.T) {
	f := defaultFixture()
	f.riskSignals = &mockRiskSignalCache{err: errors.New("redis timeout")}

	jwt, err := f.build().Resolve(context.Background(), baseRequest)
	require.NoError(t, err)
	assert.Equal(t, "signed-envelope-jwt", jwt)
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
	err = r.InvalidateSession(context.Background(), sessionID, domain.InvalidationReasonLogout, "admin", "corr1")
	require.NoError(t, err)
	assert.Len(t, f.sessions.invalidated, 1)

	// Second invalidation — idempotent; Invalidate must NOT be called again
	err = r.InvalidateSession(context.Background(), sessionID, domain.InvalidationReasonLogout, "admin", "corr2")
	require.NoError(t, err)
	assert.Len(t, f.sessions.invalidated, 1) // still 1, not 2
}

func TestInvalidateSession_NoOp_WhenSessionNotFound(t *testing.T) {
	f := defaultFixture()
	err := f.build().InvalidateSession(context.Background(), "nonexistent-id", domain.InvalidationReasonLogout, "admin", "corr1")
	require.NoError(t, err) // must not error on missing session
	assert.Empty(t, f.sessions.invalidated)
}
