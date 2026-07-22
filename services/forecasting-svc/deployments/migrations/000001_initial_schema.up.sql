-- Schema for forecasting-svc
CREATE TABLE IF NOT EXISTS forecast_models (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    model_name VARCHAR(128) NOT NULL,
    domain VARCHAR(64) NOT NULL, -- FINANCIAL, PAYROLL, CASH_FLOW, WORKFORCE, TAX_LIABILITY
    scenario_type VARCHAR(64) NOT NULL, -- BASELINE, OPTIMISTIC, PESSIMISTIC
    algorithm_type VARCHAR(64) NOT NULL, -- LINEAR_TREND, EXPONENTIAL_SMOOTHING, MOVING_AVERAGE, SEASONAL_ADJUSTED
    granularity VARCHAR(32) NOT NULL, -- DAILY, WEEKLY, MONTHLY, QUARTERLY, ANNUAL
    horizon_periods INT NOT NULL DEFAULT 12,
    historical_start_date DATE NOT NULL,
    historical_end_date DATE NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE', -- ACTIVE, ARCHIVED, SUPERSEDED
    confidence_level NUMERIC(5,2) NOT NULL DEFAULT 95.00,
    metadata JSONB DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS forecast_projections (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    forecast_model_id UUID NOT NULL REFERENCES forecast_models(id) ON DELETE CASCADE,
    period_index INT NOT NULL,
    period_start_date DATE NOT NULL,
    period_end_date DATE NOT NULL,
    projected_amount NUMERIC(18,4) NOT NULL,
    confidence_low NUMERIC(18,4) NOT NULL,
    confidence_high NUMERIC(18,4) NOT NULL,
    variance_margin NUMERIC(5,2) NOT NULL DEFAULT 5.00,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Enable RLS
ALTER TABLE forecast_models ENABLE ROW LEVEL SECURITY;
ALTER TABLE forecast_projections ENABLE ROW LEVEL SECURITY;

-- Drop policies if exist to ensure clean setup
DROP POLICY IF EXISTS forecast_models_tenant_isolation ON forecast_models;
DROP POLICY IF EXISTS forecast_projections_tenant_isolation ON forecast_projections;

-- Create Tenant RLS policies
CREATE POLICY forecast_models_tenant_isolation ON forecast_models
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY forecast_projections_tenant_isolation ON forecast_projections
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_forecast_models_tenant_entity ON forecast_models(tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_forecast_models_domain ON forecast_models(tenant_id, domain, scenario_type);
CREATE INDEX IF NOT EXISTS idx_forecast_projections_model ON forecast_projections(tenant_id, forecast_model_id, period_index);
