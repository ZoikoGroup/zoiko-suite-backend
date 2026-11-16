-- Migration: 000010_rls_relay_tables.down.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON policy_activation_schedule;
ALTER TABLE policy_activation_schedule NO FORCE ROW LEVEL SECURITY;
ALTER TABLE policy_activation_schedule DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON policy_evidence_outbox;
ALTER TABLE policy_evidence_outbox NO FORCE ROW LEVEL SECURITY;
ALTER TABLE policy_evidence_outbox DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON outbox;
ALTER TABLE outbox NO FORCE ROW LEVEL SECURITY;
ALTER TABLE outbox DISABLE ROW LEVEL SECURITY;
