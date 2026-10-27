-- 000005_strict_tenant_isolation.down.sql
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['processor_relationships', 'subprocessors', 'transfer_mechanisms', 'transfer_assessments', 'transfer_decisions'] LOOP
        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I', t, t || '_tenant_assigned');
        EXECUTE format('ALTER TABLE %I ALTER COLUMN tenant_id DROP NOT NULL', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$
            CREATE POLICY tenant_isolation_policy ON %I
                FOR ALL
                USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
                WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        $p$, t);
    END LOOP;
END $$;
