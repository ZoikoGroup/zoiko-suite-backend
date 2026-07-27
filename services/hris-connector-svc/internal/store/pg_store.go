package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/hris-connector-svc/internal/domain"
	"zoiko.io/hris-connector-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (p *PgStore) CreateIntegration(ctx context.Context, integration *domain.HrisIntegration) error {
	tenantID := middleware.GetTenantID(ctx)
	if integration.IntegrationID == "" {
		integration.IntegrationID = uuid.New().String()
	}
	now := time.Now().UTC()
	integration.CreatedAt = now
	integration.UpdatedAt = now
	integration.TenantID = tenantID

	query := `
		INSERT INTO hris_integrations (integration_id, tenant_id, legal_entity_id, provider_name, api_endpoint, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`
	_, err := p.pool.Exec(ctx, query, integration.IntegrationID, integration.TenantID, integration.LegalEntityID, integration.ProviderName, integration.ApiEndpoint, integration.Status, integration.CreatedAt, integration.UpdatedAt)
	return err
}

func (p *PgStore) GetIntegrationByID(ctx context.Context, id string) (*domain.HrisIntegration, error) {
	query := `
		SELECT integration_id, tenant_id, legal_entity_id, provider_name, api_endpoint, status, created_at, updated_at
		FROM hris_integrations
		WHERE integration_id = $1
	`
	var i domain.HrisIntegration
	err := p.pool.QueryRow(ctx, query, id).Scan(&i.IntegrationID, &i.TenantID, &i.LegalEntityID, &i.ProviderName, &i.ApiEndpoint, &i.Status, &i.CreatedAt, &i.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrIntegrationNotFound
		}
		return nil, err
	}
	return &i, nil
}

func (p *PgStore) ListIntegrations(ctx context.Context, legalEntityID string) ([]domain.HrisIntegration, error) {
	query := `
		SELECT integration_id, tenant_id, legal_entity_id, provider_name, api_endpoint, status, created_at, updated_at
		FROM hris_integrations
		WHERE ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	rows, err := p.pool.Query(ctx, query, legalEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make([]domain.HrisIntegration, 0)
	for rows.Next() {
		var i domain.HrisIntegration
		if err := rows.Scan(&i.IntegrationID, &i.TenantID, &i.LegalEntityID, &i.ProviderName, &i.ApiEndpoint, &i.Status, &i.CreatedAt, &i.UpdatedAt); err != nil {
			return nil, err
		}
		res = append(res, i)
	}
	return res, nil
}

func (p *PgStore) CreateSyncJob(ctx context.Context, job *domain.SyncJob) error {
	tenantID := middleware.GetTenantID(ctx)
	if job.JobID == "" {
		job.JobID = uuid.New().String()
	}
	job.StartedAt = time.Now().UTC()
	job.TenantID = tenantID

	query := `
		INSERT INTO sync_jobs (job_id, integration_id, tenant_id, sync_type, records_synced, status, error_message, started_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := p.pool.Exec(ctx, query, job.JobID, job.IntegrationID, job.TenantID, job.SyncType, job.RecordsSynced, job.Status, job.ErrorMessage, job.StartedAt, job.CompletedAt)
	return err
}

func (p *PgStore) GetSyncJobByID(ctx context.Context, id string) (*domain.SyncJob, error) {
	query := `
		SELECT job_id, integration_id, tenant_id, sync_type, records_synced, status, COALESCE(error_message, ''), started_at, completed_at
		FROM sync_jobs
		WHERE job_id = $1
	`
	var j domain.SyncJob
	err := p.pool.QueryRow(ctx, query, id).Scan(&j.JobID, &j.IntegrationID, &j.TenantID, &j.SyncType, &j.RecordsSynced, &j.Status, &j.ErrorMessage, &j.StartedAt, &j.CompletedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrSyncJobNotFound
		}
		return nil, err
	}
	return &j, nil
}

func (p *PgStore) ListSyncJobs(ctx context.Context, integrationID string) ([]domain.SyncJob, error) {
	query := `
		SELECT job_id, integration_id, tenant_id, sync_type, records_synced, status, COALESCE(error_message, ''), started_at, completed_at
		FROM sync_jobs
		WHERE ($1 = '' OR integration_id = $1)
		ORDER BY started_at DESC
	`
	rows, err := p.pool.Query(ctx, query, integrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := make([]domain.SyncJob, 0)
	for rows.Next() {
		var j domain.SyncJob
		if err := rows.Scan(&j.JobID, &j.IntegrationID, &j.TenantID, &j.SyncType, &j.RecordsSynced, &j.Status, &j.ErrorMessage, &j.StartedAt, &j.CompletedAt); err != nil {
			return nil, err
		}
		res = append(res, j)
	}
	return res, nil
}
