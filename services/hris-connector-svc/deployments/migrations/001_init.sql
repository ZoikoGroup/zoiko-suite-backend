CREATE TABLE IF NOT EXISTS hris_integrations (
    integration_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    provider_name VARCHAR(128) NOT NULL,
    api_endpoint VARCHAR(512) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_hris_integrations_tenant_entity ON hris_integrations(tenant_id, legal_entity_id);

CREATE TABLE IF NOT EXISTS sync_jobs (
    job_id VARCHAR(64) PRIMARY KEY,
    integration_id VARCHAR(64) NOT NULL REFERENCES hris_integrations(integration_id) ON DELETE CASCADE,
    tenant_id VARCHAR(64) NOT NULL,
    sync_type VARCHAR(64) NOT NULL,
    records_synced INT NOT NULL DEFAULT 0,
    status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    error_message TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_sync_jobs_integration ON sync_jobs(integration_id, started_at);
