-- 000004_outbox_and_strict_tenancy.down.sql
DROP POLICY IF EXISTS tenant_isolation ON recovery_commitments;
CREATE POLICY tenant_isolation ON recovery_commitments
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON recovery_applications;
CREATE POLICY tenant_isolation ON recovery_applications
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON supplier_recovery_cases;
CREATE POLICY tenant_isolation ON supplier_recovery_cases
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP TABLE IF EXISTS outbox_events;
