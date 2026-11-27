BEGIN;
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS idempotency_keys;
DROP TRIGGER IF EXISTS trg_recommendation_dispositions_append_only ON recommendation_dispositions;
DROP FUNCTION IF EXISTS recommendation_dispositions_append_only();
DROP TABLE IF EXISTS recommendation_dispositions;
DROP INDEX IF EXISTS idx_recommendations_tenant_page;
DROP INDEX IF EXISTS idx_recommendations_tenant_status;
ALTER TABLE recommendations
    DROP CONSTRAINT IF EXISTS recommendations_status_check,
    DROP CONSTRAINT IF EXISTS recommendations_oversight_class_check,
    DROP CONSTRAINT IF EXISTS recommendations_decision_recorded_check,
    DROP COLUMN IF EXISTS status, DROP COLUMN IF EXISTS version, DROP COLUMN IF EXISTS oversight_class,
    DROP COLUMN IF EXISTS recommendation_source, DROP COLUMN IF EXISTS assurance_state,
    DROP COLUMN IF EXISTS model_id, DROP COLUMN IF EXISTS model_version, DROP COLUMN IF EXISTS prompt_version,
    DROP COLUMN IF EXISTS history_status, DROP COLUMN IF EXISTS outcome_distribution, DROP COLUMN IF EXISTS evidence_refs,
    DROP COLUMN IF EXISTS expires_at, DROP COLUMN IF EXISTS decided_by_principal_id, DROP COLUMN IF EXISTS decided_at,
    DROP COLUMN IF EXISTS decision_reason, DROP COLUMN IF EXISTS downstream_action_ref, DROP COLUMN IF EXISTS approval_reference,
    DROP COLUMN IF EXISTS superseded_by_recommendation_id, DROP COLUMN IF EXISTS supersedes_recommendation_id,
    DROP COLUMN IF EXISTS updated_at;
COMMIT;
