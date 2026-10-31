-- 000023_applicability_evidence.up.sql
-- ZS-JUR-001 s15 and s16 (registration-driven applicability): which obligations apply for an entity's registrations, and
-- when each is due, is recorded in the decision evidence ledger as decision_kind APPLICABILITY. Builds on 000022: every
-- earlier kind is kept. The request holds registration type codes and dates only, never a registration number, and the
-- ledger is not tenant-scoped, so no tenant or entity id is ever part of it.
--
-- The outcome (APPLICABILITY_RESOLVED) does not rest on a single pack release (the answer spans several rules and
-- carries each one's pack basis inside it), so ck_decision_records_have_basis is unchanged.

ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION', 'PARAMETER_SET', 'RETENTION', 'MAPPING', 'PROFILE',
                                                             'REGISTRATION', 'LICENCE', 'SCHEME', 'THRESHOLD', 'JURISDICTION', 'APPLICABILITY'));
