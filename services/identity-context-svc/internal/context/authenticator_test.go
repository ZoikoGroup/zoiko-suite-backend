package context_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	authpkg "zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/config"
	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/credential"
	"zoiko.io/identity-context-svc/internal/domain"
)

// ── Test doubles ─────────────────────────────────────────────────────────────

type authCall struct {
	basis         string
	correlationID string
	principalID   string
	newHash       string
}

type mockCredentialStore struct {
	principal  *domain.Principal
	credential *domain.PrincipalCredential
	lookupErr  error

	successCalls []authCall
	failureCalls []authCall
	deniedCalls  []authCall
	upserts      []authCall

	successErr error
	failureErr error

	// lockAfter makes RecordAuthFailure report a lock once the running count
	// reaches maxAttempts, mirroring the SQL the real store executes.
	failureCount int
}

func (m *mockCredentialStore) FindActiveCredentialByEmail(_ context.Context, _, _ string) (*domain.Principal, *domain.PrincipalCredential, error) {
	if m.lookupErr != nil {
		return m.principal, nil, m.lookupErr
	}
	return m.principal, m.credential, nil
}

func (m *mockCredentialStore) RecordAuthSuccess(_ context.Context, _, principalID, _, correlationID, newHash string) error {
	m.successCalls = append(m.successCalls, authCall{
		principalID: principalID, correlationID: correlationID, newHash: newHash,
	})
	return m.successErr
}

func (m *mockCredentialStore) RecordAuthFailure(_ context.Context, _, principalID, _, correlationID string, maxAttempts int, _ time.Duration) (bool, int, error) {
	m.failureCount++
	m.failureCalls = append(m.failureCalls, authCall{
		principalID: principalID, correlationID: correlationID,
	})
	return m.failureCount >= maxAttempts, m.failureCount, m.failureErr
}

func (m *mockCredentialStore) RecordAuthDenied(_ context.Context, principalID, _, basis, correlationID string) error {
	m.deniedCalls = append(m.deniedCalls, authCall{
		principalID: principalID, basis: basis, correlationID: correlationID,
	})
	return nil
}

func (m *mockCredentialStore) UpsertPasswordCredential(_ context.Context, principalID, _, secretHash, _ string) error {
	m.upserts = append(m.upserts, authCall{principalID: principalID, newHash: secretHash})
	return nil
}

type issuedToken struct {
	subject  string
	tenantID string
	mfaDone  bool
}

type mockTokenIssuer struct {
	issued []issuedToken
	token  string
	err    error
}

func (m *mockTokenIssuer) Issue(subject, tenantID string, mfaDone bool) (string, error) {
	m.issued = append(m.issued, issuedToken{subject, tenantID, mfaDone})
	if m.err != nil {
		return "", m.err
	}
	if m.token == "" {
		return "minted-token", nil
	}
	return m.token, nil
}

func (m *mockTokenIssuer) TTLSeconds() int { return 300 }

type authEvent struct {
	kind        string
	subject     string
	principalID string
	reason      string
}

type mockAuthEvents struct {
	mu     chan struct{}
	events []authEvent
}

func newMockAuthEvents() *mockAuthEvents {
	return &mockAuthEvents{mu: make(chan struct{}, 1)}
}

func (m *mockAuthEvents) record(e authEvent) {
	m.mu <- struct{}{}
	m.events = append(m.events, e)
	<-m.mu
}

func (m *mockAuthEvents) PublishAuthenticationSucceeded(_ context.Context, principalID, _, _ string) error {
	m.record(authEvent{kind: "succeeded", principalID: principalID})
	return nil
}

func (m *mockAuthEvents) PublishAuthenticationFailed(_ context.Context, subject, principalID, _, _, reason string) error {
	m.record(authEvent{kind: "failed", subject: subject, principalID: principalID, reason: reason})
	return nil
}

// ── Fixtures ─────────────────────────────────────────────────────────────────

// errContrived stands in for any infrastructure failure the store can return.
var errContrived = errors.New("contrived store failure")

const (
	testTenantID = "11111111-1111-1111-1111-111111111111"
	testEmail    = "admin@zoikosuite.com"
	testPassword = "Zoiko@Governance1"
	testSubject  = "idp|admin@zoikosuite.com"
)

func testHasher(t *testing.T) *credential.Hasher {
	t.Helper()
	p := credential.DefaultParams()
	p.MemoryKiB = 8192
	p.Iterations = 1
	h, err := credential.NewHasher(p, 2)
	require.NoError(t, err)
	return h
}

type harness struct {
	auth   *identityctx.Authenticator
	store  *mockCredentialStore
	issuer *mockTokenIssuer
	events *mockAuthEvents
	hasher *credential.Hasher
}

func newHarness(t *testing.T, store *mockCredentialStore) *harness {
	t.Helper()
	h := testHasher(t)
	issuer := &mockTokenIssuer{}
	events := newMockAuthEvents()
	// A nil *siem.Client is safe: Stream returns immediately on a nil
	// receiver, which is the same no-op an unset SIEM_SERVICE_URL produces.
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, issuer, events, nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: 15 * time.Minute})
	return &harness{auth: auth, store: store, issuer: issuer, events: events, hasher: h}
}

// activeCredential builds a store holding one principal with a valid digest.
func activeCredential(t *testing.T, h *credential.Hasher, password string) *mockCredentialStore {
	t.Helper()
	hash, err := h.Hash(password)
	require.NoError(t, err)
	return &mockCredentialStore{
		principal: &domain.Principal{
			PrincipalID:             "01J000000000000000PRINCIPAL",
			TenantID:                testTenantID,
			PrincipalType:           domain.PrincipalTypeHuman,
			IdentityProviderSubject: testSubject,
			Email:                   testEmail,
			DisplayName:             "Demo Admin",
			Status:                  domain.PrincipalStatusActive,
		},
		credential: &domain.PrincipalCredential{
			CredentialID:   "01J00000000000000000CRED",
			PrincipalID:    "01J000000000000000PRINCIPAL",
			TenantID:       testTenantID,
			CredentialType: domain.CredentialTypePassword,
			SecretHash:     hash,
			Algorithm:      "argon2id",
			Status:         domain.CredentialStatusActive,
		},
	}
}

func request() domain.AuthenticateRequest {
	return domain.AuthenticateRequest{
		TenantID:      testTenantID,
		Email:         testEmail,
		Password:      testPassword,
		CorrelationID: "corr-1",
	}
}

// ── Tests ────────────────────────────────────────────────────────────────────

func TestAuthenticate_ValidPasswordMintsToken(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	hn := newHarness(t, store)
	// Reuse the hasher the digest was built with.
	hn.auth = identityctx.NewAuthenticator(zap.NewNop(), store, h, hn.issuer, hn.events, nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	resp, err := hn.auth.Authenticate(context.Background(), request())
	require.NoError(t, err)
	require.Equal(t, "minted-token", resp.AccessToken)
	require.Equal(t, "Bearer", resp.TokenType)
	require.Equal(t, 300, resp.ExpiresIn)
	require.Equal(t, "01J000000000000000PRINCIPAL", resp.PrincipalID)
	require.Equal(t, testTenantID, resp.TenantID)

	require.Len(t, hn.store.successCalls, 1, "a successful login must be recorded as evidence")
	require.Empty(t, hn.store.failureCalls)
}

// The token's subject must be the IdP subject, not the principal ID.
// VerifyBearer hands sub straight to FindByIDPSubject, so sending the
// principal ID would resolve to nothing and every login would 401 one step
// later — with a correct password.
func TestAuthenticate_TokenCarriesIdPSubjectNotPrincipalID(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	issuer := &mockTokenIssuer{}
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, issuer, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	_, err := auth.Authenticate(context.Background(), request())
	require.NoError(t, err)

	require.Len(t, issuer.issued, 1)
	require.Equal(t, testSubject, issuer.issued[0].subject)
	require.NotEqual(t, store.principal.PrincipalID, issuer.issued[0].subject)
	require.Equal(t, testTenantID, issuer.issued[0].tenantID)
}

// A password is one factor. Asserting mfa_done would lift the resolved trust
// posture to MFA_VERIFIED on the strength of a password alone, and every
// downstream decision that distinguishes the two would be reading an
// attestation nobody made.
func TestAuthenticate_NeverAssertsMFA(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	issuer := &mockTokenIssuer{}
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, issuer, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	resp, err := auth.Authenticate(context.Background(), request())
	require.NoError(t, err)

	require.False(t, issuer.issued[0].mfaDone, "a password must never assert a second factor")
	require.False(t, resp.MFARequired,
		"no step-up factor exists in the estate, so the response must not demand one a client cannot satisfy")
}

func TestAuthenticate_WrongPasswordRecordsFailure(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	req := request()
	req.Password = "wrong"
	_, err := auth.Authenticate(context.Background(), req)

	require.ErrorIs(t, err, identityctx.ErrInvalidCredentials)
	require.Len(t, store.failureCalls, 1, "the lockout counter must advance")
	require.Empty(t, store.successCalls)
}

// The four rejection reasons must be indistinguishable to the caller. A client
// that can tell them apart can enumerate every account on the platform and
// probe which ones it has already locked.
func TestAuthenticate_AllRejectionsReturnTheSameError(t *testing.T) {
	h := testHasher(t)
	goodHash, err := h.Hash(testPassword)
	require.NoError(t, err)
	locked := time.Now().UTC().Add(10 * time.Minute)

	principal := &domain.Principal{
		PrincipalID:             "01J000000000000000PRINCIPAL",
		TenantID:                testTenantID,
		IdentityProviderSubject: testSubject,
		Status:                  domain.PrincipalStatusActive,
	}

	cases := map[string]struct {
		store    *mockCredentialStore
		password string
	}{
		"no such principal": {
			store:    &mockCredentialStore{lookupErr: domain.ErrPrincipalNotFound},
			password: testPassword,
		},
		"principal without a credential": {
			store:    &mockCredentialStore{principal: principal, lookupErr: domain.ErrCredentialNotFound},
			password: testPassword,
		},
		"account locked": {
			store: &mockCredentialStore{
				principal: principal,
				credential: &domain.PrincipalCredential{
					CredentialID: "c-1", SecretHash: goodHash, LockedUntil: &locked,
				},
			},
			// Correct password, still refused: the lock is checked first.
			password: testPassword,
		},
		"wrong password": {
			store: &mockCredentialStore{
				principal:  principal,
				credential: &domain.PrincipalCredential{CredentialID: "c-1", SecretHash: goodHash},
			},
			password: "not the password",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			auth := identityctx.NewAuthenticator(zap.NewNop(), tc.store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
				identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})
			req := request()
			req.Password = tc.password

			_, err := auth.Authenticate(context.Background(), req)
			require.ErrorIs(t, err, identityctx.ErrInvalidCredentials)
			require.Equal(t, "invalid credentials", err.Error(),
				"the message must not narrate which check failed")
		})
	}
}

// A locked account must not have its lock extended by the locked-out user
// retrying — that would turn a 15-minute lock into a permanent one for anyone
// who keeps trying.
func TestAuthenticate_LockedAccountDoesNotExtendItsOwnLock(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	locked := time.Now().UTC().Add(10 * time.Minute)
	store.credential.LockedUntil = &locked

	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: 15 * time.Minute})

	_, err := auth.Authenticate(context.Background(), request())
	require.ErrorIs(t, err, identityctx.ErrInvalidCredentials)

	require.Empty(t, store.failureCalls, "a locked account must not re-arm its own lock")
	require.Len(t, store.deniedCalls, 1)
	require.Equal(t, "account_locked", store.deniedCalls[0].basis,
		"the real reason belongs in the evidence log even though it is not on the wire")
}

// An expired lock is not a lock. Checking the timestamp rather than a boolean
// is what lets it lapse without a sweeper job.
func TestAuthenticate_ExpiredLockAllowsAuthentication(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	expired := time.Now().UTC().Add(-time.Minute)
	store.credential.LockedUntil = &expired

	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: 15 * time.Minute})

	resp, err := auth.Authenticate(context.Background(), request())
	require.NoError(t, err)
	require.NotEmpty(t, resp.AccessToken)
	require.Len(t, store.successCalls, 1)
}

// A digest we cannot parse is an operational fault, not a wrong password.
// Answering 401 would send the user to a password reset that cannot fix it and
// bury the fault as routine login noise.
func TestAuthenticate_UnusableDigestIsUnavailableNotInvalid(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	store.credential.SecretHash = "$argon2i$v=19$m=8192,t=1,p=1$c2FsdA$a2V5"

	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	_, err := auth.Authenticate(context.Background(), request())
	require.ErrorIs(t, err, identityctx.ErrAuthUnavailable)
	require.NotErrorIs(t, err, identityctx.ErrInvalidCredentials)
	require.Empty(t, store.failureCalls,
		"a broken digest must not advance the lockout counter against the user")
}

// The evidence write and the lockout reset share one transaction. Losing it
// means the login is absent from access_decision_log AND the failure counter
// still holds this principal's earlier misses. Issuing a token anyway would be
// an unlogged login.
func TestAuthenticate_EvidenceWriteFailureRefusesToken(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	store.successErr = errors.New("postgres unreachable")
	issuer := &mockTokenIssuer{}

	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, issuer, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	_, err := auth.Authenticate(context.Background(), request())
	require.ErrorIs(t, err, identityctx.ErrAuthUnavailable)
	require.Empty(t, issuer.issued, "no token may be minted for an unlogged login")
}

func TestAuthenticate_StoreUnreachableFailsClosed(t *testing.T) {
	h := testHasher(t)
	store := &mockCredentialStore{lookupErr: errors.New("connection refused")}

	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	_, err := auth.Authenticate(context.Background(), request())
	require.ErrorIs(t, err, identityctx.ErrAuthUnavailable,
		"an unreachable store is 'cannot determine', never 'wrong password'")
}

// A digest stored under weaker parameters must verify and then be upgraded in
// the same transaction, while the plaintext is still in hand.
func TestAuthenticate_UpgradesWeakDigestOnSuccess(t *testing.T) {
	weakParams := credential.DefaultParams()
	weakParams.MemoryKiB = 8192
	weakParams.Iterations = 1
	weak, err := credential.NewHasher(weakParams, 1)
	require.NoError(t, err)

	strongParams := weakParams
	strongParams.Iterations = 2
	strong, err := credential.NewHasher(strongParams, 1)
	require.NoError(t, err)

	store := activeCredential(t, weak, testPassword)
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, strong, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	_, err = auth.Authenticate(context.Background(), request())
	require.NoError(t, err)

	require.Len(t, store.successCalls, 1)
	newHash := store.successCalls[0].newHash
	require.NotEmpty(t, newHash, "a weak digest must be rehashed on successful login")

	needsRehash, err := strong.Verify(testPassword, newHash)
	require.NoError(t, err, "the upgraded digest must still verify the same password")
	require.False(t, needsRehash, "and must be at the current parameters")
}

func TestAuthenticate_UnchangedDigestIsNotRewritten(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	_, err := auth.Authenticate(context.Background(), request())
	require.NoError(t, err)
	require.Empty(t, store.successCalls[0].newHash,
		"a digest already at current parameters must not be rewritten on every login")
}

func TestAuthenticate_MissingFieldsRejected(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	cases := map[string]domain.AuthenticateRequest{
		"no tenant":   {Email: testEmail, Password: testPassword},
		"no email":    {TenantID: testTenantID, Password: testPassword},
		"no password": {TenantID: testTenantID, Email: testEmail},
		"blank email": {TenantID: testTenantID, Email: "   ", Password: testPassword},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := auth.Authenticate(context.Background(), req)
			require.ErrorIs(t, err, identityctx.ErrAuthRequestInvalid)
			require.NotErrorIs(t, err, identityctx.ErrInvalidCredentials,
				"a malformed request is 400, not a failed login attempt")
		})
	}
	require.Empty(t, store.failureCalls,
		"a malformed request must not consume a lockout attempt")
}

func TestAuthenticate_EmailIsCaseAndSpaceInsensitive(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	req := request()
	req.Email = "  Admin@ZoikoSuite.com  "

	_, err := auth.Authenticate(context.Background(), req)
	require.NoError(t, err, "a typed email must not fail on capitalisation or a trailing space")
}

// An unknown email writes no access_decision_log row — principal_id is NOT
// NULL and carries a foreign key, so there is nothing to point at. The attempt
// must still be observable, because a run of them is what an enumeration sweep
// looks like.
func TestAuthenticate_UnknownEmailIsStillObservable(t *testing.T) {
	h := testHasher(t)
	store := &mockCredentialStore{lookupErr: domain.ErrPrincipalNotFound}
	events := newMockAuthEvents()
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, events, nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	_, err := auth.Authenticate(context.Background(), request())
	require.ErrorIs(t, err, identityctx.ErrInvalidCredentials)

	_ = auth.Drain(context.Background())
	require.Empty(t, store.deniedCalls, "there is no principal row to reference")
	require.Len(t, events.events, 1)
	require.Equal(t, "failed", events.events[0].kind)
	require.Equal(t, "no_such_principal", events.events[0].reason)
	require.Equal(t, testEmail, events.events[0].subject)
}

func TestSetPassword_StoresPHCDigestNotPlaintext(t *testing.T) {
	h := testHasher(t)
	store := &mockCredentialStore{}
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	require.NoError(t, auth.SetPassword(context.Background(), "p-1", testTenantID, testPassword))

	require.Len(t, store.upserts, 1)
	stored := store.upserts[0].newHash
	require.NotEqual(t, testPassword, stored)
	require.NotContains(t, stored, testPassword)
	require.Contains(t, stored, "$argon2id$")

	_, err := h.Verify(testPassword, stored)
	require.NoError(t, err)
}

func TestSetPassword_RejectsEmptyInputs(t *testing.T) {
	h := testHasher(t)
	store := &mockCredentialStore{}
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	require.Error(t, auth.SetPassword(context.Background(), "", testTenantID, testPassword))
	require.Error(t, auth.SetPassword(context.Background(), "p-1", "", testPassword))
	require.Error(t, auth.SetPassword(context.Background(), "p-1", testTenantID, ""))
	require.Empty(t, store.upserts)
}

// The integration seam that the whole change rests on: the token minted by
// the authenticator must be one the EXISTING VerifyBearer accepts, carrying
// the subject FindByIDPSubject looks up. If these two halves do not fit,
// every login succeeds and then 401s at /v1/context/resolve.
func TestIssuedToken_IsAcceptedByTheResolvePath(t *testing.T) {
	cfg := &config.Config{
		JWTSigningSecret:   "local-dev-jwt-signing-secret-key-32-chars-long",
		JWTIssuer:          "identity-context-svc",
		IdPTokenTTLSeconds: 300,
	}
	issuer, err := authpkg.NewIdPTokenIssuer(cfg)
	require.NoError(t, err)

	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	authenticator := identityctx.NewAuthenticator(zap.NewNop(), store, h, issuer, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	resp, err := authenticator.Authenticate(context.Background(), request())
	require.NoError(t, err)

	claims, err := authpkg.NewJWTVerifier(cfg).VerifyBearer(context.Background(), resp.AccessToken)
	require.NoError(t, err, "the resolve path must accept the token authentication mints")
	require.Equal(t, testSubject, claims.Subject,
		"sub must be the IdP subject FindByIDPSubject queries on")
	require.Equal(t, testTenantID, claims.TenantID)
	require.False(t, claims.MFADone)
}
