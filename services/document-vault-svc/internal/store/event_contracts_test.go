package store_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/eventing/contracttest"
)

// verifyEventContracts fails the calling test if any event it left in the
// outbox does not satisfy the schema this service publishes (ZS-EVENT-001 12.1,
// EV-T22). Call it right after registering the pool's Close cleanup: cleanups
// run last-in first-out, so the check sees the rows before the pool closes.
func verifyEventContracts(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "contracts", "events")
	t.Cleanup(func() { contracttest.Verify(t, pool, dir) })
}

// eventingCount counts events of one canonical type about one document in the
// shared eventing outbox. The document id is read from the event's data, because
// classification events are keyed by the classification, not the document.
func eventingCount(t *testing.T, pool *pgxpool.Pool, eventType, documentID string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM eventing_outbox
		 WHERE event_type = $1 AND convert_from(payload, 'UTF8')::jsonb->'data'->>'document_id' = $2`,
		eventType, documentID).Scan(&n))
	return n
}
