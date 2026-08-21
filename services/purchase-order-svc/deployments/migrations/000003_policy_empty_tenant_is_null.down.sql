-- Reverts to the raw cast, which RAISES on an empty app.tenant_id rather than
-- filtering. Down only for completeness; rolling this back reintroduces the
-- intermittent error described in the up migration.

DROP POLICY IF EXISTS tenant_isolation_policy ON purchase_orders;
CREATE POLICY tenant_isolation_policy ON purchase_orders
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON purchase_order_amendments;
CREATE POLICY tenant_isolation_policy ON purchase_order_amendments
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);
