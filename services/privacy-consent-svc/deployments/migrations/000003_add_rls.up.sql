-- 000003_add_rls.up.sql
-- Row-level security for privacy-consent-svc — same nullable-tenant-is-
-- platform-wide convention as privacy-purpose-registry-svc and
-- retention-registry-svc.
--
-- notice_versions/presentation_receipts join back to their stable-
-- identity parent for scoping, same as PRV-01's *_versions tables.
-- consent_receipts, withdrawal_receipts and preference_assertions carry
-- their own tenant_id directly (they have no stable-identity parent row
-- to join to — each is its own append-only fact).

ALTER TABLE notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE notices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON notices
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE notice_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE notice_versions FORCE ROW LEVEL SECURITY;
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

ALTER TABLE presentation_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE presentation_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON presentation_receipts
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE consent_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE consent_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON consent_receipts
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE withdrawal_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON withdrawal_receipts
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE preference_assertions ENABLE ROW LEVEL SECURITY;
ALTER TABLE preference_assertions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON preference_assertions
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
