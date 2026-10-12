-- Migration: 000007_add_tenant_to_replay_manifests.down.sql
-- Drops all objects created by 000007_add_tenant_to_replay_manifests.up.sql.

DROP POLICY IF EXISTS tenant_isolation_policy ON replay_manifests;
ALTER TABLE replay_manifests DISABLE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS idx_replay_manifests_tenant_decision;
ALTER TABLE replay_manifests DROP COLUMN IF EXISTS tenant_id;