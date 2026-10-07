-- Rollback for reconciliation-engine-svc (DATA-07) initial schema.

DROP TRIGGER IF EXISTS trigger_exception_resolution ON reconciliation_exceptions;
DROP FUNCTION IF EXISTS enforce_exception_resolution();

DROP TRIGGER IF EXISTS trigger_certifications_immutable ON certifications;
DROP TRIGGER IF EXISTS trigger_match_results_immutable ON match_results;
DROP TRIGGER IF EXISTS trigger_population_items_immutable ON population_items;
DROP TRIGGER IF EXISTS trigger_population_snapshots_immutable ON population_snapshots;
DROP FUNCTION IF EXISTS reject_immutable_row();

DROP TRIGGER IF EXISTS trigger_run_lifecycle ON reconciliation_runs;
DROP FUNCTION IF EXISTS enforce_run_lifecycle();

DROP TRIGGER IF EXISTS trigger_definition_versioning ON reconciliation_definitions;
DROP FUNCTION IF EXISTS enforce_definition_versioning();

DROP TABLE IF EXISTS outbox_events CASCADE;
DROP TABLE IF EXISTS idempotency_keys CASCADE;
DROP TABLE IF EXISTS certifications CASCADE;
DROP TABLE IF EXISTS reconciliation_exceptions CASCADE;
DROP TABLE IF EXISTS match_results CASCADE;
DROP TABLE IF EXISTS population_items CASCADE;
DROP TABLE IF EXISTS population_snapshots CASCADE;
DROP TABLE IF EXISTS reconciliation_runs CASCADE;
DROP TABLE IF EXISTS reconciliation_definitions CASCADE;
