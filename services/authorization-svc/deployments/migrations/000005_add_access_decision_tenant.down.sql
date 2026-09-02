-- Migration: 000005_add_access_decision_tenant.down.sql
--
-- Dropping the column drops the only tenant scope GET /v1/access-decisions/{id}
-- has. The handler's authentication check survives a rollback, but its tenant
-- predicate does not compile against a table without the column, so this is a
-- schema rollback only — roll the service back with it.

DROP POLICY IF EXISTS tenant_isolation_policy ON access_decision_log;
ALTER TABLE access_decision_log NO FORCE ROW LEVEL SECURITY;
ALTER TABLE access_decision_log DISABLE ROW LEVEL SECURITY;

DROP INDEX IF EXISTS idx_access_decision_log_tenant;

ALTER TABLE access_decision_log DROP COLUMN IF EXISTS tenant_id;
