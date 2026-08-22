package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
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
// operating on a synthetic "default-tenant" bucket shared with every other
// such caller.
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

func (p *PgStore) CreateBridge(ctx context.Context, b *domain.ApiBridge) error {
	tenantID := middleware.GetTenantID(ctx)
	if b.BridgeID == "" {
		b.BridgeID = uuid.New().String()
	}
	now := time.Now().UTC()
	b.CreatedAt = now
	b.UpdatedAt = now
	b.TenantID = tenantID

	query := `
		INSERT INTO api_bridges (bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, b.BridgeID, b.TenantID, b.LegalEntityID, b.BridgeName, b.Protocol, b.EndpointURL, b.AuthType, b.Status, b.CreatedAt, b.UpdatedAt)
		return err
	})
}

// GetBridgeByID looks up a bridge scoped to the caller's tenant.
//
// The tenant predicate is new. This query was `WHERE bridge_id = $1`
// alone, so any caller holding (or guessing) a bridge_id could read
// another tenant's integration configuration — endpoint_url and auth_type
// included. Returns ErrBridgeNotFound rather than a distinct forbidden
// error, so a probe cannot confirm that another tenant's bridge_id exists.
func (p *PgStore) GetBridgeByID(ctx context.Context, id string) (*domain.ApiBridge, error) {
	query := `
		SELECT bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status, created_at, updated_at
		FROM api_bridges
		WHERE bridge_id = $1
		  AND tenant_id = $2
	`
	var b domain.ApiBridge
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, id, middleware.GetTenantID(ctx)).Scan(
			&b.BridgeID, &b.TenantID, &b.LegalEntityID, &b.BridgeName, &b.Protocol, &b.EndpointURL, &b.AuthType, &b.Status, &b.CreatedAt, &b.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrBridgeNotFound
		}
		return nil, err
	}
	return &b, nil
}

// ListBridges lists the caller's own tenant's bridges, optionally narrowed
// to one legal entity.
//
// The tenant predicate is new and, unlike the legal-entity one, is NOT
// optional. This query matched when the legal_entity_id parameter was the
// empty string OR equalled the column, so omitting legal_entity_id — the
// shorter, easier request — disabled the only filter present and returned
// every tenant's bridges. Narrowing within your own tenant is a legitimate
// choice; skipping the tenant dimension never is.
func (p *PgStore) ListBridges(ctx context.Context, legalEntityID string) ([]domain.ApiBridge, error) {
	query := `
		SELECT bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status, created_at, updated_at
		FROM api_bridges
		WHERE tenant_id = $2
		  AND ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	res := make([]domain.ApiBridge, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, legalEntityID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var b domain.ApiBridge
			if err := rows.Scan(&b.BridgeID, &b.TenantID, &b.LegalEntityID, &b.BridgeName, &b.Protocol, &b.EndpointURL, &b.AuthType, &b.Status, &b.CreatedAt, &b.UpdatedAt); err != nil {
				return err
			}
			res = append(res, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (p *PgStore) RecordIngestion(ctx context.Context, log *domain.IngestionLog) error {
	tenantID := middleware.GetTenantID(ctx)
	if log.LogID == "" {
		log.LogID = uuid.New().String()
	}
	log.IngestedAt = time.Now().UTC()
	log.TenantID = tenantID

	query := `
		INSERT INTO ingestion_logs (log_id, bridge_id, tenant_id, payload_summary, ingestion_status, error_message, ingested_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, log.LogID, log.BridgeID, log.TenantID, log.PayloadSummary, log.IngestionStatus, log.ErrorMessage, log.IngestedAt)
		return err
	})
}

// ListIngestionLogs lists a bridge's ingestion logs, scoped to the
// caller's tenant.
//
// The tenant predicate is new. This query was `WHERE bridge_id = $1`
// alone, so any caller holding another tenant's bridge_id could read its
// ingestion history — payload summaries and error messages, which are the
// contents flowing through that tenant's integration.
func (p *PgStore) ListIngestionLogs(ctx context.Context, bridgeID string) ([]domain.IngestionLog, error) {
	query := `
		SELECT log_id, bridge_id, tenant_id, payload_summary, ingestion_status, COALESCE(error_message, ''), ingested_at
		FROM ingestion_logs
		WHERE bridge_id = $1
		  AND tenant_id = $2
		ORDER BY ingested_at DESC
	`
	res := make([]domain.IngestionLog, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, bridgeID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var l domain.IngestionLog
			if err := rows.Scan(&l.LogID, &l.BridgeID, &l.TenantID, &l.PayloadSummary, &l.IngestionStatus, &l.ErrorMessage, &l.IngestedAt); err != nil {
				return err
			}
			res = append(res, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
