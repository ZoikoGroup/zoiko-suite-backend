package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/store"
)

// The OAuth and lifecycle store methods (migration 000014) had no store test
// when the provider and the status command were wired into cmd/server on
// 19 Oct 2026 (Group 1 re-audit F-3, F-5). These run through a NOSUPERUSER
// NOBYPASSRLS role, so the tenant policy and the app.token_lookup hatch are in
// force: a code or refresh token is found by its secret before its tenant is
// known, and only that path may see across tenants.

func authSession(sid, principal, tenant string, now time.Time) domain.AuthSession {
	return domain.AuthSession{SID: sid, TenantID: tenant, PrincipalID: principal, ClientID: "spa", AuthTime: now,
		AMR: []string{domain.AMRPassword}, AssuranceLevel: domain.AssuranceSingleFactor, CreatedAt: now,
		ExpiresAt: now.Add(time.Hour)}
}

func oauthStores(t *testing.T) (context.Context, *pgxpool.Pool, *store.PgStore) {
	t.Helper()
	ctx := context.Background()
	admin := openTestPool(t)
	seedCredential(t, ctx, admin, store.New(admin, zap.NewNop()), "p-1", "t-1", "idp|alice")
	seedCredential(t, ctx, admin, store.New(admin, zap.NewNop()), "p-2", "t-2", "idp|bob")
	return ctx, admin, store.New(ordinaryRolePool(t, admin), zap.NewNop())
}

func TestPgStore_AuthCodeIsSingleUse_Integration(t *testing.T) {
	ctx, _, s := oauthStores(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, s.CreateAuthSession(ctx, authSession("sid-1", "p-1", "t-1", now)))

	code := domain.AuthorizationCode{CodeHash: "code-hash-1", TenantID: "t-1", ClientID: "spa", RedirectURI: "https://app/cb",
		SID: "sid-1", PrincipalID: "p-1", Scope: "openid", CodeChallenge: "c", CodeChallengeMethod: "S256",
		ExpiresAt: now.Add(time.Minute)}
	require.NoError(t, s.InsertAuthCode(ctx, code))

	got, err := s.ConsumeAuthCode(ctx, "code-hash-1", now)
	require.NoError(t, err, "found by its hash with no tenant known (token lookup hatch)")
	require.Equal(t, "sid-1", got.SID)

	replay, err := s.ConsumeAuthCode(ctx, "code-hash-1", now)
	require.ErrorIs(t, err, domain.ErrCodeReplayed)
	require.NotNil(t, replay, "a replay returns the row, so the caller can end its login session")
	require.Equal(t, "t-1", replay.TenantID)

	_, err = s.ConsumeAuthCode(ctx, "unknown", now)
	require.ErrorIs(t, err, domain.ErrCodeInvalid)

	code.CodeHash, code.ExpiresAt = "code-hash-2", now.Add(-time.Second)
	require.NoError(t, s.InsertAuthCode(ctx, code))
	_, err = s.ConsumeAuthCode(ctx, "code-hash-2", now)
	require.ErrorIs(t, err, domain.ErrCodeInvalid, "an expired code")
}

func TestPgStore_RefreshRotationAndReuse_Integration(t *testing.T) {
	ctx, _, s := oauthStores(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, s.CreateAuthSession(ctx, authSession("sid-2", "p-1", "t-1", now)))
	first := domain.RefreshToken{TokenHash: "rt-1", FamilyID: "fam-1", TenantID: "t-1", PrincipalID: "p-1", ClientID: "spa",
		SID: "sid-2", Scope: "openid offline_access", IssuedAt: now, ExpiresAt: now.Add(time.Hour), FamilyExpiresAt: now.Add(8 * time.Hour)}
	require.NoError(t, s.InsertRefreshToken(ctx, first))

	next := func(t *domain.RefreshToken) domain.RefreshToken {
		n := *t
		n.TokenHash, n.IssuedAt, n.UsedAt, n.RevokedAt = "rt-2", now, nil, nil
		return n
	}
	old, err := s.RotateRefreshToken(ctx, "rt-1", now, nil, next)
	require.NoError(t, err)
	require.Equal(t, "fam-1", old.FamilyID)

	_, err = s.RotateRefreshToken(ctx, "rt-1", now, nil, next)
	require.ErrorIs(t, err, domain.ErrRefreshReused)

	sess, err := s.GetAuthSession(ctx, "t-1", "sid-2")
	require.NoError(t, err)
	require.NotNil(t, sess.EndedAt, "reuse ends the login session")
	require.Equal(t, domain.SessionEndRefreshReuse, sess.EndReason)

	_, err = s.RotateRefreshToken(ctx, "rt-2", now, nil, next)
	require.ErrorIs(t, err, domain.ErrRefreshInvalid, "the whole family is revoked")
}

func TestPgStore_AuthSessionsAreTenantScoped_Integration(t *testing.T) {
	ctx, _, s := oauthStores(t)
	now := time.Now().UTC()
	require.NoError(t, s.CreateAuthSession(ctx, authSession("sid-3", "p-1", "t-1", now)))

	other, err := s.GetAuthSession(ctx, "t-2", "sid-3")
	require.NoError(t, err)
	require.Nil(t, other, "another tenant cannot read the session")

	live, err := s.LiveAuthSessionIDs(ctx, "t-1", "p-1", now)
	require.NoError(t, err)
	require.Contains(t, live, "sid-3")

	_, err = s.EndAuthSession(ctx, "t-1", "sid-3", domain.SessionEndLogout, now)
	require.NoError(t, err)
	live, err = s.LiveAuthSessionIDs(ctx, "t-1", "p-1", now)
	require.NoError(t, err)
	require.NotContains(t, live, "sid-3")
}

// F-3: a status change writes the row, its decision-log entry and its events
// in one transaction; repeating it writes nothing.
func TestPgStore_SetPrincipalLifecycleWritesItsEvents_Integration(t *testing.T) {
	ctx, admin, s := oauthStores(t)
	payload := map[string]any{"principal_id": "p-1", "tenant_id": "t-1", "new_status": "DISABLED"}
	var recs []outbox.Record
	for _, et := range []string{events.EventPrincipalStatusChanged, events.EventSubjectDisabled} {
		rec, err := events.Render(et, "t-1", "", "admin-1", "corr-dis", "p-1", payload)
		require.NoError(t, err)
		recs = append(recs, rec)
	}

	changed, err := s.SetPrincipalLifecycle(ctx, "t-1", "p-1", domain.PrincipalStatusDisabled, "admin-1", "left the company", "corr-dis", recs...)
	require.NoError(t, err)
	require.True(t, changed)

	p, err := s.FindByID(ctx, "p-1", "t-1")
	require.NoError(t, err)
	require.Equal(t, domain.PrincipalStatusDisabled, p.Status)

	var n int
	require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE payload->>'correlation_id' = 'corr-dis'
		AND event_type IN ('principal.status.changed', 'identity.subject.disabled')`).Scan(&n))
	require.Equal(t, 2, n, "both event names are enqueued with the change")

	changed, err = s.SetPrincipalLifecycle(ctx, "t-1", "p-1", domain.PrincipalStatusDisabled, "admin-1", "again", "corr-dis-2", recs...)
	require.NoError(t, err)
	require.False(t, changed, "re-applying the same status changes nothing")

	// Another tenant cannot change the principal.
	changed, err = s.SetPrincipalLifecycle(ctx, "t-2", "p-1", domain.PrincipalStatusActive, "mallory", "x", "corr-x")
	if err != nil && !errors.Is(err, context.Canceled) {
		require.NoError(t, err)
	}
	require.False(t, changed)
}
