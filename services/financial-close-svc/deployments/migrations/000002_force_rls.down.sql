-- Migration: 000002_force_rls.down.sql
--
-- Returns these tables to the 000001 posture: the policy applies to non-owner
-- roles only, and its write check is the implicit one.

ALTER TABLE fiscal_periods NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON fiscal_periods;
CREATE POLICY tenant_isolation_policy ON fiscal_periods
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE close_evidences NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON close_evidences;
CREATE POLICY tenant_isolation_policy ON close_evidences
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
