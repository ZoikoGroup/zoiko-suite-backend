-- 000005_add_rls.down.sql
--
-- Drops in the reverse order of the up migration: derived tables before
-- their parents. The order does not matter to Postgres here (dropping a
-- policy never cascades), but it keeps the pairing readable against the up
-- file.
--
-- Note what rolling this back means, per the caveat in the up migration:
-- dropping commercial_accounts' policy alone would widen all seven derived
-- tables, because their policies resolve through it. This file drops
-- everything, so that asymmetry does not arise — but a partial hand-rollback
-- would hit exactly that.

DROP POLICY IF EXISTS tenant_isolation_policy ON subscription_status_events;
ALTER TABLE subscription_status_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE subscription_status_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON subscription_change_requests;
ALTER TABLE subscription_change_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE subscription_change_requests DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON commercial_usage_meter_events;
ALTER TABLE commercial_usage_meter_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE commercial_usage_meter_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON evaluation_programs;
ALTER TABLE evaluation_programs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE evaluation_programs DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON billing_source_transfers;
ALTER TABLE billing_source_transfers NO FORCE ROW LEVEL SECURITY;
ALTER TABLE billing_source_transfers DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON contract_entitlement_overlays;
ALTER TABLE contract_entitlement_overlays NO FORCE ROW LEVEL SECURITY;
ALTER TABLE contract_entitlement_overlays DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON commercial_subscriptions;
ALTER TABLE commercial_subscriptions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE commercial_subscriptions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON memberships;
ALTER TABLE memberships NO FORCE ROW LEVEL SECURITY;
ALTER TABLE memberships DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON commercial_accounts;
ALTER TABLE commercial_accounts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE commercial_accounts DISABLE ROW LEVEL SECURITY;

DROP INDEX IF EXISTS idx_billing_source_transfers_account;
DROP INDEX IF EXISTS idx_contract_overlays_account;
DROP INDEX IF EXISTS idx_commercial_subscriptions_account;
