package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/store"
)

func passkeyFor(id, principalID, tenantID string) domain.WebAuthnCredential {
	return domain.WebAuthnCredential{CredentialID: id, TenantID: tenantID, PrincipalID: principalID,
		PublicKey: []byte{0xa5, 0x01, 0x02}, AttestationType: "none", AAGUID: "00000000-0000-0000-0000-000000000001",
		SignCount: 0, Transports: []string{"internal"}, BackupEligible: true, UserVerified: true, Nickname: "laptop"}
}

func TestAU11_PasskeyRegistrationIsAssuranceHistory(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "alice", "ACTIVE")

	rec, err := events.Render(events.EventMFAFactorEnrolled, "t-1", "", "p-1", "corr-1", "p-1", map[string]any{"factor_type": "WEBAUTHN"})
	require.NoError(t, err)
	require.NoError(t, s.InsertWebAuthnCredential(ctx, passkeyFor("cred-1", "p-1", "t-1"), "corr-1", rec))

	creds, err := s.ListWebAuthnCredentials(ctx, "t-1", "p-1")
	require.NoError(t, err)
	require.Len(t, creds, 1)
	assert.Equal(t, []string{"internal"}, creds[0].Transports)
	assert.True(t, creds[0].BackupEligible)
	assert.Equal(t, "laptop", creds[0].Nickname)
	has, err := s.HasActiveWebAuthn(ctx, "t-1", "p-1")
	require.NoError(t, err)
	assert.True(t, has)

	hist, err := s.ListAssuranceHistoryPage(ctx, "t-1", "p-1", 0, 10)
	require.NoError(t, err)
	require.Len(t, hist, 1)
	assert.Equal(t, "PASSKEY", hist[0].AssuranceLevel)

	var outboxed int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM event_outbox WHERE event_id = $1`, rec.EventID).Scan(&outboxed))
	assert.Equal(t, 1, outboxed, "the enrolment event commits with the credential")

	sum, err := s.GetIdentitySummary(ctx, "t-1")
	require.NoError(t, err)
	assert.Equal(t, 1, sum.ActiveHumansWithMFA, "a passkey counts towards MFA coverage")

	err = s.InsertWebAuthnCredential(ctx, passkeyFor("cred-1", "p-1", "t-1"), "corr-2")
	require.Error(t, err, "a credential id registers once")
}

func TestAU11_PasskeyAssertionIsASignInEvent(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "alice", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-2", "t-1", "bob", "ACTIVE")
	require.NoError(t, s.InsertWebAuthnCredential(ctx, passkeyFor("cred-1", "p-1", "t-1"), "corr-1"))

	require.NoError(t, s.UseWebAuthnCredential(ctx, "t-1", "p-1", "cred-1", 7, true, "MFA_LOGIN", "corr-2"))
	creds, err := s.ListWebAuthnCredentials(ctx, "t-1", "p-1")
	require.NoError(t, err)
	require.Len(t, creds, 1)
	assert.EqualValues(t, 7, creds[0].SignCount)
	assert.True(t, creds[0].BackupState)
	assert.NotNil(t, creds[0].LastUsedAt)

	evs, err := s.ListAuthenticationEventsPage(ctx, "t-1", store.AuthEventFilter{PrincipalID: "p-1"}, 0, 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "MFA_CHALLENGE", evs[0].EventType)
	assert.Equal(t, "PASSKEY", evs[0].AssuranceLevel)
	assert.Equal(t, "SUCCESS", evs[0].Outcome)

	// Another principal cannot assert with p-1's credential, even in-tenant.
	err = s.UseWebAuthnCredential(ctx, "t-1", "p-2", "cred-1", 8, false, "MFA_STEP_UP", "corr-3")
	assert.True(t, errors.Is(err, domain.ErrCredentialNotFound), "got %v", err)
}

func TestAU11_RetiredPasskeyDropsAssuranceAndCannotAssert(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "alice", "ACTIVE")
	require.NoError(t, s.InsertWebAuthnCredential(ctx, passkeyFor("cred-1", "p-1", "t-1"), "corr-1"))
	require.NoError(t, s.InsertWebAuthnCredential(ctx, passkeyFor("cred-2", "p-1", "t-1"), "corr-2"))

	n, err := s.RetireWebAuthnCredential(ctx, "t-1", "p-1", "", "admin_reset:LOST_DEVICE", "MFA_RESET", "corr-3")
	require.NoError(t, err)
	assert.EqualValues(t, 2, n, "an empty credential id retires them all")
	has, err := s.HasActiveWebAuthn(ctx, "t-1", "p-1")
	require.NoError(t, err)
	assert.False(t, has)

	hist, err := s.ListAssuranceHistoryPage(ctx, "t-1", "p-1", 0, 10)
	require.NoError(t, err)
	require.Len(t, hist, 3)
	assert.Equal(t, "PASSWORD", hist[0].AssuranceLevel, "newest first: nothing stronger than the password is left")

	err = s.UseWebAuthnCredential(ctx, "t-1", "p-1", "cred-1", 9, false, "MFA_STEP_UP", "corr-4")
	assert.True(t, errors.Is(err, domain.ErrCredentialNotFound), "a retired passkey cannot assert: %v", err)
	n, err = s.RetireWebAuthnCredential(ctx, "t-1", "p-1", "cred-1", "owner_revoked", "MFA_REVOKE", "corr-5")
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestIC2_PasswordResetsPage(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "p-1", "t-1", "alice", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-2", "t-1", "bob", "ACTIVE")
	insertPrincipal(t, ctx, pool, "admin", "t-1", "admin", "ACTIVE")
	now := time.Now().UTC()
	for _, r := range []domain.PasswordResetRequest{
		{RequestID: "r-1", PrincipalID: "p-1", ExpiresAt: now.Add(time.Hour), CreatedAt: now.Add(-3 * time.Minute)},
		{RequestID: "r-2", PrincipalID: "p-2", ExpiresAt: now.Add(time.Hour), CreatedAt: now.Add(-2 * time.Minute)},
		{RequestID: "r-3", PrincipalID: "p-1", ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Minute)},
	} {
		r.TenantID, r.RequestedBy, r.Reason, r.ApprovalsRequired, r.Status = "t-1", "admin", "lost access", 1, "PENDING_APPROVAL"
		require.NoError(t, s.CreateResetRequest(ctx, r))
	}

	all, err := s.ListPasswordResetsPage(ctx, "t-1", store.ResetListFilter{}, 0, 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, "r-3", all[0].RequestID, "newest first")
	assert.Equal(t, "EXPIRED", all[0].Status, "a pending request past its expiry reads EXPIRED")
	assert.Equal(t, "admin", all[0].RequestedBy)
	assert.NotNil(t, all[0].Approvals)

	pending, err := s.ListPasswordResetsPage(ctx, "t-1", store.ResetListFilter{Status: "PENDING_APPROVAL"}, 0, 10)
	require.NoError(t, err)
	assert.Len(t, pending, 2)
	expired, err := s.ListPasswordResetsPage(ctx, "t-1", store.ResetListFilter{Status: "EXPIRED"}, 0, 10)
	require.NoError(t, err)
	assert.Len(t, expired, 1)
	alices, err := s.ListPasswordResetsPage(ctx, "t-1", store.ResetListFilter{PrincipalID: "p-1"}, 0, 10)
	require.NoError(t, err)
	assert.Len(t, alices, 2)
	page, err := s.ListPasswordResetsPage(ctx, "t-1", store.ResetListFilter{}, 2, 10)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "r-1", page[0].RequestID)

	_, err = s.ListPasswordResetsPage(ctx, "t-1", store.ResetListFilter{Status: "MAYBE"}, 0, 10)
	assert.True(t, errors.Is(err, store.ErrInvalidFilter))
}

// The app role (NOBYPASSRLS) never sees another tenant's resets or passkeys.
func TestRLS_ResetsAndPasskeysNeverCrossTenant(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	owner := store.New(pool, zap.NewNop())
	insertPrincipal(t, ctx, pool, "pb", "t-b", "bob", "ACTIVE")
	insertPrincipal(t, ctx, pool, "ab", "t-b", "admin-b", "ACTIVE")
	require.NoError(t, owner.InsertWebAuthnCredential(ctx, passkeyFor("cred-b", "pb", "t-b"), "c"))
	require.NoError(t, owner.CreateResetRequest(ctx, domain.PasswordResetRequest{RequestID: "r-b", TenantID: "t-b",
		PrincipalID: "pb", RequestedBy: "ab", Reason: "x", ApprovalsRequired: 1, Status: "PENDING_APPROVAL",
		ExpiresAt: time.Now().Add(time.Hour)}))

	for _, stmt := range []string{
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ic_rls_probe') THEN
		     CREATE ROLE ic_rls_probe NOLOGIN NOSUPERUSER NOBYPASSRLS; END IF; END $$`,
		`GRANT USAGE ON SCHEMA public TO ic_rls_probe`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA public TO ic_rls_probe`,
	} {
		_, err := pool.Exec(ctx, stmt)
		require.NoError(t, err)
	}
	cfg := pool.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE ic_rls_probe")
		return err
	}
	probePool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(probePool.Close)
	probe := store.New(probePool, zap.NewNop())

	rs, err := probe.ListPasswordResetsPage(ctx, "t-a", store.ResetListFilter{PrincipalID: "pb"}, 0, 50)
	require.NoError(t, err)
	assert.Empty(t, rs, "tenant A naming tenant B's principal sees no reset")
	ks, err := probe.ListWebAuthnCredentials(ctx, "t-a", "pb")
	require.NoError(t, err)
	assert.Empty(t, ks, "nor its passkeys")
	var leaked int
	require.NoError(t, probePool.QueryRow(ctx,
		`SELECT (SELECT COUNT(*) FROM password_reset_requests) + (SELECT COUNT(*) FROM webauthn_credentials)`).Scan(&leaked))
	assert.Zero(t, leaked, "with no tenant set, the app role sees nothing")
}
