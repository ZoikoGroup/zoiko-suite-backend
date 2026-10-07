-- Migration: 000017_add_access_reviews_and_workload_identities.down.sql

BEGIN;

DROP POLICY IF EXISTS tenant_isolation_policy ON workload_bindings;
DROP TABLE IF EXISTS workload_bindings;

DROP POLICY IF EXISTS tenant_isolation_policy ON access_reviews;
DROP TABLE IF EXISTS access_reviews;

COMMIT;
