-- BNK-10 FX Exposure: fx_rates is the ONLY new table. No real market-data
-- FX feed exists anywhere in this platform, so a rate is explicit,
-- versioned, caller-supplied input — the same honesty pattern as the
-- Audit domain's sample_size. Exposure recognition and scenario modeling
-- (internal/handler/bnk10_handler.go) are live compositions over the
-- existing AP/AR/obligations clients, never persisted snapshots.

CREATE TABLE fx_rates (
    rate_id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                VARCHAR(255) NOT NULL,
    currency_pair            VARCHAR(16) NOT NULL,
    rate                     NUMERIC(20, 8) NOT NULL,
    effective_at             TIMESTAMPTZ NOT NULL,
    recorded_by_principal_id VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_fx_rates_tenant_pair_effective
    ON fx_rates (tenant_id, currency_pair, effective_at DESC);

ALTER TABLE fx_rates ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_rates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fx_rates
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Append-only: a correction is a NEW rate observation with a later
-- effective_at, never an edit of a prior one — the same versioning
-- discipline the doc requires for tolerance/materiality policies
-- elsewhere in this domain.
CREATE OR REPLACE FUNCTION reject_fx_rate_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'fx_rates rows are append-only and can never be updated or deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_fx_rate_mutation
    BEFORE UPDATE OR DELETE ON fx_rates
    FOR EACH ROW EXECUTE FUNCTION reject_fx_rate_mutation();
