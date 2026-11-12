package store

// Unit of work: one database transaction for every write a request makes.
//
// Before this, each store method committed on its own and the registry events
// (ZS-JUR-001 s26) were written to the outbox afterwards, through the pool
// (Governance audit J-11): a crash or an outbox failure between the two lost
// the event while the change it announced stayed committed. Inside a unit,
// every statement of the request and every outbox row it writes share one
// transaction, so they commit together or not at all. The HTTP middleware
// (handler.UnitOfWork) releases the response only after the commit.
//
// The transaction also carries the caller's tenant as app.tenant_id, the
// setting the row-level security policies of the tenant tables read (R4).
//
// Semantics inside a unit are kept equal to the pool's:
//
//   - The transaction begins lazily, at the first statement, so a request
//     refused before it touches the database (validation, authorization)
//     never holds a connection.
//   - Every statement outside an explicit store transaction runs in its own
//     savepoint. On the pool a failing statement (a unique violation an
//     idempotent create maps to "already exists", a malformed id read as not
//     found) left nothing behind; in one transaction it would abort every
//     statement after it. The savepoint confines the failure to the statement.
//   - A store method that opens its own transaction (Begin) gets a savepoint
//     for its whole body, so it stays all-or-nothing within the request.
//
// Requests without a unit (reads, the Kafka consumer, the outbox worker, the
// ticker jobs) use the pool exactly as before.

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// dbtx is what both *pgxpool.Pool and a unit's savepoint wrapper provide.
type dbtx interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type unitKey struct{}

// Unit is one request's transaction. Safe for the sequential use a request
// makes of it; a pgx transaction is never used from two goroutines at once.
type Unit struct {
	pool     *pgxpool.Pool
	tenantID string

	mu      sync.Mutex
	tx      pgx.Tx
	aborted error
}

// ErrUnitAborted is returned by Finish when something the request needed in
// its transaction (an outbox row, typically) could not be written.
var ErrUnitAborted = errors.New("store: unit of work aborted")

// BeginUnit returns ctx carrying a new unit for tenantID ("" when the request
// has no tenant: the RLS policies then match no tenant row). Nothing touches
// the database until the first statement.
func (s *PgStore) BeginUnit(ctx context.Context, tenantID string) (context.Context, *Unit) {
	u := &Unit{pool: s.pool, tenantID: tenantID}
	return context.WithValue(ctx, unitKey{}, u), u
}

func unitFrom(ctx context.Context) *Unit {
	u, _ := ctx.Value(unitKey{}).(*Unit)
	return u
}

// InUnit reports whether ctx carries a unit of work.
func InUnit(ctx context.Context) bool { return unitFrom(ctx) != nil }

// AbortUnit marks ctx's unit so Finish rolls it back. It is how a write that
// must not commit without its companion (a registry event without its outbox
// row) refuses the whole request. A no-op outside a unit.
func AbortUnit(ctx context.Context, cause error) {
	if u := unitFrom(ctx); u != nil {
		u.mu.Lock()
		if u.aborted == nil {
			u.aborted = cause
		}
		u.mu.Unlock()
	}
}

func (u *Unit) txFor(ctx context.Context) (pgx.Tx, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tx != nil {
		return u.tx, nil
	}
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: begin unit: %v", domain.ErrStoreUnavailable, err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, u.tenantID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("%w: set tenant: %v", domain.ErrStoreUnavailable, err)
	}
	u.tx = tx
	return tx, nil
}

// Began reports whether the unit opened a transaction.
func (u *Unit) Began() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.tx != nil
}

// Finish commits the unit's transaction, or rolls it back when the unit was
// aborted. It returns ErrUnitAborted (wrapping the cause) for an aborted unit,
// and ErrStoreUnavailable when the commit fails; either way nothing was
// written. A unit that never began has nothing to do.
func (u *Unit) Finish(ctx context.Context) error {
	u.mu.Lock()
	tx, aborted := u.tx, u.aborted
	u.tx = nil
	u.mu.Unlock()
	if aborted != nil {
		if tx != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
		return fmt.Errorf("%w: %v", ErrUnitAborted, aborted)
	}
	if tx == nil {
		return nil
	}
	// The client may have gone away; the work it asked for is still decided
	// here, not by its connection.
	if err := tx.Commit(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w: commit unit: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// Rollback discards the unit's work (a panic, or a response that must not stand).
func (u *Unit) Rollback(ctx context.Context) {
	u.mu.Lock()
	tx := u.tx
	u.tx = nil
	u.mu.Unlock()
	if tx != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}
}

// db is the handle every store statement runs on: the unit's transaction
// (statement by statement in savepoints) when ctx carries one, else the pool.
func (s *PgStore) db(ctx context.Context) dbtx {
	if u := unitFrom(ctx); u != nil {
		return unitDB{u: u}
	}
	return s.pool
}

// DB is db for other packages (the outbox publisher writes registry events
// through it, so they join the request's transaction).
func (s *PgStore) DB(ctx context.Context) Querier { return s.db(ctx) }

// ── row-level security scope (R4, migration 000032) ────────────────────────

type tenantKey struct{}

// WithTenant records the tenant a request without a unit (a read) acts for,
// so the store's reads of tenant tables are scoped to it.
func WithTenant(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

// TenantFrom is the tenant ctx acts for: its unit's, else WithTenant's, else "".
func TenantFrom(ctx context.Context) string {
	if u := unitFrom(ctx); u != nil {
		return u.tenantID
	}
	t, _ := ctx.Value(tenantKey{}).(string)
	return t
}

// setCrossTenant switches the transaction's cross-tenant read of decision
// evidence on or off. Only operator paths that the handler has authorized use
// it, and they switch it off again before their transaction is released into
// the request's unit, so it never outlives the statements that need it.
func setCrossTenant(ctx context.Context, q Querier, on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	_, err := q.Exec(ctx, `SELECT set_config('app.cross_tenant', $1, true)`, v)
	return err
}

// scoped runs fn in a transaction whose row-level security scope is tenantID
// ("" = no tenant) and, with cross, every tenant's decision evidence. Reads of
// the tenant tables go through it: on the pool a statement has no tenant, and
// would see nothing. Inside a unit it is a savepoint, and the unit's own scope
// is restored before the savepoint is released.
func (s *PgStore) scoped(ctx context.Context, tenantID string, cross bool, fn func(q Querier) error) error {
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: begin scoped read: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("%w: set tenant: %v", domain.ErrStoreUnavailable, err)
	}
	if cross {
		if err := setCrossTenant(ctx, tx, true); err != nil {
			return fmt.Errorf("%w: set cross-tenant: %v", domain.ErrStoreUnavailable, err)
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	if u := unitFrom(ctx); u != nil {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.cross_tenant', 'off', true)`, u.tenantID); err != nil {
			return fmt.Errorf("%w: restore scope: %v", domain.ErrStoreUnavailable, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit scoped read: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// isTx reports whether q is already a transaction the caller manages.
func isTx(q Querier) bool {
	_, ok := q.(pgx.Tx)
	return ok
}

// unitDB runs each statement in a savepoint of the unit's transaction.
type unitDB struct{ u *Unit }

func (d unitDB) savepoint(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.u.txFor(ctx)
	if err != nil {
		return nil, err
	}
	return tx.Begin(ctx) // pgx: a nested Begin is SAVEPOINT
}

func (d unitDB) Begin(ctx context.Context) (pgx.Tx, error) { return d.savepoint(ctx) }

func (d unitDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	sp, err := d.savepoint(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := sp.Exec(ctx, sql, args...)
	if err != nil {
		_ = sp.Rollback(ctx)
		return tag, err
	}
	return tag, sp.Commit(ctx)
}

func (d unitDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &unitRow{ctx: ctx, d: d, sql: sql, args: args}
}

type unitRow struct {
	ctx  context.Context
	d    unitDB
	sql  string
	args []any
}

func (r *unitRow) Scan(dest ...any) error {
	sp, err := r.d.savepoint(r.ctx)
	if err != nil {
		return err
	}
	if err := sp.QueryRow(r.ctx, r.sql, r.args...).Scan(dest...); err != nil {
		_ = sp.Rollback(r.ctx)
		return err
	}
	return sp.Commit(r.ctx)
}

func (d unitDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	sp, err := d.savepoint(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := sp.Query(ctx, sql, args...)
	if err != nil {
		_ = sp.Rollback(ctx)
		return nil, err
	}
	return &unitRows{Rows: rows, sp: sp, ctx: ctx}, nil
}

// unitRows releases the statement's savepoint when the rows are closed, or
// rolls back to it when reading them failed.
type unitRows struct {
	pgx.Rows
	sp     pgx.Tx
	ctx    context.Context
	closed bool
}

func (r *unitRows) Close() {
	if r.closed {
		return
	}
	r.closed = true
	r.Rows.Close()
	if r.Rows.Err() != nil {
		_ = r.sp.Rollback(r.ctx)
		return
	}
	_ = r.sp.Commit(r.ctx)
}

func (r *unitRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	// Release the savepoint as soon as the rows are exhausted: code that
	// drains rows and issues its next statement before the deferred Close
	// must find the connection free.
	r.Close()
	return false
}

// AggregateVersion reads the row_version (000020, 000034) of the row `where`
// selects in `table`. table and where are code constants, never request input;
// args bind the request's keys. With lock the row is read FOR UPDATE, so a
// command that checked If-Match holds it until its unit of work commits and no
// other request can change it in between. found is false when no row matches
// (or a key is malformed). Scoped to the request's tenant, so a tenant table's
// row of another tenant is not found.
func (s *PgStore) AggregateVersion(ctx context.Context, table, where string, lock bool, args ...any) (int64, bool, error) {
	q := `SELECT row_version FROM ` + table + ` WHERE ` + where
	if lock {
		q += ` FOR UPDATE`
	}
	var v int64
	err := s.scoped(ctx, TenantFrom(ctx), false, func(tx Querier) error {
		err := tx.QueryRow(ctx, q, args...).Scan(&v)
		if errors.Is(err, pgx.ErrNoRows) || isInvalidTextRepresentation(err) {
			// Rolls the scoped savepoint back: a malformed key aborted it.
			return errNoAggregate
		}
		return err
	})
	if errors.Is(err, errNoAggregate) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("%w: aggregate version: %v", domain.ErrStoreUnavailable, err)
	}
	return v, true, nil
}

var errNoAggregate = errors.New("store: no such aggregate")
