package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

// A store with no event region cannot build the envelope, so the template write is refused
// and rolled back: no template row, and no event in either outbox.
func TestTemplateEvents_MissingRegionRollsTheWriteBack(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool) // no WithEventRegion
	ctx := tenantCtx("tenant-region")

	_, err := s.CreateTemplate(ctx, domain.CreateTemplateParams{
		LegalEntityID: "le-us", Name: "Reset", BusinessPurpose: "x", OwnerPrincipalID: "owner-1",
	})
	require.Error(t, err)

	var templates, events int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM template_definitions`).Scan(&templates))
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM eventing_outbox`).Scan(&events))
	assert.Zero(t, templates, "the template must not exist when its event could not be written")
	assert.Zero(t, events)
}

// The template events no longer touch the service's own outbox, and the envelope keeps the
// old event type as its legacy type.
func TestTemplateEvents_LeaveTheOldOutboxAndKeepTheLegacyType(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("local"))
	ctx := tenantCtx("tenant-legacy")
	tmpl := newTestTemplate(t, s, ctx, "owner-1")

	var old int
	tx, err := pool.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(context.Background(), "SELECT set_config('app.outbox_relay', 'true', true)")
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(context.Background(), `SELECT COUNT(*) FROM event_outbox WHERE event_type LIKE 'template.%'`).Scan(&old))
	assert.Zero(t, old)

	var legacy, region string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT convert_from(payload, 'UTF8')::jsonb->>'event_type', region FROM eventing_outbox WHERE aggregate_id = $1`, tmpl.TemplateID).Scan(&legacy, &region))
	assert.Equal(t, "template.created", legacy)
	assert.Equal(t, "local", region)
}
