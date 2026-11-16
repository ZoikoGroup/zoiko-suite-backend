package store_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

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
