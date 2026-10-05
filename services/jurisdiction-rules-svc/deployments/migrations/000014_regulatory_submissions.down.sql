-- 000014 down: removes submissions and restores the 000013 evidence kinds for NEW rows. PROFILE evidence already
-- written is append-only and stays; NOT VALID keeps it while refusing new ones.
DROP TABLE IF EXISTS regulatory_submission_events;
DROP TABLE IF EXISTS regulatory_submissions;
DROP FUNCTION IF EXISTS jur_submission_event_append_only();
DROP FUNCTION IF EXISTS jur_submission_guard();
ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT IF EXISTS ck_decision_records_have_basis,
    DROP CONSTRAINT IF EXISTS ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION', 'PARAMETER_SET', 'RETENTION', 'MAPPING')) NOT VALID,
    ADD CONSTRAINT ck_decision_records_have_basis CHECK (
        outcome NOT IN ('RETENTION_RESOLVED', 'MAPPING_RESOLVED')
        OR (pack_version_id IS NOT NULL AND artifact_digest IS NOT NULL AND rule_id IS NOT NULL AND rule_content_digest IS NOT NULL)) NOT VALID;
