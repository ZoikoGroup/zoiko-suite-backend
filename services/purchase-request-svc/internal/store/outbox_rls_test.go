package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	svcmiddleware "zoiko.io/purchase-request-svc/internal/middleware"
	"zoiko.io/purchase-request-svc/internal/outbox"
)

// appPool connects as the ordinary NOSUPERUSER NOBYPASSRLS role (the role the
// service runs as), so the outbox_events policy (000008_outbox_rls) binds it.
// A superuser bypasses RLS entirely, which is why the relay test above cannot
// prove this.
func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_APP_DATABASE_URL not set: needs an ordinary role for RLS")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(p.Close)
	return p
}

// Under FORCE RLS the relay, running as the service's own role, still drains
// every tenant's backlog because it declares app.outbox_relay; an ordinary
// session with no tenant and no relay flag sees no outbox row at all.
func TestOutboxRLS_RelayDrainsAllTenants_UnscopedSessionSeesNothing(t *testing.T) {
	f := setup(t)
	app := appPool(t)
	f.create(t) // tenant A
	other := uuid.NewString()
	_, err := f.s.CreateRequest(svcmiddleware.WithTenant(context.Background(), other), newReq(other)) // tenant B
	require.NoError(t, err)

	var visible int
	require.NoError(t, app.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&visible))
	assert.Zero(t, visible, "no tenant and no relay flag: RLS must hide every outbox row")

	pub := &fakePub{}
	relay := outbox.NewRelay(app, pub, time.Second, 50, zap.NewNop())
	assert.Equal(t, 4, relay.RelayOnce(context.Background()), "relay delivers both tenants' events (2 each)")
	assert.Zero(t, relay.RelayOnce(context.Background()), "published rows are not re-sent")
}

// A request-path insert for another tenant is refused by WITH CHECK.
func TestOutboxRLS_CrossTenantInsertRefused(t *testing.T) {
	f := setup(t)
	app := appPool(t)
	tx, err := app.Begin(context.Background())
	require.NoError(t, err)
	defer tx.Rollback(context.Background()) //nolint:errcheck
	_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true)`, f.tid)
	require.NoError(t, err)
	_, err = outbox.Insert(context.Background(), tx, outbox.Event{
		AggregateType: "x", AggregateID: uuid.NewString(), EventType: "Forged",
		TenantID: uuid.NewString(), LegalEntityID: uuid.NewString(), Payload: map[string]string{},
	})
	require.Error(t, err, "inserting an outbox row for another tenant must be refused")
	assert.Contains(t, err.Error(), "row-level security")
}
