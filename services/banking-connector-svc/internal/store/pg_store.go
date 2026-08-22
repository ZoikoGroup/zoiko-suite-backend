package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// withTenant runs fn inside a transaction with app.tenant_id set from the
// request context, so the tenant_isolation_policy added in migration
// 002_add_rls.sql has a value to enforce against.
//
// The tenant is read from context (set by middleware.TenantContext from a
// gateway-verified X-Tenant-Id) and never from a request body or query
// parameter. GetTenantID returns "" when absent rather than a fabricated
// default, and "" matches no real tenant_id — so a call that somehow
// arrives without a verified tenant sees and writes nothing, instead of
// silently operating on a synthetic "default-tenant" bucket shared with
// every other such caller.
func (p *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", middleware.GetTenantID(ctx),
	); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *PgStore) CreateConnection(ctx context.Context, c *domain.BankConnection) error {
	tenantID := middleware.GetTenantID(ctx)
	if c.ConnectionID == "" {
		c.ConnectionID = uuid.New().String()
	}
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	c.TenantID = tenantID

	query := `
		INSERT INTO bank_connections (connection_id, tenant_id, legal_entity_id, bank_name, bic, account_number, currency, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, c.ConnectionID, c.TenantID, c.LegalEntityID, c.BankName, c.BIC, c.AccountNumber, c.Currency, c.Status, c.CreatedAt, c.UpdatedAt)
		return err
	})
}

// GetConnectionByID looks up a connection scoped to the caller's tenant.
//
// The tenant predicate is new. This query was `WHERE connection_id = $1`
// alone, so any caller holding (or guessing) a connection_id could read
// another tenant's bank connection — bank_name, bic and account_number
// included. Returns ErrConnectionNotFound rather than a distinct
// forbidden error, so a probe cannot confirm that another tenant's
// connection_id exists.
func (p *PgStore) GetConnectionByID(ctx context.Context, id string) (*domain.BankConnection, error) {
	query := `
		SELECT connection_id, tenant_id, legal_entity_id, bank_name, bic, account_number, currency, status, created_at, updated_at
		FROM bank_connections
		WHERE connection_id = $1
		  AND tenant_id = $2
	`
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, id, middleware.GetTenantID(ctx)).Scan(
			&c.ConnectionID, &c.TenantID, &c.LegalEntityID, &c.BankName, &c.BIC, &c.AccountNumber, &c.Currency, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrConnectionNotFound
		}
		return nil, err
	}
	return &c, nil
}

// ListConnections lists the caller's own tenant's connections, optionally
// narrowed to one legal entity.
//
// The tenant predicate is new and, unlike the legal-entity one, is NOT
// optional. This query was `WHERE ($1 = '' OR legal_entity_id = $1)`, so
// omitting legal_entity_id — the shorter, easier request — disabled the
// only filter present and returned every tenant's bank connections. The
// legal-entity dimension stays optional because narrowing within your own
// tenant is a legitimate choice; the tenant dimension never is.
func (p *PgStore) ListConnections(ctx context.Context, legalEntityID string) ([]domain.BankConnection, error) {
	query := `
		SELECT connection_id, tenant_id, legal_entity_id, bank_name, bic, account_number, currency, status, created_at, updated_at
		FROM bank_connections
		WHERE tenant_id = $2
		  AND ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	res := make([]domain.BankConnection, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, legalEntityID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.BankConnection
			if err := rows.Scan(&c.ConnectionID, &c.TenantID, &c.LegalEntityID, &c.BankName, &c.BIC, &c.AccountNumber, &c.Currency, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
				return err
			}
			res = append(res, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (p *PgStore) RecordStatement(ctx context.Context, stmt *domain.BankStatement) error {
	tenantID := middleware.GetTenantID(ctx)
	if stmt.StatementID == "" {
		stmt.StatementID = uuid.New().String()
	}
	stmt.IngestedAt = time.Now().UTC()
	stmt.TenantID = tenantID

	query := `
		INSERT INTO bank_statements (statement_id, connection_id, tenant_id, statement_format, statement_date, opening_balance, closing_balance, transaction_count, ingested_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, stmt.StatementID, stmt.ConnectionID, stmt.TenantID, stmt.StatementFormat, stmt.StatementDate, stmt.OpeningBalance, stmt.ClosingBalance, stmt.TransactionCount, stmt.IngestedAt)
		return err
	})
}

// ListStatements lists statements for a connection, scoped to the
// caller's tenant.
//
// The tenant predicate is new. This query was `WHERE connection_id = $1`
// alone, so any caller holding another tenant's connection_id could read
// its statements — opening/closing balances and transaction counts.
func (p *PgStore) ListStatements(ctx context.Context, connectionID string) ([]domain.BankStatement, error) {
	query := `
		SELECT statement_id, connection_id, tenant_id, statement_format, statement_date, opening_balance, closing_balance, transaction_count, ingested_at
		FROM bank_statements
		WHERE connection_id = $1
		  AND tenant_id = $2
		ORDER BY statement_date DESC
	`
	res := make([]domain.BankStatement, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, connectionID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var s domain.BankStatement
			if err := rows.Scan(&s.StatementID, &s.ConnectionID, &s.TenantID, &s.StatementFormat, &s.StatementDate, &s.OpeningBalance, &s.ClosingBalance, &s.TransactionCount, &s.IngestedAt); err != nil {
				return err
			}
			res = append(res, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
