-- 000003_add_rls.up.sql
-- Row-level security for privacy-decision-svc — same nullable-tenant-is-
-- platform-wide convention as every other privacy-domain service.
ALTER TABLE privacy_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE privacy_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON privacy_decisions
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
