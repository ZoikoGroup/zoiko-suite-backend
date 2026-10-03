-- 000011_rule_parameters.up.sql
-- ZS-JUR-001 Wave 4 (tax half): typed calculation parameters on rule versions.
--
-- DECISION: ZS-JUR-001 governs. Rates, thresholds and rounding live in
-- released, immutable, sourced, tested rule modules (s2, s3, s11, s12), carried
-- in this NEW column. rule_payload keeps its original meaning (applicability
-- metadata only), so every existing reader and writer is unaffected.
--
-- The value is validated by the service (family TAX_RATE or TAX_BANDS,
-- decimal strings only) and, like the rest of a rule's provenance, is editable
-- only while the rule is DRAFT.

ALTER TABLE jurisdiction_rules ADD COLUMN rule_parameters JSONB;

CREATE OR REPLACE FUNCTION jur_rule_provenance_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.rule_status <> 'DRAFT' AND (
           NEW.regime_id IS DISTINCT FROM OLD.regime_id
        OR NEW.interpretation_id IS DISTINCT FROM OLD.interpretation_id
        OR NEW.supersedes_rule_id IS DISTINCT FROM OLD.supersedes_rule_id
        OR NEW.precedence IS DISTINCT FROM OLD.precedence
        OR NEW.published_on IS DISTINCT FROM OLD.published_on
        OR NEW.rule_parameters IS DISTINCT FROM OLD.rule_parameters) THEN
        RAISE EXCEPTION 'rule % is past DRAFT; its provenance and parameters are immutable', OLD.jurisdiction_rule_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Calculations share the decision evidence ledger.
ALTER TABLE rule_decision_evidence
    DROP CONSTRAINT ck_decision_kind,
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION', 'CALCULATION')),
    ADD CONSTRAINT ck_decision_calc_has_basis CHECK (
        outcome <> 'CALCULATED' OR (pack_version_id IS NOT NULL AND artifact_digest IS NOT NULL AND rule_id IS NOT NULL AND rule_content_digest IS NOT NULL));
