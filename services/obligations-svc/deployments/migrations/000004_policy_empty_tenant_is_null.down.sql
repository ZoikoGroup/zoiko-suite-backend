-- Reverts to the raw cast, which RAISES on an empty app.tenant_id rather than
-- filtering. Down only for completeness; rolling this back reintroduces the
-- intermittent error described in the up migration.

DROP POLICY IF EXISTS tenant_isolation_policy ON obligations;
CREATE POLICY tenant_isolation_policy ON obligations
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_policy ON filing_requirements;
CREATE POLICY tenant_isolation_policy ON filing_requirements
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
