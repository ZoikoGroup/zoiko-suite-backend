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
	_, err := p.pool.Exec(ctx, query, b.BridgeID, b.TenantID, b.LegalEntityID, b.BridgeName, b.Protocol, b.EndpointURL, b.AuthType, b.Status, b.CreatedAt, b.UpdatedAt)
	return err
}

func (p *PgStore) GetBridgeByID(ctx context.Context, id string) (*domain.ApiBridge, error) {
	query := `
		SELECT bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status, created_at, updated_at
		FROM api_bridges
		WHERE bridge_id = $1
	`
	var b domain.ApiBridge
	err := p.pool.QueryRow(ctx, query, id).Scan(&b.BridgeID, &b.TenantID, &b.LegalEntityID, &b.BridgeName, &b.Protocol, &b.EndpointURL, &b.AuthType, &b.Status, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrBridgeNotFound
		}
		return nil, err
	}
	return &b, nil
}

func (p *PgStore) ListBridges(ctx context.Context, legalEntityID string) ([]domain.ApiBridge, error) {
	query := `
		SELECT bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status, created_at, updated_at
		FROM api_bridges
		WHERE ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	rows, err := p.pool.Query(ctx, query, legalEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make([]domain.ApiBridge, 0)
	for rows.Next() {
		var b domain.ApiBridge
		if err := rows.Scan(&b.BridgeID, &b.TenantID, &b.LegalEntityID, &b.BridgeName, &b.Protocol, &b.EndpointURL, &b.AuthType, &b.Status, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		res = append(res, b)
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
	_, err := p.pool.Exec(ctx, query, log.LogID, log.BridgeID, log.TenantID, log.PayloadSummary, log.IngestionStatus, log.ErrorMessage, log.IngestedAt)
	return err
}

func (p *PgStore) ListIngestionLogs(ctx context.Context, bridgeID string) ([]domain.IngestionLog, error) {
	query := `
		SELECT log_id, bridge_id, tenant_id, payload_summary, ingestion_status, COALESCE(error_message, ''), ingested_at
		FROM ingestion_logs
		WHERE bridge_id = $1
		ORDER BY ingested_at DESC
	`
	rows, err := p.pool.Query(ctx, query, bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make([]domain.IngestionLog, 0)
	for rows.Next() {
		var l domain.IngestionLog
		if err := rows.Scan(&l.LogID, &l.BridgeID, &l.TenantID, &l.PayloadSummary, &l.IngestionStatus, &l.ErrorMessage, &l.IngestedAt); err != nil {
			return nil, err
		}
		res = append(res, l)
	}
	return res, nil
}
