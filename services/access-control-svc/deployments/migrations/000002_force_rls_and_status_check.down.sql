-- Reverse 000002: back to ENABLE-without-FORCE, USING-only policies, and an
-- unconstrained status.
--
-- This restores the defects 000002 fixed. It exists because the estate's
-- convention is that every up has a down, not because rolling back is safe:
-- after this runs the table owner reads every tenant again, a caller can write
-- a row into a tenant it cannot read, and status accepts any string.

ALTER TABLE role_definitions DROP CONSTRAINT IF EXISTS role_definitions_status_check;

DROP POLICY IF EXISTS tenant_isolation_policy ON permission_bundle_defs;
CREATE POLICY tenant_isolation_policy ON permission_bundle_defs FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS tenant_isolation_policy ON role_definitions;
CREATE POLICY tenant_isolation_policy ON role_definitions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE permission_bundle_defs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE role_definitions NO FORCE ROW LEVEL SECURITY;
