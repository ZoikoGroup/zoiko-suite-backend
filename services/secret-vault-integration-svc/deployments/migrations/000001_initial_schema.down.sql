-- 000001_initial_schema.down.sql
-- Drops all objects created by 000001_initial_schema.up.sql in reverse order.

DROP TABLE IF EXISTS secret_access_audit_log;
DROP TABLE IF EXISTS secret_leases;
DROP TABLE IF EXISTS secret_policy_versions;
DROP TABLE IF EXISTS secret_policies;
