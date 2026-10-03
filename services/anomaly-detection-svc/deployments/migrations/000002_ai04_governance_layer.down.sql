-- Rollback for the AI-04 governance layer. Leaves the pre-existing
-- anomaly_detection_rules/anomaly_records tables and their data intact.

DROP TRIGGER IF EXISTS trigger_signal_lifecycle ON anomaly_signals;
DROP FUNCTION IF EXISTS enforce_signal_lifecycle();

DROP TRIGGER IF EXISTS trigger_review_dispositions_immutable ON review_dispositions;
DROP TRIGGER IF EXISTS trigger_detection_runs_immutable ON detection_runs;
DROP FUNCTION IF EXISTS reject_immutable_row();

DROP TRIGGER IF EXISTS trigger_anomaly_models_no_delete ON anomaly_models;
DROP FUNCTION IF EXISTS reject_delete();

ALTER TABLE anomaly_records NO FORCE ROW LEVEL SECURITY;
ALTER TABLE anomaly_detection_rules NO FORCE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS idempotency_keys CASCADE;
DROP TABLE IF EXISTS review_dispositions CASCADE;
DROP TABLE IF EXISTS anomaly_signals CASCADE;
DROP TABLE IF EXISTS detection_runs CASCADE;
DROP TABLE IF EXISTS anomaly_models CASCADE;
