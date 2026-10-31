-- 000021_registration_evidence.up.sql
-- ZS-JUR-001 s16 (registration identifiers, licences, schemes and threshold monitors): these
-- determinations share the decision evidence ledger. A determination that resolves must be
-- anchored on the pack release, artifact, rule and content digest it rests on.
--
-- Builds on 000014 (which added PROFILE): every earlier kind and outcome is kept.
--
-- Nothing is stored here about an entity's actual registrations: they are tenant facts owned by
-- tenant-entity-registry-svc and the Tax domain. Evidence holds the verdict and its basis only, and
-- for an identifier check the request carries a digest of the identifier, never the identifier.

ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT ck_decision_records_have_basis,
    DROP CONSTRAINT ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION', 'PARAMETER_SET', 'RETENTION', 'MAPPING', 'PROFILE',
                                                             'REGISTRATION', 'LICENCE', 'SCHEME', 'THRESHOLD')),
    ADD CONSTRAINT ck_decision_records_have_basis CHECK (
        outcome NOT IN ('RETENTION_RESOLVED', 'MAPPING_RESOLVED', 'PROFILE_RESOLVED', 'IDENTIFIER_VALID', 'IDENTIFIER_INVALID', 'LICENCE_EVALUATED',
                        'SCHEME_ELIGIBLE', 'SCHEME_NOT_ELIGIBLE', 'THRESHOLD_EVALUATED')
        OR (pack_version_id IS NOT NULL AND artifact_digest IS NOT NULL AND rule_id IS NOT NULL AND rule_content_digest IS NOT NULL));
