-- Restores the pre-000012 policies (000004 / 000003 forms).
DROP POLICY IF EXISTS tenant_isolation_policy ON config_entries;
CREATE POLICY tenant_isolation_policy ON config_entries FOR ALL
    USING (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true')
    WITH CHECK (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true');
DROP POLICY IF EXISTS tenant_isolation_policy ON feature_flags;
CREATE POLICY tenant_isolation_policy ON feature_flags FOR ALL
    USING (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true')
    WITH CHECK (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true');
DROP POLICY IF EXISTS outbox_tenant_isolation ON event_outbox;
CREATE POLICY outbox_tenant_isolation ON event_outbox FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true')
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
           OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true');
