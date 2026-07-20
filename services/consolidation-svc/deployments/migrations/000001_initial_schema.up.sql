-- Initial schema for Consolidation Service (consolidation-svc)

CREATE TABLE IF NOT EXISTS consolidation_runs (
    consolidation_run_id   UUID PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    group_legal_entity_id  VARCHAR(255) NOT NULL,
    fiscal_period          VARCHAR(50) NOT NULL,
    target_currency        VARCHAR(10) NOT NULL,
    status                 VARCHAR(50) NOT NULL, -- 'RUNNING', 'COMPLETED', 'FAILED'
    exception_count        INT NOT NULL DEFAULT 0,
    started_at             TIMESTAMP WITH TIME ZONE NOT NULL,
    completed_at           TIMESTAMP WITH TIME ZONE
);

CREATE TABLE IF NOT EXISTS balance_snapshots (
    balance_snapshot_id    UUID PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    consolidation_run_id   UUID NOT NULL REFERENCES consolidation_runs(consolidation_run_id) ON DELETE CASCADE,
    legal_entity_id        VARCHAR(255) NOT NULL,
    fiscal_period          VARCHAR(50) NOT NULL,
    account_code           VARCHAR(100) NOT NULL,
    consolidated_balance   NUMERIC(18,4) NOT NULL,
    currency_code          VARCHAR(10) NOT NULL,
    snapshot_signature     VARCHAR(255) NOT NULL,
    generated_at           TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE consolidation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE balance_snapshots ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_policy ON consolidation_runs FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_policy ON balance_snapshots FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance Indexes
CREATE INDEX idx_consolidation_runs_tenant_group ON consolidation_runs (tenant_id, group_legal_entity_id, fiscal_period);
CREATE INDEX idx_balance_snapshots_run ON balance_snapshots (consolidation_run_id);
CREATE INDEX idx_balance_snapshots_tenant_entity ON balance_snapshots (tenant_id, legal_entity_id, fiscal_period);