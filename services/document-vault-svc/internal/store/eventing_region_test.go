package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/store"
)

// A write whose eventing-outbox event cannot be built must leave neither state nor
// event. With no event region configured the envelope is refused; creating a
// document proposes a classification, so the whole create must roll back with it.
func TestPgStore_NoEventRegion_RollsBackTheWrite(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop()) // deliberately no WithEventRegion

	clearOutbox(t, pool)
	doc, v := newDoc("no-region")
	require.Error(t, s.CreateDocument(tenantCtx(), doc, v, "corr-no-region"))

	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM documents WHERE title = 'no-region'`).Scan(&n))
	require.Zero(t, n, "the document insert must roll back when its classification event cannot be built")
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM eventing_outbox`).Scan(&n))
	require.Zero(t, n, "no event may be written for a rolled-back change")
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&n))
	require.Zero(t, n, "the older outbox must not hold events for a rolled-back change either")
}
