-- 000012 down: restores the 000011 evidence kinds for NEW rows. Evidence is append-only, so any PARAMETER_SET rows
-- already written cannot be rewritten or removed; NOT VALID keeps them while refusing new ones.
ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT IF EXISTS ck_decision_params_have_digest,
    DROP CONSTRAINT IF EXISTS ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION')) NOT VALID;
