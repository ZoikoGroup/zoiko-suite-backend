-- 000006_org02_org03_completion.down.sql
--
-- Reverses 000006 exactly, including restoring the 000002 policy text on the
-- seven pre-existing tables. A down migration that drops the new tables but
-- leaves FORCE and the rewritten policies in place would not return the
-- database to its prior state, and the redeploy path (down then up again) is
-- the one that would discover that.
--
-- Data loss is real and unavoidable here: the profile-version history, the
-- lifecycle history and any open registry conflicts are dropped with their
-- tables. That is inherent to reversing a migration that introduced them.

DROP TABLE IF EXISTS event_outbox;
DROP TABLE IF EXISTS entity_registry_conflicts;
DROP TABLE IF EXISTS tenant_host_bindings;
DROP TABLE IF EXISTS tenant_lifecycle_history;
DROP TABLE IF EXISTS legal_entity_profile_versions;

-- Restore the 000002 policies (USING only, no explicit WITH CHECK).
DROP POLICY IF EXISTS tenant_isolation_policy ON tenants;
CREATE POLICY tenant_isolation_policy ON tenants
    FOR ALL USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON data_residency_policies;
CREATE POLICY tenant_isolation_policy ON data_residency_policies
    FOR ALL USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON legal_entities;
CREATE POLICY tenant_isolation_policy ON legal_entities
    FOR ALL USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON entity_hierarchies;
CREATE POLICY tenant_isolation_policy ON entity_hierarchies
    FOR ALL USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON entity_jurisdiction_assignments;
CREATE POLICY tenant_isolation_policy ON entity_jurisdiction_assignments
    FOR ALL USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON tax_identity_bundles;
CREATE POLICY tenant_isolation_policy ON tax_identity_bundles
    FOR ALL USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

DROP POLICY IF EXISTS tenant_isolation_policy ON workspaces;
CREATE POLICY tenant_isolation_policy ON workspaces
    FOR ALL USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE tenants                          NO FORCE ROW LEVEL SECURITY;
ALTER TABLE data_residency_policies          NO FORCE ROW LEVEL SECURITY;
ALTER TABLE legal_entities                   NO FORCE ROW LEVEL SECURITY;
ALTER TABLE entity_hierarchies               NO FORCE ROW LEVEL SECURITY;
ALTER TABLE entity_jurisdiction_assignments  NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tax_identity_bundles             NO FORCE ROW LEVEL SECURITY;
ALTER TABLE workspaces                       NO FORCE ROW LEVEL SECURITY;

ALTER TABLE legal_entities DROP COLUMN IF EXISTS record_version;
ALTER TABLE tenants        DROP COLUMN IF EXISTS record_version;
