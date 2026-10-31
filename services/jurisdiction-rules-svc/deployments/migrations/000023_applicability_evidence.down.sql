-- 000023 down: restores the 000022 evidence kinds for NEW rows. Evidence is append-only, so APPLICABILITY rows already
-- written cannot be rewritten or removed; NOT VALID keeps them while refusing new ones.
ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT IF EXISTS ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION', 'PARAMETER_SET', 'RETENTION', 'MAPPING', 'PROFILE',
                                                             'REGISTRATION', 'LICENCE', 'SCHEME', 'THRESHOLD', 'JURISDICTION')) NOT VALID;
