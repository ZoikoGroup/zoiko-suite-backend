-- Migration: 000002_add_tenant_id_to_junction_tables.down.sql
--
-- Reverses migration 000002.
-- Restores the original correlated-subquery RLS policies and drops the
-- tenant_id columns from entity_jurisdiction_assignments and tax_identity_bundles.

-- ── Restore original RLS policies ───────────────────────────────────────────

DROP POLICY IF EXISTS tenant_isolation_policy ON entity_jurisdiction_assignments;
CREATE POLICY tenant_isolation_policy ON entity_jurisdiction_assignments
    FOR ALL USING (
        legal_entity_id IN (
            SELECT legal_entity_id FROM legal_entities
            WHERE tenant_id = current_setting('app.tenant_id')::UUID
        )
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON tax_identity_bundles;
CREATE POLICY tenant_isolation_policy ON tax_identity_bundles
    FOR ALL USING (
        legal_entity_id IN (
            SELECT legal_entity_id FROM legal_entities
            WHERE tenant_id = current_setting('app.tenant_id')::UUID
        )
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON tenants;
CREATE POLICY tenant_isolation_policy ON tenants
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON data_residency_policies;
CREATE POLICY tenant_isolation_policy ON data_residency_policies
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON legal_entities;
CREATE POLICY tenant_isolation_policy ON legal_entities
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON entity_hierarchies;
CREATE POLICY tenant_isolation_policy ON entity_hierarchies
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

-- ── Drop tenant_id columns ───────────────────────────────────────────────────

ALTER TABLE entity_jurisdiction_assignments DROP COLUMN IF EXISTS tenant_id;
ALTER TABLE tax_identity_bundles DROP COLUMN IF EXISTS tenant_id;
