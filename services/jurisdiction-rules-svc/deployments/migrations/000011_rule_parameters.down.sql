-- 000011 down: removes rule parameters and restores the 000005 provenance guard.
ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT IF EXISTS ck_decision_calc_has_basis,
    DROP CONSTRAINT IF EXISTS ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION')) NOT VALID;
ALTER TABLE jurisdiction_rules DROP COLUMN IF EXISTS rule_parameters;

CREATE OR REPLACE FUNCTION jur_rule_provenance_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.rule_status <> 'DRAFT' AND (
           NEW.regime_id IS DISTINCT FROM OLD.regime_id
        OR NEW.interpretation_id IS DISTINCT FROM OLD.interpretation_id
        OR NEW.supersedes_rule_id IS DISTINCT FROM OLD.supersedes_rule_id
        OR NEW.precedence IS DISTINCT FROM OLD.precedence
        OR NEW.published_on IS DISTINCT FROM OLD.published_on) THEN
        RAISE EXCEPTION 'rule % is past DRAFT; its provenance is immutable', OLD.jurisdiction_rule_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
