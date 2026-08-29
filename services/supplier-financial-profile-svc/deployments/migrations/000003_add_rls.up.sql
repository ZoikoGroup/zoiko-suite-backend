-- 000003_add_rls.up.sql
-- Row-level security for supplier-financial-profile-svc — same nullable-
-- tenant-is-platform-wide convention as every other service in this
-- platform.
ALTER TABLE supplier_financial_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE supplier_financial_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON supplier_financial_profiles
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE payment_terms_periods ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_terms_periods FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON payment_terms_periods
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE high_risk_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE high_risk_change_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON high_risk_change_requests
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE profile_change_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE profile_change_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON profile_change_events
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
