-- Migration: 000006_add_shared_secret_exceptions.down.sql
--
-- Drop the policy before the table (Postgres requires dropping dependent
-- policies first when the function survives).

DROP POLICY IF EXISTS tenant_isolation_policy ON shared_secret_exceptions;
DROP TABLE IF EXISTS shared_secret_exceptions;