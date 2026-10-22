-- 000012_completion.down.sql
BEGIN;

ALTER TABLE obligations DROP CONSTRAINT IF EXISTS chk_obligations_definition_pinned;
ALTER TABLE obligations DROP COLUMN IF EXISTS definition_version;
ALTER TABLE obligations DROP COLUMN IF EXISTS obligation_definition_id;

DROP TABLE IF EXISTS obligation_entity_mappings;
DROP TABLE IF EXISTS obligation_definition_versions;
DROP TABLE IF EXISTS obligation_definitions;
DROP TABLE IF EXISTS filing_requirement_transitions;
DROP FUNCTION IF EXISTS obligations_refuse_mutation();

ALTER TABLE filing_requirements DROP CONSTRAINT IF EXISTS chk_filing_requirements_status;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS row_version;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS updated_at;

DROP INDEX IF EXISTS uq_applicability_decisions_idempotency;
ALTER TABLE applicability_decisions DROP COLUMN IF EXISTS request_hash;
ALTER TABLE applicability_decisions DROP COLUMN IF EXISTS idempotency_key;

DROP POLICY IF EXISTS overdue_sweep_select ON obligations;

DROP INDEX IF EXISTS idx_event_outbox_due;
ALTER TABLE event_outbox DROP COLUMN IF EXISTS failed_at;
ALTER TABLE event_outbox DROP COLUMN IF EXISTS next_attempt_at;

COMMIT;
