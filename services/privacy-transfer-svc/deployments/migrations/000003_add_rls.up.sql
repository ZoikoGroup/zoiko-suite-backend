-- 000003_add_rls.up.sql
-- Row-level security for privacy-transfer-svc — same nullable-tenant-is-
-- platform-wide convention as every other privacy-domain service.
ALTER TABLE processor_relationships ENABLE ROW LEVEL SECURITY;
ALTER TABLE processor_relationships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON processor_relationships
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE subprocessors ENABLE ROW LEVEL SECURITY;
ALTER TABLE subprocessors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subprocessors
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE transfer_mechanisms ENABLE ROW LEVEL SECURITY;
ALTER TABLE transfer_mechanisms FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON transfer_mechanisms
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE transfer_assessments ENABLE ROW LEVEL SECURITY;
ALTER TABLE transfer_assessments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON transfer_assessments
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE transfer_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE transfer_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON transfer_decisions
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
