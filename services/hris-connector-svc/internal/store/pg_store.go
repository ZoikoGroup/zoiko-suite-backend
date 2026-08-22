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
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, integration.IntegrationID, integration.TenantID, integration.LegalEntityID, integration.ProviderName, integration.ApiEndpoint, integration.Status, integration.CreatedAt, integration.UpdatedAt)
		return err
	})
}

// GetIntegrationByID looks up an integration scoped to the caller's
// tenant.
//
// The tenant predicate is new. This query was `WHERE integration_id = $1`
// alone, so any caller holding (or guessing) an integration_id could read
// another tenant's provider_name and api_endpoint — the address of the HR
// system holding their workforce data. Returns ErrIntegrationNotFound
// rather than a distinct forbidden error, so a probe cannot confirm that
// another tenant's integration_id exists.
func (p *PgStore) GetIntegrationByID(ctx context.Context, id string) (*domain.HrisIntegration, error) {
	query := `
		SELECT integration_id, tenant_id, legal_entity_id, provider_name, api_endpoint, status, created_at, updated_at
		FROM hris_integrations
		WHERE integration_id = $1
		  AND tenant_id = $2
	`
	var i domain.HrisIntegration
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, id, middleware.GetTenantID(ctx)).Scan(
			&i.IntegrationID, &i.TenantID, &i.LegalEntityID, &i.ProviderName, &i.ApiEndpoint, &i.Status, &i.CreatedAt, &i.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrIntegrationNotFound
		}
		return nil, err
	}
	return &i, nil
}

// ListIntegrations lists the caller's own tenant's integrations,
// optionally narrowed to one legal entity.
//
// The tenant predicate is new and, unlike the legal-entity one, is NOT
// optional. This query matched when the legal_entity_id parameter was the
// empty string OR equalled the column, so omitting legal_entity_id — the
// shorter, easier request — disabled the only filter present and returned
// every tenant's HR integrations.
func (p *PgStore) ListIntegrations(ctx context.Context, legalEntityID string) ([]domain.HrisIntegration, error) {
	query := `
		SELECT integration_id, tenant_id, legal_entity_id, provider_name, api_endpoint, status, created_at, updated_at
		FROM hris_integrations
		WHERE tenant_id = $2
		  AND ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	res := make([]domain.HrisIntegration, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, legalEntityID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var i domain.HrisIntegration
			if err := rows.Scan(&i.IntegrationID, &i.TenantID, &i.LegalEntityID, &i.ProviderName, &i.ApiEndpoint, &i.Status, &i.CreatedAt, &i.UpdatedAt); err != nil {
				return err
			}
			res = append(res, i)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
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
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, job.JobID, job.IntegrationID, job.TenantID, job.SyncType, job.RecordsSynced, job.Status, job.ErrorMessage, job.StartedAt, job.CompletedAt)
		return err
	})
}

// GetSyncJobByID looks up a sync job scoped to the caller's tenant.
//
// The tenant predicate is new. This query was `WHERE job_id = $1` alone,
// so any caller holding another tenant's job_id could read its sync
// history — sync_type, records_synced and error_message, the last of which
// can carry provider detail about that tenant's HR system.
func (p *PgStore) GetSyncJobByID(ctx context.Context, id string) (*domain.SyncJob, error) {
	query := `
		SELECT job_id, integration_id, tenant_id, sync_type, records_synced, status, COALESCE(error_message, ''), started_at, completed_at
		FROM sync_jobs
		WHERE job_id = $1
		  AND tenant_id = $2
	`
	var j domain.SyncJob
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, id, middleware.GetTenantID(ctx)).Scan(
			&j.JobID, &j.IntegrationID, &j.TenantID, &j.SyncType, &j.RecordsSynced, &j.Status, &j.ErrorMessage, &j.StartedAt, &j.CompletedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrSyncJobNotFound
		}
		return nil, err
	}
	return &j, nil
}

// ListSyncJobs lists sync jobs for the caller's tenant, optionally
// narrowed to one integration.
//
// The tenant predicate is new, and this was a self-disabling filter: the
// ONLY predicate matched when the integration_id parameter was the empty
// string OR equalled the column, so calling it with no integration_id
// returned every tenant's sync history. The integration dimension stays
// optional; the tenant dimension never is.
func (p *PgStore) ListSyncJobs(ctx context.Context, integrationID string) ([]domain.SyncJob, error) {
	query := `
		SELECT job_id, integration_id, tenant_id, sync_type, records_synced, status, COALESCE(error_message, ''), started_at, completed_at
		FROM sync_jobs
		WHERE tenant_id = $2
		  AND ($1 = '' OR integration_id = $1)
		ORDER BY started_at DESC
	`
	res := make([]domain.SyncJob, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, integrationID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var j domain.SyncJob
			if err := rows.Scan(&j.JobID, &j.IntegrationID, &j.TenantID, &j.SyncType, &j.RecordsSynced, &j.Status, &j.ErrorMessage, &j.StartedAt, &j.CompletedAt); err != nil {
				return err
			}
			res = append(res, j)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
