-- Reverts to the raw cast, which RAISES on an empty app.tenant_id rather than
-- filtering. Down only for completeness; rolling this back reintroduces the
-- intermittent error described in the up migration.

DROP POLICY IF EXISTS tenant_isolation_policy ON customer_invoices;
CREATE POLICY tenant_isolation_policy ON customer_invoices
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);
