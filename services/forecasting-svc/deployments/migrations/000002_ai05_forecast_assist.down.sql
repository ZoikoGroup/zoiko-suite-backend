-- Rollback for the AI-05 Forecast Assist governance layer. Leaves the
-- pre-existing forecast_models/forecast_projections tables and their
-- data intact.

DROP TRIGGER IF EXISTS trigger_forecast_job_lifecycle ON forecast_assist_jobs;
DROP FUNCTION IF EXISTS enforce_forecast_job_lifecycle();

DROP TRIGGER IF EXISTS trigger_planner_decisions_immutable ON planner_decisions;
DROP TRIGGER IF EXISTS trigger_evidence_references_immutable ON evidence_references;
DROP TRIGGER IF EXISTS trigger_suggested_ranges_immutable ON suggested_ranges;
DROP TRIGGER IF EXISTS trigger_suggested_drivers_immutable ON suggested_drivers;
DROP FUNCTION IF EXISTS reject_immutable_row();

DROP TRIGGER IF EXISTS trigger_forecast_model_releases_no_delete ON forecast_model_releases;
DROP FUNCTION IF EXISTS reject_delete();

ALTER TABLE forecast_projections NO FORCE ROW LEVEL SECURITY;
ALTER TABLE forecast_models NO FORCE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS idempotency_keys CASCADE;
DROP TABLE IF EXISTS planner_decisions CASCADE;
DROP TABLE IF EXISTS evidence_references CASCADE;
DROP TABLE IF EXISTS suggested_ranges CASCADE;
DROP TABLE IF EXISTS suggested_drivers CASCADE;
DROP TABLE IF EXISTS forecast_assist_jobs CASCADE;
DROP TABLE IF EXISTS forecast_model_releases CASCADE;
