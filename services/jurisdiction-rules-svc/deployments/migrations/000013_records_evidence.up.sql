-- 000013_records_evidence.up.sql
-- ZS-JUR-001 Wave 6 (retention s18, mappings s19): retention determinations and
-- accounting mapping lookups share the decision evidence ledger. A determination
-- that resolves must be anchored on the pack release, artifact, rule and content
-- digest it rests on.

ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION', 'PARAMETER_SET', 'RETENTION', 'MAPPING')),
    ADD CONSTRAINT ck_decision_records_have_basis CHECK (
        outcome NOT IN ('RETENTION_RESOLVED', 'MAPPING_RESOLVED')
        OR (pack_version_id IS NOT NULL AND artifact_digest IS NOT NULL AND rule_id IS NOT NULL AND rule_content_digest IS NOT NULL));
