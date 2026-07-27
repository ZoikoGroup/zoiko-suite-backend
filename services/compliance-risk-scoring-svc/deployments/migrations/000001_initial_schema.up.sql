-- Schema for compliance-risk-scoring-svc
CREATE TABLE IF NOT EXISTS risk_score_assessments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    assessment_name VARCHAR(128) NOT NULL,
    composite_risk_score NUMERIC(5,2) NOT NULL, -- 0.00 to 100.00
    risk_tier VARCHAR(32) NOT NULL, -- LOW, MODERATE, HIGH, CRITICAL
    open_obligations_count INT NOT NULL DEFAULT 0,
    policy_violations_count INT NOT NULL DEFAULT 0,
    audit_exceptions_count INT NOT NULL DEFAULT 0,
    privacy_incidents_count INT NOT NULL DEFAULT 0,
    tax_penalties_count INT NOT NULL DEFAULT 0,
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE', -- ACTIVE, ARCHIVED
    evaluated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS risk_factor_breakdowns (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    assessment_id UUID NOT NULL REFERENCES risk_score_assessments(id) ON DELETE CASCADE,
    risk_category VARCHAR(64) NOT NULL, -- REGULATORY_OBLIGATIONS, POLICY_VIOLATIONS, AUDIT_EXCEPTIONS, DATA_PRIVACY, TAX_COMPLIANCE
    category_weight NUMERIC(5,2) NOT NULL, -- Weight e.g. 30.00%
    raw_score NUMERIC(5,2) NOT NULL, -- 0.00 to 100.00
    weighted_score NUMERIC(5,2) NOT NULL, -- raw_score * weight
    risk_driver_summary TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS risk_threshold_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    rule_name VARCHAR(128) NOT NULL,
    risk_category VARCHAR(64) NOT NULL,
    high_threshold NUMERIC(5,2) NOT NULL DEFAULT 60.00,
    critical_threshold NUMERIC(5,2) NOT NULL DEFAULT 80.00,
    notification_channel VARCHAR(64) DEFAULT 'GOVERNANCE_DESK',
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Enable RLS
ALTER TABLE risk_score_assessments ENABLE ROW LEVEL SECURITY;
ALTER TABLE risk_factor_breakdowns ENABLE ROW LEVEL SECURITY;
ALTER TABLE risk_threshold_rules ENABLE ROW LEVEL SECURITY;

-- Drop policies if exist to ensure clean setup
DROP POLICY IF EXISTS risk_score_assessments_tenant_isolation ON risk_score_assessments;
DROP POLICY IF EXISTS risk_factor_breakdowns_tenant_isolation ON risk_factor_breakdowns;
DROP POLICY IF EXISTS risk_threshold_rules_tenant_isolation ON risk_threshold_rules;

-- Create Tenant RLS policies
CREATE POLICY risk_score_assessments_tenant_isolation ON risk_score_assessments
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY risk_factor_breakdowns_tenant_isolation ON risk_factor_breakdowns
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY risk_threshold_rules_tenant_isolation ON risk_threshold_rules
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes for fast querying
CREATE INDEX IF NOT EXISTS idx_risk_assessments_tenant_entity ON risk_score_assessments(tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_risk_assessments_tier ON risk_score_assessments(tenant_id, risk_tier);
CREATE INDEX IF NOT EXISTS idx_risk_breakdowns_assessment ON risk_factor_breakdowns(tenant_id, assessment_id);
