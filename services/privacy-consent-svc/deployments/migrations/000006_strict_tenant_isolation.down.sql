-- 000006_strict_tenant_isolation.down.sql
-- Restores the 000003/000005 policies (including their "tenant_id IS NULL OR"
-- exception) and drops the tenant-assigned CHECKs.

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['notices', 'presentation_receipts', 'consent_receipts',
                             'withdrawal_receipts', 'preference_assertions',
                             'consent_requests', 'consent_invalidations'] LOOP
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$
            CREATE POLICY tenant_isolation_policy ON %I
                FOR ALL
                USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
                WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        $p$, t);
        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I', t, t || '_tenant_assigned');
    END LOOP;
END $$;

DROP POLICY IF EXISTS tenant_isolation_policy ON notice_versions;
CREATE POLICY tenant_isolation_policy ON notice_versions
    FOR ALL
    USING (
        EXISTS (
            SELECT 1 FROM notices n
            WHERE n.notice_id = notice_versions.notice_id
              AND (n.tenant_id IS NULL OR n.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM notices n
            WHERE n.notice_id = notice_versions.notice_id
              AND (n.tenant_id IS NULL OR n.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        )
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON event_outbox;
CREATE POLICY tenant_isolation_policy ON event_outbox
    FOR ALL
    USING (
        tenant_id = COALESCE(current_setting('app.tenant_id', true), '')
        OR current_setting('app.outbox_relay', true) = 'true'
    )
    WITH CHECK (
        tenant_id = COALESCE(current_setting('app.tenant_id', true), '')
        OR current_setting('app.outbox_relay', true) = 'true'
    );
