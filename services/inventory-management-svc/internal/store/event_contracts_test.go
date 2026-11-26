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

// verifyEventContracts fails the calling test if any event it left in the shared eventing
// outbox does not satisfy the schema this service publishes (ZS-EVENT-001 12.1, EV-T22).
// Register it after the pool's Close cleanup: cleanups run last-in first-out, so the
// check sees the rows before the pool closes.
func verifyEventContracts(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "contracts", "events")
	t.Cleanup(func() { contracttest.Verify(t, pool, dir) })
}

// eventingTypes lists the canonical types written to eventing_outbox about one aggregate, oldest first.
func eventingTypes(t *testing.T, pool *pgxpool.Pool, aggregateID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT event_type FROM eventing_outbox WHERE aggregate_id = $1 ORDER BY created_at, outbox_id`, aggregateID)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}
