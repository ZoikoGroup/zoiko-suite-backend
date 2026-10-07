-- Rollback for classification-svc (AI-02) initial schema.

DROP TRIGGER IF EXISTS trigger_classification_decisions_immutable ON classification_decisions;
DROP TRIGGER IF EXISTS trigger_classification_candidates_immutable ON classification_candidates;
DROP TRIGGER IF EXISTS trigger_feature_snapshots_immutable ON feature_snapshots;
DROP FUNCTION IF EXISTS reject_immutable_row();

DROP TRIGGER IF EXISTS trigger_job_lifecycle ON classification_jobs;
DROP FUNCTION IF EXISTS enforce_job_lifecycle();

DROP TRIGGER IF EXISTS trigger_model_releases_no_delete ON model_releases;
DROP FUNCTION IF EXISTS reject_delete();

DROP TABLE IF EXISTS outbox_events CASCADE;
DROP TABLE IF EXISTS idempotency_keys CASCADE;
DROP TABLE IF EXISTS classification_decisions CASCADE;
DROP TABLE IF EXISTS classification_candidates CASCADE;
DROP TABLE IF EXISTS feature_snapshots CASCADE;
DROP TABLE IF EXISTS classification_jobs CASCADE;
DROP TABLE IF EXISTS model_releases CASCADE;
