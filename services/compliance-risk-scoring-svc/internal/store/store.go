// Package store is the PostgreSQL persistence layer.
//
// Tenant isolation is enforced twice on every tenant-owned query:
//
//  1. Row-level security. InTenantTx opens a transaction and runs
//     SELECT set_config('app.tenant_id', $1, true) before any statement, so
//     the RLS policies from migration 000002 (FORCE + USING + WITH CHECK)
//     only ever see that tenant's rows.
//  2. An explicit "tenant_id = $n" predicate in every SQL statement, so a
//     mis-provisioned database (RLS disabled, superuser connection) still
//     cannot leak another tenant's rows.
//
// Numeric columns are always read as col::text and parsed into
// domain.Hundredths, and DATE columns as col::text, so no value passes
// through float64 or time.Time on its way out.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
)

// ErrNotFound is returned when a row does not exist for this tenant. Rows
// that belong to another tenant are indistinguishable from missing rows.
var ErrNotFound = errors.New("not found")

// ErrUniqueViolation wraps a Postgres unique-constraint failure; Constraint
// names the index that fired.
type ErrUniqueViolation struct {
	Constraint string
}

func (e *ErrUniqueViolation) Error() string {
	return "unique constraint violated: " + e.Constraint
}

// Store owns the connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps an open pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Ping checks database reachability for /readyz.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// TenantQueries runs statements inside one tenant-scoped transaction.
// Every method filters on TenantID in addition to RLS.
type TenantQueries struct {
	tx       pgx.Tx
	TenantID string
}

// InTenantTx runs work in a transaction scoped to tenantID. The transaction
// commits only if work returns nil.
func (s *Store) InTenantTx(ctx context.Context, tenantID string, work func(q *TenantQueries) error) error {
	if tenantID == "" {
		return errors.New("store: refusing to open a transaction without a tenant")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant scope: %w", err)
	}

	if err := work(&TenantQueries{tx: tx, TenantID: tenantID}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return translatePgError(fmt.Errorf("commit: %w", err))
	}
	return nil
}

// translatePgError turns driver errors the service layer must react to into
// typed errors; anything else is returned unchanged.
func translatePgError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return &ErrUniqueViolation{Constraint: pgErr.ConstraintName}
	}
	return err
}

// decimalScanner scans a NUMERIC selected as ::text into a Hundredths.
type decimalScanner struct {
	target **domain.Hundredths // nullable target
	value  *domain.Hundredths  // non-null target
}

func scanDecimal(target *domain.Hundredths) *decimalScanner {
	return &decimalScanner{value: target}
}

func scanNullableDecimal(target **domain.Hundredths) *decimalScanner {
	return &decimalScanner{target: target}
}

// Scan implements sql.Scanner.
func (d *decimalScanner) Scan(src any) error {
	var text string
	switch typed := src.(type) {
	case nil:
		if d.target != nil {
			*d.target = nil
			return nil
		}
		return errors.New("store: NULL in non-null decimal column")
	case string:
		text = typed
	case []byte:
		text = string(typed)
	default:
		return fmt.Errorf("store: unexpected decimal source %T", src)
	}

	parsed, err := domain.ParseHundredths(text)
	if err != nil {
		return err
	}
	if d.target != nil {
		*d.target = &parsed
		return nil
	}
	*d.value = parsed
	return nil
}

// nullableDecimalText converts an optional decimal into a SQL parameter.
func nullableDecimalText(value *domain.Hundredths) *string {
	if value == nil {
		return nil
	}
	text := value.String()
	return &text
}
