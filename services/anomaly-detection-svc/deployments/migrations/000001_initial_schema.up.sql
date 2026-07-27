-- 000001_initial_schema.up.sql
-- Anomaly Detection Service Schema with Multi-Tenant Row-Level Security (RLS)

CREATE TABLE IF NOT EXISTS anomaly_detection_rules (
    rule_id          VARCHAR(64) PRIMARY KEY,
    tenant_id        VARCHAR(64) NOT NULL,
    rule_name        VARCHAR(128) NOT NULL,
    domain_name      VARCHAR(64) NOT NULL,
    metric_type      VARCHAR(64) NOT NULL,
    threshold_value  NUMERIC(18,4) NOT NULL,
    z_score_cutoff   NUMERIC(6,2) DEFAULT 3.00,
    is_active        BOOLEAN DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS anomaly_records (
    anomaly_id       VARCHAR(64) PRIMARY KEY,
    tenant_id        VARCHAR(64) NOT NULL,
    legal_entity_id  VARCHAR(64) NOT NULL,
    domain_name      VARCHAR(64) NOT NULL,
    source_entity_id VARCHAR(128) NOT NULL,
    rule_id          VARCHAR(64) REFERENCES anomaly_detection_rules(rule_id),
    severity         VARCHAR(32) NOT NULL DEFAULT 'MEDIUM',
    anomaly_score    NUMERIC(6,2) NOT NULL,
    observed_value   NUMERIC(18,4) NOT NULL,
    expected_value   NUMERIC(18,4) NOT NULL,
    description      TEXT NOT NULL,
    status           VARCHAR(32) NOT NULL DEFAULT 'OPEN',
    investigated_by  VARCHAR(64),
    investigated_at  TIMESTAMPTZ,
    resolution_notes TEXT,
    detected_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Enable RLS
ALTER TABLE anomaly_detection_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE anomaly_records ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON anomaly_detection_rules
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_policy ON anomaly_records
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_anomaly_records_tenant_entity ON anomaly_records(tenant_id, legal_entity_id);
CREATE INDEX idx_anomaly_records_domain_status ON anomaly_records(domain_name, status);
CREATE INDEX idx_anomaly_rules_tenant_domain ON anomaly_detection_rules(tenant_id, domain_name);
