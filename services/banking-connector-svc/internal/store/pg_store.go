package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
)

type PgStore struct {
	pool        *pgxpool.Pool
	eventRegion string
}

// Option configures a PgStore at construction.
type Option func(*PgStore)

// WithEventRegion sets the residency region carried by emitted events
// (ZS-EVENT-001 §4, envelope.Spec.ResidencyRegion — required, a controlled
// lower-case code such as "uk" or "eu-west").
func WithEventRegion(region string) Option {
	return func(s *PgStore) { s.eventRegion = region }
}

func NewPgStore(pool *pgxpool.Pool, opts ...Option) *PgStore {
	s := &PgStore{pool: pool}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// enqueueEvent builds and enqueues one event inside the caller's own
// transaction — the entire point of the transactional outbox (ZS-EVENT-001
// §6): the event and the state change it reports commit atomically, or
// neither does. legacyType is the pre-standard event_type string existing
// consumers filter on; always pass it explicitly — envelope.New silently
// defaults it to the canonical type when empty, which would silently break
// any consumer still reading the old name.
func (p *PgStore) enqueueEvent(ctx context.Context, tx pgx.Tx, canonicalType, legacyType, aggregateType, aggregateID, tenantID, legalEntityID, actorID, correlationID string, data any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            canonicalType,
		LegacyType:      legacyType,
		Service:         "banking-connector-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   aggregateType,
		AggregateID:     aggregateID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: p.eventRegion,
		Classification:  envelope.Confidential,
		Data:            data,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", canonicalType, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue %s event: %w", canonicalType, err)
	}
	return nil
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

func (p *PgStore) CreateConnection(ctx context.Context, c *domain.BankConnection, actorID, correlationID string) error {
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
		if _, err := tx.Exec(ctx, query, c.ConnectionID, c.TenantID, c.LegalEntityID, c.BankName, c.BIC, c.AccountNumber, c.Currency, c.Status, c.CreatedAt, c.UpdatedAt); err != nil {
			return err
		}
		return p.enqueueEvent(ctx, tx, "com.zoikosuite.banking.bank-connection.created", "banking.connection.created",
			"bank-connection", c.ConnectionID, c.TenantID, c.LegalEntityID, actorID, correlationID, c)
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
	// Select the full BNK-02 row, not just the original nine columns. The
	// BNK-02 lifecycle handlers load the connection through this method
	// before acting on it; a partial row left Region and TokenLeaseRef empty,
	// so region enforcement compared against "" and suspend/revoke passed an
	// empty lease to the vault (a no-op revoke). Treasury-svc's BNK-01
	// ListConnectionOptions also reads this endpoint and filters on
	// bank_account_id, which the partial row never returned. One projection
	// feeds all three callers.
	query := `
		SELECT ` + bnk02Columns + `
		FROM bank_connections
		WHERE connection_id = $1
		  AND tenant_id = $2
	`
	var c domain.BankConnection
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return scanConnection(tx.QueryRow(ctx, query, id, middleware.GetTenantID(ctx)), &c)
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
	// Same full BNK-02 projection as GetConnectionByID — treasury-svc's
	// BNK-01 ListConnectionOptions route reads this response and filters on
	// bank_account_id, which the original nine-column projection omitted.
	query := `
		SELECT ` + bnk02Columns + `
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
			if err := scanConnection(rows, &c); err != nil {
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

func (p *PgStore) RecordStatement(ctx context.Context, stmt *domain.BankStatement, legalEntityID, actorID, correlationID string) error {
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
		if _, err := tx.Exec(ctx, query, stmt.StatementID, stmt.ConnectionID, stmt.TenantID, stmt.StatementFormat, stmt.StatementDate, stmt.OpeningBalance, stmt.ClosingBalance, stmt.TransactionCount, stmt.IngestedAt); err != nil {
			return err
		}
		// BankStatement itself carries no legal_entity_id (it links back to a
		// connection by id only) — the caller resolves it from the owning
		// connection, same as the pre-outbox Publisher call site this replaces.
		return p.enqueueEvent(ctx, tx, "com.zoikosuite.banking.bank-statement.ingested", "banking.statement.ingested",
			"bank-statement", stmt.StatementID, stmt.TenantID, legalEntityID, actorID, correlationID, stmt)
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
