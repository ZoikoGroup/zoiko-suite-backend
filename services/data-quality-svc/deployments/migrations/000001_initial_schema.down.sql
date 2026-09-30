-- Down migration for data-quality-svc (DATA-02)
-- Migration: 000001_initial_schema.down.sql

DROP TRIGGER IF EXISTS trigger_dq_run_lifecycle ON dq_runs;
DROP FUNCTION IF EXISTS enforce_dq_run_lifecycle();

DROP TRIGGER IF EXISTS trigger_dq_issue_assign_only ON dq_issues;
DROP FUNCTION IF EXISTS enforce_dq_issue_assign_only();

DROP TRIGGER IF EXISTS trigger_dq_certification_immutability ON dq_certifications;
DROP FUNCTION IF EXISTS enforce_dq_certification_immutability();

DROP TRIGGER IF EXISTS trigger_dq_result_immutability ON dq_results;
DROP FUNCTION IF EXISTS enforce_dq_result_immutability();

DROP TRIGGER IF EXISTS trigger_dq_rule_set_version_immutability ON dq_rule_set_versions;
DROP FUNCTION IF EXISTS enforce_dq_rule_set_version_immutability();

DROP TRIGGER IF EXISTS trigger_dq_rule_set_immutability ON dq_rule_sets;
DROP FUNCTION IF EXISTS enforce_dq_rule_set_immutability();

DROP INDEX IF EXISTS idx_outbox_events_unpublished;
DROP INDEX IF EXISTS idx_dq_issues_open;
DROP INDEX IF EXISTS idx_dq_issues_run;
DROP INDEX IF EXISTS idx_dq_results_failing;
DROP INDEX IF EXISTS idx_dq_results_run;
DROP INDEX IF EXISTS idx_dq_runs_ruleset_version;
DROP INDEX IF EXISTS idx_dq_runs_tenant_status;
DROP INDEX IF EXISTS idx_dq_rule_set_versions_ruleset;

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
DROP POLICY IF EXISTS dq_certifications_delete ON dq_certifications;
DROP POLICY IF EXISTS dq_certifications_update ON dq_certifications;
DROP POLICY IF EXISTS dq_certifications_insert ON dq_certifications;
DROP POLICY IF EXISTS dq_certifications_read ON dq_certifications;
DROP POLICY IF EXISTS dq_issues_tenant_isolation ON dq_issues;
DROP POLICY IF EXISTS dq_results_delete ON dq_results;
DROP POLICY IF EXISTS dq_results_update ON dq_results;
DROP POLICY IF EXISTS dq_results_insert ON dq_results;
DROP POLICY IF EXISTS dq_results_read ON dq_results;
DROP POLICY IF EXISTS dq_runs_tenant_isolation ON dq_runs;
DROP POLICY IF EXISTS dq_rule_set_versions_delete ON dq_rule_set_versions;
DROP POLICY IF EXISTS dq_rule_set_versions_update ON dq_rule_set_versions;
DROP POLICY IF EXISTS dq_rule_set_versions_insert ON dq_rule_set_versions;
DROP POLICY IF EXISTS dq_rule_set_versions_read ON dq_rule_set_versions;
DROP POLICY IF EXISTS dq_rule_sets_tenant_isolation ON dq_rule_sets;

ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys DISABLE ROW LEVEL SECURITY;
ALTER TABLE dq_certifications DISABLE ROW LEVEL SECURITY;
ALTER TABLE dq_issues DISABLE ROW LEVEL SECURITY;
ALTER TABLE dq_results DISABLE ROW LEVEL SECURITY;
ALTER TABLE dq_runs DISABLE ROW LEVEL SECURITY;
ALTER TABLE dq_rule_set_versions DISABLE ROW LEVEL SECURITY;
ALTER TABLE dq_rule_sets DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS dq_certifications;
DROP TABLE IF EXISTS dq_issues;
DROP TABLE IF EXISTS dq_results;
DROP TABLE IF EXISTS dq_runs;
DROP TABLE IF EXISTS dq_rule_set_versions;
DROP TABLE IF EXISTS dq_rule_sets;
