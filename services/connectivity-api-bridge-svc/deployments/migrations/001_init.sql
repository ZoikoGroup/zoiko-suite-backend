CREATE TABLE IF NOT EXISTS api_bridges (
    bridge_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    bridge_name VARCHAR(255) NOT NULL,
    protocol VARCHAR(64) NOT NULL,
    endpoint_url VARCHAR(512) NOT NULL,
    auth_type VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_api_bridges_tenant_entity ON api_bridges(tenant_id, legal_entity_id);

CREATE TABLE IF NOT EXISTS ingestion_logs (
    log_id VARCHAR(64) PRIMARY KEY,
    bridge_id VARCHAR(64) NOT NULL REFERENCES api_bridges(bridge_id) ON DELETE CASCADE,
    tenant_id VARCHAR(64) NOT NULL,
    payload_summary TEXT NOT NULL,
    ingestion_status VARCHAR(32) NOT NULL,
    error_message TEXT,
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_ingestion_logs_bridge ON ingestion_logs(bridge_id, ingested_at);
