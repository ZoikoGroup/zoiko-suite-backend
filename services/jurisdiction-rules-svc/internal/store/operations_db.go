package store

// The database handle the shared API-5 operation store
// (identity-envelope/apicontract/asyncpgx) runs on. async_operations has tenant
// row-level security (000036), so every statement must run as the request's
// tenant: inside a unit of work it joins the unit (whose transaction carries the
// tenant); outside one, each statement runs in its own short transaction scoped
// to the tenant in ctx (WithTenant).

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// OperationsDB is asyncpgx.DB (plus Begin) bound to this store's scoping.
type OperationsDB struct{ s *PgStore }

// OperationsDB returns the handle for asyncpgx.New.
func (s *PgStore) OperationsDB() *OperationsDB { return &OperationsDB{s: s} }

func (d *OperationsDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if InUnit(ctx) {
		return d.s.db(ctx).Exec(ctx, sql, args...)
	}
	var tag pgconn.CommandTag
	err := d.s.scoped(ctx, TenantFrom(ctx), false, func(q Querier) error {
		var err error
		tag, err = q.Exec(ctx, sql, args...)
		return err
	})
	return tag, err
}

func (d *OperationsDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if InUnit(ctx) {
		return d.s.db(ctx).QueryRow(ctx, sql, args...)
	}
	return &scopedRow{ctx: ctx, d: d, sql: sql, args: args}
}

// Query is not used by the operation store; rows cannot outlive the scoped
// transaction they would be read in, so it is refused rather than half-supported.
func (d *OperationsDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if InUnit(ctx) {
		return d.s.db(ctx).Query(ctx, sql, args...)
	}
	return nil, errors.New("store: OperationsDB.Query needs a unit of work")
}

// Begin opens a transaction scoped to the tenant (a savepoint inside a unit).
func (d *OperationsDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.s.db(ctx).Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if !InUnit(ctx) {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, TenantFrom(ctx)); err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}
	return tx, nil
}

type scopedRow struct {
	ctx  context.Context
	d    *OperationsDB
	sql  string
	args []any
}

func (r *scopedRow) Scan(dest ...any) error {
	var scanErr error
	err := r.d.s.scoped(r.ctx, TenantFrom(r.ctx), false, func(q Querier) error {
		scanErr = q.QueryRow(r.ctx, r.sql, r.args...).Scan(dest...)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return errNoAggregate // nothing written; roll the scoped read back
		}
		return scanErr
	})
	if errors.Is(err, errNoAggregate) {
		return pgx.ErrNoRows
	}
	if err != nil && scanErr != nil {
		return scanErr
	}
	return err
}
