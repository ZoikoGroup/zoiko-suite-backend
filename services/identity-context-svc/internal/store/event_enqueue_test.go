package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/outbox"
)

// A failed resolution has no tenant. It must still be recorded (the events an
// investigation needs), under the platform tenant, with the legacy name intact.
func TestOutboxStore_TenantlessEventIsRecordedUnderThePlatformTenant(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	st := outbox.NewStore(pool, zap.NewNop())

	rec, err := events.Render(events.EventResolutionFailed, "", "", "", "corr-nt", "subject-x",
		map[string]any{"principal_id_or_subject": "subject-x", "failure_reason": "unknown"})
	require.NoError(t, err)
	require.NoError(t, st.Enqueue(ctx, rec))

	var tenant, typ, legacy string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT tenant_id, event_type, convert_from(payload,'UTF8')::jsonb->>'event_type'
		  FROM eventing_outbox WHERE convert_from(payload,'UTF8')::jsonb->>'correlation_id' = 'corr-nt'`).
		Scan(&tenant, &typ, &legacy))
	assert.Equal(t, outbox.PlatformTenant, tenant)
	assert.Equal(t, "com.zoikosuite.identity.context.resolution-failed", typ)
	assert.Equal(t, events.EventResolutionFailed, legacy, "existing consumers filter on the legacy name")
}

func TestOutboxStore_TenantedEventIsScopedToItsTenant(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	st := outbox.NewStore(pool, zap.NewNop())

	rec, err := events.Render(events.EventAuthenticationSucceeded, "t-9", "", "p-9", "corr-t9", "p-9",
		map[string]any{"principal_id": "p-9"})
	require.NoError(t, err)
	require.NoError(t, st.Enqueue(ctx, rec))

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM eventing_outbox WHERE tenant_id='t-9' AND event_type='com.zoikosuite.identity.authentication.succeeded'`).Scan(&n))
	assert.Equal(t, 1, n)
}
