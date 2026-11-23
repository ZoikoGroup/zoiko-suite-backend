-- 000021 down: restores the 000014 evidence kinds for NEW rows. Evidence is append-only, so REGISTRATION, LICENCE,
-- SCHEME and THRESHOLD rows already written cannot be rewritten or removed; NOT VALID keeps them while refusing new ones.
ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT IF EXISTS ck_decision_records_have_basis,
    DROP CONSTRAINT IF EXISTS ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION', 'PARAMETER_SET', 'RETENTION', 'MAPPING', 'PROFILE')) NOT VALID,
    ADD CONSTRAINT ck_decision_records_have_basis CHECK (
        outcome NOT IN ('RETENTION_RESOLVED', 'MAPPING_RESOLVED', 'PROFILE_RESOLVED')
        OR (pack_version_id IS NOT NULL AND artifact_digest IS NOT NULL AND rule_id IS NOT NULL AND rule_content_digest IS NOT NULL)) NOT VALID;
