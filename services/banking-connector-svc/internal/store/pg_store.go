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
	_, err := p.pool.Exec(ctx, query, c.ConnectionID, c.TenantID, c.LegalEntityID, c.BankName, c.BIC, c.AccountNumber, c.Currency, c.Status, c.CreatedAt, c.UpdatedAt)
	return err
}

func (p *PgStore) GetConnectionByID(ctx context.Context, id string) (*domain.BankConnection, error) {
	query := `
		SELECT connection_id, tenant_id, legal_entity_id, bank_name, bic, account_number, currency, status, created_at, updated_at
		FROM bank_connections
		WHERE connection_id = $1
	`
	var c domain.BankConnection
	err := p.pool.QueryRow(ctx, query, id).Scan(&c.ConnectionID, &c.TenantID, &c.LegalEntityID, &c.BankName, &c.BIC, &c.AccountNumber, &c.Currency, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrConnectionNotFound
		}
		return nil, err
	}
	return &c, nil
}

func (p *PgStore) ListConnections(ctx context.Context, legalEntityID string) ([]domain.BankConnection, error) {
	query := `
		SELECT connection_id, tenant_id, legal_entity_id, bank_name, bic, account_number, currency, status, created_at, updated_at
		FROM bank_connections
		WHERE ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	rows, err := p.pool.Query(ctx, query, legalEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make([]domain.BankConnection, 0)
	for rows.Next() {
		var c domain.BankConnection
		if err := rows.Scan(&c.ConnectionID, &c.TenantID, &c.LegalEntityID, &c.BankName, &c.BIC, &c.AccountNumber, &c.Currency, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		res = append(res, c)
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
	_, err := p.pool.Exec(ctx, query, stmt.StatementID, stmt.ConnectionID, stmt.TenantID, stmt.StatementFormat, stmt.StatementDate, stmt.OpeningBalance, stmt.ClosingBalance, stmt.TransactionCount, stmt.IngestedAt)
	return err
}

func (p *PgStore) ListStatements(ctx context.Context, connectionID string) ([]domain.BankStatement, error) {
	query := `
		SELECT statement_id, connection_id, tenant_id, statement_format, statement_date, opening_balance, closing_balance, transaction_count, ingested_at
		FROM bank_statements
		WHERE connection_id = $1
		ORDER BY statement_date DESC
	`
	rows, err := p.pool.Query(ctx, query, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make([]domain.BankStatement, 0)
	for rows.Next() {
		var s domain.BankStatement
		if err := rows.Scan(&s.StatementID, &s.ConnectionID, &s.TenantID, &s.StatementFormat, &s.StatementDate, &s.OpeningBalance, &s.ClosingBalance, &s.TransactionCount, &s.IngestedAt); err != nil {
			return nil, err
		}
		res = append(res, s)
	}
	return res, nil
}
