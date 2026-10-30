-- 000022_jurisdiction_decision_evidence.up.sql
-- ZS-JUR-001 s9 (jurisdiction determination): a determination is recorded in the decision evidence ledger as
-- decision_kind JURISDICTION. Builds on 000021: every earlier kind is kept. The ledger is platform-wide, not
-- tenant-scoped, so the recorded request holds a digest of tenant and entity, never their ids.
--
-- None of the determination outcomes (JURISDICTION_DETERMINED, AMBIGUOUS_JURISDICTION, NO_APPLICABLE_JURISDICTION,
-- CLAIM_NOT_AUTHORISED) rests on a pack release, so ck_decision_records_have_basis is unchanged.

ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION', 'PARAMETER_SET', 'RETENTION', 'MAPPING', 'PROFILE',
                                                             'REGISTRATION', 'LICENCE', 'SCHEME', 'THRESHOLD', 'JURISDICTION'));
