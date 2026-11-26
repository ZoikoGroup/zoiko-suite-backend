-- Defence in depth: FORCE row-level security so the policies also bind the
-- table owner, not only the non-owner app role (2026-10-28 Blocks 6-10 audit:
-- RLS was ENABLEd but not FORCEd on these tables).

-- clause_versions carried tenant_id but had no RLS at all; every access already
-- runs inside a transaction that sets app.tenant_id (store.setRLS).
ALTER TABLE clause_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE clause_versions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS clause_versions_tenant_isolation ON clause_versions;
CREATE POLICY clause_versions_tenant_isolation ON clause_versions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
