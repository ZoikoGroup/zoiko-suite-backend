package store_test

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fixtureExec runs a statement the way a test fixture needs to: with the guard triggers off for this transaction only
// (session_replication_role = replica, superuser). Fixtures that back-date or re-status a posted journal do it to build a
// scenario; production code can never do it, which is exactly what the posted-journal guard exists to enforce.
func fixtureExec(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
