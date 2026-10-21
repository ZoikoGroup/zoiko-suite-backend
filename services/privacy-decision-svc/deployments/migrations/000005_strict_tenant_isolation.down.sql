-- 000005_strict_tenant_isolation.down.sql
ALTER TABLE privacy_decisions
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS evidence_ref,
    DROP COLUMN IF EXISTS policy_refs;

DROP POLICY IF EXISTS tenant_isolation_policy ON privacy_decisions;
CREATE POLICY tenant_isolation_policy ON privacy_decisions
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE privacy_decisions
    DROP CONSTRAINT IF EXISTS privacy_decisions_tenant_id_required;
ALTER TABLE privacy_decisions
    ALTER COLUMN tenant_id DROP NOT NULL;
