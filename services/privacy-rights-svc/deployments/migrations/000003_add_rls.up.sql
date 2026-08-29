-- 000003_add_rls.up.sql
-- Row-level security for privacy-rights-svc — same nullable-tenant-is-
-- platform-wide convention as every other privacy-domain service.
ALTER TABLE rights_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE rights_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON rights_requests
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE identity_verification_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_verification_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON identity_verification_events
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE discovery_manifests ENABLE ROW LEVEL SECURITY;
ALTER TABLE discovery_manifests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON discovery_manifests
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
