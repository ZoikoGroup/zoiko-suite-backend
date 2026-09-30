-- Migration: 000008_add_idempotency_keys.down.sql
-- Drops all objects created by 000008_add_idempotency_keys.up.sql.

DROP POLICY IF EXISTS tenant_isolation_policy ON idempotency_keys;
ALTER TABLE idempotency_keys DISABLE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS idx_idempotency_keys_decision;
DROP TABLE IF EXISTS idempotency_keys;