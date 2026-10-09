package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/domain"
	"zoiko.io/payment-authorization-svc/internal/middleware"
	"zoiko.io/payment-authorization-svc/internal/store"
)

type fixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	s      *store.PgStore
	ctx    context.Context
	tenant string
	le     string
}

func newFixture(t *testing.T) *fixture {
	pool := openTestPool(t)
	tenant := uuid.New().String()
	return &fixture{t: t, pool: pool, s: store.NewPgStore(pool, zap.NewNop()), ctx: middleware.WithTenant(context.Background(), tenant), tenant: tenant, le: uuid.New().String()}
}

func (f *fixture) request(expiresAt *time.Time) *domain.PaymentAuthorization {
	a, err := f.s.RequestAuthorization(f.ctx, f.tenant, domain.PaymentAuthorization{
		LegalEntityID: f.le, ProposalID: uuid.New().String(), ProposalFingerprint: "sha256:fp", NetAmount: 100,
		Currency: "USD", RequestedByPrincipalID: "maker", ExpiresAt: expiresAt,
	}, nil)
	require.NoError(f.t, err)
	return a
}

// TestPgStore_Quorum_DistinctSignersApproveOnlyAtThreshold: the signature
// unique index and the row lock make quorum a database fact.
func TestPgStore_Quorum_DistinctSignersApproveOnlyAtThreshold(t *testing.T) {
	f := newFixture(t)
	a := f.request(nil)
	require.Equal(t, 1, a.Version)
	require.Equal(t, 1, a.RequiredSignatures)

	one, err := f.s.ApproveAuthorization(f.ctx, a.AuthorizationID, "APPROVAL_REQUIRED", "pv1", "signer-1", 2, nil)
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, one.Status)
	require.Equal(t, 1, one.SignatureCount)
	require.Equal(t, 2, one.RequiredSignatures)
	require.Greater(t, one.Version, a.Version, "the version trigger must bump on every update")

	// the same signer again: refused, and the failed insert must not poison anything
	_, err = f.s.ApproveAuthorization(f.ctx, a.AuthorizationID, "APPROVAL_REQUIRED", "pv1", "signer-1", 2, nil)
	require.ErrorIs(t, err, domain.ErrAlreadySigned)

	two, err := f.s.ApproveAuthorization(f.ctx, a.AuthorizationID, "APPROVAL_REQUIRED", "pv1", "signer-2", 2, nil)
	require.NoError(t, err)
	require.Equal(t, domain.StatusApproved, two.Status)
	require.Equal(t, 2, two.SignatureCount)
	require.NotNil(t, two.ApprovedByPrincipalID)
	require.Equal(t, "signer-2", *two.ApprovedByPrincipalID)

	sigs, err := f.s.ListSignatures(f.ctx, a.AuthorizationID)
	require.NoError(t, err)
	require.Len(t, sigs, 2)

	// approved is no longer PENDING
	_, err = f.s.ApproveAuthorization(f.ctx, a.AuthorizationID, "APPROVAL_REQUIRED", "pv1", "signer-3", 2, nil)
	require.ErrorIs(t, err, domain.ErrInvalidTransition)
}

// The required count can be raised but never lowered by a later signer.
func TestPgStore_Quorum_RequiredCountNeverDecreases(t *testing.T) {
	f := newFixture(t)
	a := f.request(nil)
	_, err := f.s.ApproveAuthorization(f.ctx, a.AuthorizationID, "APPROVAL_REQUIRED", "pv1", "s1", 3, nil)
	require.NoError(t, err)
	got, err := f.s.ApproveAuthorization(f.ctx, a.AuthorizationID, "WITHIN_THRESHOLD", "pv1", "s2", 1, nil)
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, got.Status, "a lower requirement from a later call must not shortcut the quorum")
	require.Equal(t, 3, got.RequiredSignatures)
}

func TestPgStore_Approve_StaleVersionAndExpiry(t *testing.T) {
	f := newFixture(t)
	a := f.request(nil)
	stale := a.Version + 3
	_, err := f.s.ApproveAuthorization(f.ctx, a.AuthorizationID, "", "", "s1", 1, &stale)
	require.ErrorIs(t, err, domain.ErrStaleVersion)

	past := time.Now().Add(-time.Minute)
	expired := f.request(&past)
	_, err = f.s.ApproveAuthorization(f.ctx, expired.AuthorizationID, "", "", "s1", 1, nil)
	require.ErrorIs(t, err, domain.ErrAuthorizationExpired)
	sigs, _ := f.s.ListSignatures(f.ctx, expired.AuthorizationID)
	require.Empty(t, sigs, "a refused (expired) approval must leave no signature behind")
}

// TestPgStore_ExpiryCandidates_AndSweepPublishesEvent: only overdue
// PENDING/APPROVED rows with an expires_at are candidates; expiring one writes
// the Expired outbox event.
func TestPgStore_ExpiryCandidates_AndSweepPublishesEvent(t *testing.T) {
	f := newFixture(t)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	overdue := f.request(&past)
	live := f.request(&future)
	never := f.request(nil)

	refs, err := f.s.ListExpiredCandidates(context.Background(), 100)
	require.NoError(t, err)
	ids := map[string]string{}
	for _, r := range refs {
		ids[r.AuthorizationID] = r.TenantID
	}
	require.Contains(t, ids, overdue.AuthorizationID)
	require.Equal(t, f.tenant, ids[overdue.AuthorizationID])
	require.NotContains(t, ids, live.AuthorizationID)
	require.NotContains(t, ids, never.AuthorizationID)

	// expire through the tenant-scoped path the sweeper uses
	_, err = f.s.ExpireAuthorization(middleware.WithTenant(context.Background(), ids[overdue.AuthorizationID]), overdue.AuthorizationID, "system:expiry-sweeper")
	require.NoError(t, err)

	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`,
		overdue.AuthorizationID, domain.EventAuthorizationExpired).Scan(&n))
	require.Equal(t, 1, n)

	refs, err = f.s.ListExpiredCandidates(context.Background(), 100)
	require.NoError(t, err)
	for _, r := range refs {
		require.NotEqual(t, overdue.AuthorizationID, r.AuthorizationID, "an expired authorization is no longer a candidate")
	}
}

// TestPgStore_Idempotency covers claim, replay, in-progress, release and
// the tenant scoping of keys.
func TestPgStore_Idempotency(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx

	rec, created, err := f.s.BeginIdempotent(ctx, "approve:x", "k1", "hash-1")
	require.NoError(t, err)
	require.True(t, created)
	require.Nil(t, rec)

	// same key while still running -> existing, not completed
	rec, created, err = f.s.BeginIdempotent(ctx, "approve:x", "k1", "hash-1")
	require.NoError(t, err)
	require.False(t, created)
	require.False(t, rec.Completed)
	require.Equal(t, "hash-1", rec.RequestHash)

	require.NoError(t, f.s.CompleteIdempotent(ctx, "approve:x", "k1", 200, []byte(`{"ok":true}`)))
	rec, created, err = f.s.BeginIdempotent(ctx, "approve:x", "k1", "hash-1")
	require.NoError(t, err)
	require.False(t, created)
	require.True(t, rec.Completed)
	require.Equal(t, 200, rec.StatusCode)
	require.JSONEq(t, `{"ok":true}`, string(rec.Body))

	// a completed record is never overwritten by a second Complete
	require.NoError(t, f.s.CompleteIdempotent(ctx, "approve:x", "k1", 500, []byte(`{"ok":false}`)))
	rec, _, _ = f.s.BeginIdempotent(ctx, "approve:x", "k1", "hash-1")
	require.Equal(t, 200, rec.StatusCode)

	// release only frees an unfinished claim
	_, created, _ = f.s.BeginIdempotent(ctx, "approve:y", "k2", "h")
	require.True(t, created)
	require.NoError(t, f.s.ReleaseIdempotent(ctx, "approve:y", "k2"))
	_, created, _ = f.s.BeginIdempotent(ctx, "approve:y", "k2", "h")
	require.True(t, created, "a released key can be claimed again")
	require.NoError(t, f.s.ReleaseIdempotent(ctx, "approve:x", "k1")) // completed: untouched
	rec, created, _ = f.s.BeginIdempotent(ctx, "approve:x", "k1", "hash-1")
	require.False(t, created)
	require.True(t, rec.Completed)

	// keys are per tenant
	other := middleware.WithTenant(context.Background(), uuid.New().String())
	_, created, err = f.s.BeginIdempotent(other, "approve:x", "k1", "hash-1")
	require.NoError(t, err)
	require.True(t, created, "the same key in another tenant is a different key")

	// purge removes old rows
	purged, err := f.s.PurgeIdempotency(ctx, 0)
	require.NoError(t, err)
	require.GreaterOrEqual(t, purged, int64(1))
}
