-- 000002_add_rls.down.sql
--
-- Note the asymmetry recorded in the up migration: rolling this back
-- REMOVES a tenant boundary but does not affect whether kill switches
-- resolve correctly, because ResolveKillSwitch's own
-- "tenant_id IS NULL OR tenant_id = $4" predicate is independent of the
-- policy. Tightening the policy is the dangerous direction here, not
-- removing it.
DROP POLICY IF EXISTS tenant_isolation_policy ON kill_switch_events;
ALTER TABLE kill_switch_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE kill_switch_events DISABLE ROW LEVEL SECURITY;
