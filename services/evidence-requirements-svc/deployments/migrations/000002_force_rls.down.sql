-- Migration: 000002_force_rls.down.sql
--
-- Returns these tables to the 000001 posture: the policy applies to non-owner
-- roles only, and its write check is the implicit one.

ALTER TABLE evidence_requirements NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON evidence_requirements;
CREATE POLICY tenant_isolation_policy ON evidence_requirements
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true)::UUID);

ALTER TABLE evidence_evaluations NO FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON evidence_evaluations;
CREATE POLICY tenant_isolation_policy ON evidence_evaluations
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true)::UUID);
