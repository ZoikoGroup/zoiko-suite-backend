package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/store"
)

// Migration 000010 against a real Postgres: claim, replay, mismatch, release
// and purge, all through FORCE row-level security.
func TestIdempotencyKeys_ClaimReplayMismatchRelease(t *testing.T) {
	f := newORGFixture(t)
	endpoint := "POST /v1/tenants/" + f.tenantID + "/commands/SuspendTenant"
	key := uuid.New().String()

	rec, err := f.s.ClaimIdempotencyKey(f.ctx, f.tenantID, endpoint, key, "fp-a")
	require.NoError(t, err)
	require.Nil(t, rec, "a first claim executes")

	rec, err = f.s.ClaimIdempotencyKey(f.ctx, f.tenantID, endpoint, key, "fp-a")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, 0, rec.ResponseStatus, "claimed but unanswered reads as in flight")

	require.NoError(t, f.s.CompleteIdempotencyKey(f.ctx, f.tenantID, endpoint, key, 200, []byte(`{"record_version":2}`)))
	rec, err = f.s.ClaimIdempotencyKey(f.ctx, f.tenantID, endpoint, key, "fp-a")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, 200, rec.ResponseStatus)
	assert.JSONEq(t, `{"record_version":2}`, string(rec.ResponseBody))

	_, err = f.s.ClaimIdempotencyKey(f.ctx, f.tenantID, endpoint, key, "fp-b")
	assert.ErrorIs(t, err, store.ErrIdempotencyFingerprintMismatch)

	// A non-JSON body (chi's text/plain 404) must not fail the jsonb write.
	key2 := uuid.New().String()
	_, err = f.s.ClaimIdempotencyKey(f.ctx, f.tenantID, endpoint, key2, "fp-a")
	require.NoError(t, err)
	require.NoError(t, f.s.CompleteIdempotencyKey(f.ctx, f.tenantID, endpoint, key2, 404, []byte("404 page not found\n")))

	// Release drops only an unanswered claim.
	key3 := uuid.New().String()
	_, err = f.s.ClaimIdempotencyKey(f.ctx, f.tenantID, endpoint, key3, "fp-a")
	require.NoError(t, err)
	require.NoError(t, f.s.ReleaseIdempotencyKey(f.ctx, f.tenantID, endpoint, key3))
	rec, err = f.s.ClaimIdempotencyKey(f.ctx, f.tenantID, endpoint, key3, "fp-a")
	require.NoError(t, err)
	assert.Nil(t, rec, "a released key can be claimed afresh")
}

func TestIdempotencyKeys_AreTenantIsolatedAndPurgeable(t *testing.T) {
	a := newORGFixture(t)
	b := newORGFixture(t)
	endpoint, key := "POST /v1/entities", uuid.New().String()

	_, err := a.s.ClaimIdempotencyKey(a.ctx, a.tenantID, endpoint, key, "fp-a")
	require.NoError(t, err)

	// Tenant B using the same key and endpoint is a different request, not a
	// replay of A's.
	rec, err := b.s.ClaimIdempotencyKey(b.ctx, b.tenantID, endpoint, key, "fp-other")
	require.NoError(t, err)
	assert.Nil(t, rec)

	n, err := a.s.PurgeIdempotencyKeysBefore(a.ctx, time.Now().UTC().Add(time.Minute))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(2), "the purge sees every tenant's rows")
}
