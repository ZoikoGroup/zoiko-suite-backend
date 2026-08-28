-- 000003_add_rls.up.sql
-- Row-level security for privacy-purpose-registry-svc.
--
-- purposes and processing_activities both have a NULLABLE tenant_id where
-- NULL means "applies platform-wide" (Zoiko acting as an independent
-- controller, §23.1) rather than one tenant's own processing —
-- same nullable-scope doctrine as retention-registry-svc's
-- retention_policies/legal_holds, and the same reasoning against
-- tightening it: a platform-wide purpose or activity hidden from
-- tenant-scoped callers is not hardening, it is a silent correctness
-- failure (a tenant would see PRV-001 PURPOSE_NOT_REGISTERED for a
-- purpose that genuinely exists and is published, just platform-wide).
--
-- purpose_versions/processing_activity_versions have no tenant_id column
-- of their own — their RLS policy joins back to the parent
-- purposes/processing_activities row, so tenant scoping is enforced
-- exactly once, at the stable-identity table, and versions inherit it.
--
-- tenant_id is compared as ::text against the GUC rather than casting the
-- GUC to uuid, for the same reason as every other RLS policy in this
-- platform: app.tenant_id is legitimately the empty string for a
-- platform-level caller, and ''::uuid raises invalid input syntax.

ALTER TABLE purposes ENABLE ROW LEVEL SECURITY;
ALTER TABLE purposes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purposes
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    );

ALTER TABLE purpose_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE purpose_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purpose_versions
    FOR ALL
    USING (
        EXISTS (
            SELECT 1 FROM purposes p
            WHERE p.purpose_id = purpose_versions.purpose_id
              AND (p.tenant_id IS NULL OR p.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM purposes p
            WHERE p.purpose_id = purpose_versions.purpose_id
              AND (p.tenant_id IS NULL OR p.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        )
    );

ALTER TABLE processing_activities ENABLE ROW LEVEL SECURITY;
ALTER TABLE processing_activities FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON processing_activities
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    );

ALTER TABLE processing_activity_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE processing_activity_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON processing_activity_versions
    FOR ALL
    USING (
        EXISTS (
            SELECT 1 FROM processing_activities a
            WHERE a.activity_id = processing_activity_versions.activity_id
              AND (a.tenant_id IS NULL OR a.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM processing_activities a
            WHERE a.activity_id = processing_activity_versions.activity_id
              AND (a.tenant_id IS NULL OR a.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        )
    );
