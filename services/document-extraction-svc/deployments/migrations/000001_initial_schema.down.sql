-- Rollback for document-extraction-svc (AI-01) initial schema.

DROP TRIGGER IF EXISTS trigger_candidate_decision ON extraction_candidates;
DROP FUNCTION IF EXISTS enforce_candidate_decision();

DROP TRIGGER IF EXISTS trigger_extraction_decisions_immutable ON extraction_decisions;
DROP TRIGGER IF EXISTS trigger_evidence_spans_immutable ON evidence_spans;
DROP TRIGGER IF EXISTS trigger_model_invocations_immutable ON model_invocations;
DROP FUNCTION IF EXISTS reject_immutable_row();

DROP TRIGGER IF EXISTS trigger_job_lifecycle ON extraction_jobs;
DROP FUNCTION IF EXISTS enforce_job_lifecycle();

DROP TABLE IF EXISTS outbox_events CASCADE;
DROP TABLE IF EXISTS idempotency_keys CASCADE;
DROP TABLE IF EXISTS extraction_decisions CASCADE;
DROP TABLE IF EXISTS evidence_spans CASCADE;
DROP TABLE IF EXISTS extraction_candidates CASCADE;
DROP TABLE IF EXISTS model_invocations CASCADE;
DROP TABLE IF EXISTS extraction_jobs CASCADE;
