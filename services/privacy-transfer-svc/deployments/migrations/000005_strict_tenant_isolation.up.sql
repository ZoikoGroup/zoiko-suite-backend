-- 000005_strict_tenant_isolation.up.sql
-- Removes the `tenant_id IS NULL OR ...` exception from every row-level-security
-- policy (000003). Under the old policy a row with no tenant was visible to
-- EVERY tenant and writable by every tenant.
--
-- NO ROW IS DELETED OR REWRITTEN (decisions, assessments, mechanisms and
-- subprocessors are append-only evidence, and no tenant is guessed). Legacy
-- rows that have no tenant_id are left in place: under the strict policy they
-- are invisible to the application role, a WARNING with the count is raised,
-- and they need an operator-assigned owner (BLOCKED — DATA MIGRATION REQUIRES
-- AUTHORITATIVE TENANT MAPPING). New rows without a tenant are refused by a
-- CHECK constraint.

DO $$
DECLARE
    t text;
    n bigint;
BEGIN
    FOREACH t IN ARRAY ARRAY['processor_relationships', 'subprocessors', 'transfer_mechanisms', 'transfer_assessments', 'transfer_decisions'] LOOP
        EXECUTE format('SELECT count(*) FROM %I WHERE tenant_id IS NULL', t) INTO n;
        IF n > 0 THEN
            RAISE WARNING 'strict tenant isolation: % row(s) in % have no tenant_id and are now invisible to the application role; they need an operator-assigned owner', n, t;
        END IF;

        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I', t, t || '_tenant_assigned');
        EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK (tenant_id IS NOT NULL) NOT VALID', t, t || '_tenant_assigned');
        IF n = 0 THEN
            EXECUTE format('ALTER TABLE %I VALIDATE CONSTRAINT %I', t, t || '_tenant_assigned');
            EXECUTE format('ALTER TABLE %I ALTER COLUMN tenant_id SET NOT NULL', t);
        END IF;

        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$
            CREATE POLICY tenant_isolation_policy ON %I
                FOR ALL
                USING (tenant_id IS NOT NULL AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
                WITH CHECK (tenant_id IS NOT NULL AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        $p$, t);
    END LOOP;
END $$;
