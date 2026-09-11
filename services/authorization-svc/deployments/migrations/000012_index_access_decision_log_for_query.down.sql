-- Migration: 000012_index_access_decision_log_for_query.down.sql
--
-- Restores 000009's (tenant_id, decided_at DESC) index and drops the
-- outcome-aware superset. GET /v1/access-decisions keeps working after this
-- runs — an unfiltered tenant listing is still served by the restored index;
-- only the outcome-filtered query loses its index support and degrades to a
-- scan-and-filter within the tenant.

BEGIN;

CREATE INDEX idx_access_decision_log_tenant
    ON access_decision_log (tenant_id, decided_at DESC);

DROP INDEX idx_access_decision_log_tenant_outcome;

COMMIT;
