-- Migration: 000002_add_tenant_id_to_junction_tables.up.sql
--
-- R1: Add tenant_id column to entity_jurisdiction_assignments and
--     tax_identity_bundles, replace the correlated-subquery RLS policies
--     with direct tenant_id equality checks.
--
-- R2: Fix current_setting calls to use missing_ok=true form.
--     In Postgres, current_setting('app.tenant_id') raises an error if the
--     setting has never been set in the session. The missing_ok=true form
--     returns NULL instead, which the policy safely evaluates to FALSE
--     (NULL = anything is NULL, which is not TRUE, so no rows are returned).
--
-- Migrations are run via golang-migrate CLI in CI/CD.
-- Do NOT auto-run on service startup.

-- ── entity_jurisdiction_assignments ─────────────────────────────────────────

ALTER TABLE entity_jurisdiction_assignments
    ADD COLUMN tenant_id UUID;

-- Back-fill from the parent legal_entity.
UPDATE entity_jurisdiction_assignments eja
    SET tenant_id = le.tenant_id
    FROM legal_entities le
    WHERE le.legal_entity_id = eja.legal_entity_id;

-- Make it non-nullable now that it's populated.
ALTER TABLE entity_jurisdiction_assignments
    ALTER COLUMN tenant_id SET NOT NULL;

-- Drop the old correlated-subquery policy and replace with a direct check.
DROP POLICY IF EXISTS tenant_isolation_policy ON entity_jurisdiction_assignments;

CREATE POLICY tenant_isolation_policy ON entity_jurisdiction_assignments
    FOR ALL USING (
        tenant_id = current_setting('app.tenant_id', true)::UUID
    );

-- ── tax_identity_bundles ─────────────────────────────────────────────────────

ALTER TABLE tax_identity_bundles
    ADD COLUMN tenant_id UUID;

-- Back-fill from the parent legal_entity.
UPDATE tax_identity_bundles tib
    SET tenant_id = le.tenant_id
    FROM legal_entities le
    WHERE le.legal_entity_id = tib.legal_entity_id;

ALTER TABLE tax_identity_bundles
    ALTER COLUMN tenant_id SET NOT NULL;

DROP POLICY IF EXISTS tenant_isolation_policy ON tax_identity_bundles;

CREATE POLICY tenant_isolation_policy ON tax_identity_bundles
    FOR ALL USING (
        tenant_id = current_setting('app.tenant_id', true)::UUID
    );

-- ── Fix existing RLS policies on all other tables to use missing_ok=true ────
--    This is R2: prevents ERROR when app.tenant_id has never been set.

DROP POLICY IF EXISTS tenant_isolation_policy ON tenants;
CREATE POLICY tenant_isolation_policy ON tenants
    FOR ALL USING (
        tenant_id = current_setting('app.tenant_id', true)::UUID
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON data_residency_policies;
CREATE POLICY tenant_isolation_policy ON data_residency_policies
    FOR ALL USING (
        tenant_id = current_setting('app.tenant_id', true)::UUID
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON legal_entities;
CREATE POLICY tenant_isolation_policy ON legal_entities
    FOR ALL USING (
        tenant_id = current_setting('app.tenant_id', true)::UUID
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON entity_hierarchies;
CREATE POLICY tenant_isolation_policy ON entity_hierarchies
    FOR ALL USING (
        tenant_id = current_setting('app.tenant_id', true)::UUID
    );
