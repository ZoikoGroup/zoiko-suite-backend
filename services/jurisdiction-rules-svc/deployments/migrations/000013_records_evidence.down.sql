-- 000013 down: restores the 000012 evidence kinds for NEW rows. Evidence is append-only, so RETENTION and MAPPING
-- rows already written cannot be rewritten or removed; NOT VALID keeps them while refusing new ones.
ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT IF EXISTS ck_decision_records_have_basis,
    DROP CONSTRAINT IF EXISTS ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION', 'PARAMETER_SET')) NOT VALID;
