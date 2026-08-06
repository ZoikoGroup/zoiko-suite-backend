-- Initial schema for Spend Controls Service (spend-controls-svc)

CREATE TABLE IF NOT EXISTS spend_policies (
    spend_policy_id         UUID PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,
    category                VARCHAR(100) NOT NULL,
    period                  VARCHAR(20) NOT NULL, -- 'PER_TRANSACTION', 'MONTHLY', 'ANNUAL'
    threshold_amount        NUMERIC(18,4) NOT NULL CHECK (threshold_amount > 0),
    currency_code           VARCHAR(10) NOT NULL,
    active_flag             BOOLEAN NOT NULL DEFAULT TRUE,
    created_by_principal_id VARCHAR(255) NOT NULL,
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at              TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS spend_consumptions (
    consumption_id           UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    legal_entity_id          VARCHAR(255) NOT NULL,
    spend_policy_id          UUID NOT NULL REFERENCES spend_policies(spend_policy_id) ON DELETE CASCADE,
    amount                   NUMERIC(18,4) NOT NULL CHECK (amount > 0),
    currency_code            VARCHAR(10) NOT NULL,
    source_reference         VARCHAR(255),
    correlation_id           VARCHAR(255) NOT NULL,
    decision_outcome         VARCHAR(20) NOT NULL, -- 'ALLOWED', 'BLOCKED'
    recorded_by_principal_id VARCHAR(255) NOT NULL,
    recorded_at              TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE spend_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE spend_consumptions ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_policy ON spend_policies FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_policy ON spend_consumptions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Idempotency: a retried spend-check with the same (tenant_id, correlation_id)
-- must resolve to the original consumption record, never a duplicate.
CREATE UNIQUE INDEX idx_spend_consumptions_tenant_correlation ON spend_consumptions (tenant_id, correlation_id);

-- Performance Indexes
CREATE INDEX idx_spend_policies_tenant_entity_category ON spend_policies (tenant_id, legal_entity_id, category);
CREATE INDEX idx_spend_consumptions_tenant_policy ON spend_consumptions (tenant_id, spend_policy_id, recorded_at);
