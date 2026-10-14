-- 000005_strict_tenant_isolation.up.sql
-- Strict Tenant Isolation & Durable Decision Response Fields (§12, §13.2, §18, §24)
-- 1. Remove legacy RLS bypass (tenant_id IS NULL OR ...) on privacy_decisions.
-- 2. Require tenant_id on decision rows WITHOUT deleting any evidence.
-- 3. Extend privacy_decisions with §18 fields: policy_refs, evidence_ref, expires_at.
-- 4. Ensure FORCE ROW LEVEL SECURITY is enabled on all tables.
--
-- privacy_decisions is append-only evidence (000002 trigger rejects UPDATE and
-- DELETE). This migration therefore NEVER deletes or rewrites a row and NEVER
-- invents a tenant for a legacy row:
--   * a CHECK (tenant_id IS NOT NULL) NOT VALID constraint rejects every NEW
--     row without a tenant, whatever the legacy data looks like;
--   * if no legacy NULL-tenant row exists, the column is also set NOT NULL
--     and the constraint validated;
--   * if legacy NULL-tenant rows exist they are LEFT IN PLACE. Under the strict
--     policy below they are invisible to the application role. They need an
--     authoritative tenant mapping that this service does not have, so an
--     operator must supply it (BLOCKED — DATA MIGRATION REQUIRES AUTHORITATIVE
--     TENANT MAPPING). A WARNING with the count is raised.

ALTER TABLE privacy_decisions
    DROP CONSTRAINT IF EXISTS privacy_decisions_tenant_id_required;
ALTER TABLE privacy_decisions
    ADD CONSTRAINT privacy_decisions_tenant_id_required CHECK (tenant_id IS NOT NULL) NOT VALID;

DO $$
DECLARE
    legacy_rows BIGINT;
BEGIN
    SELECT count(*) INTO legacy_rows FROM privacy_decisions WHERE tenant_id IS NULL;
    IF legacy_rows = 0 THEN
        ALTER TABLE privacy_decisions ALTER COLUMN tenant_id SET NOT NULL;
        ALTER TABLE privacy_decisions VALIDATE CONSTRAINT privacy_decisions_tenant_id_required;
    ELSE
        RAISE WARNING 'privacy_decisions: % legacy row(s) have NULL tenant_id. They were preserved (append-only evidence), are invisible under strict RLS, and require an authoritative tenant mapping supplied by an operator. tenant_id was NOT set NOT NULL; the CHECK constraint still rejects new NULL-tenant rows.', legacy_rows;
    END IF;
END $$;

-- Remove legacy RLS policy with NULL bypass and create strict tenant policy
DROP POLICY IF EXISTS tenant_isolation_policy ON privacy_decisions;
CREATE POLICY tenant_isolation_policy ON privacy_decisions
    FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

-- Add §18 response & durability fields
ALTER TABLE privacy_decisions
    ADD COLUMN IF NOT EXISTS policy_refs  JSONB       NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS evidence_ref TEXT,
    ADD COLUMN IF NOT EXISTS expires_at   TIMESTAMPTZ;

-- Re-assert FORCE ROW LEVEL SECURITY
ALTER TABLE privacy_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE privacy_decisions FORCE ROW LEVEL SECURITY;

ALTER TABLE decision_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE decision_idempotency_keys FORCE ROW LEVEL SECURITY;
