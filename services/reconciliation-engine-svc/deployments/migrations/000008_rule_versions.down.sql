-- Migration: 000008_rule_versions.down.sql
--
-- Restores the 000007 schema. Rule versions are approval evidence, and every
-- definition created under 000008 has its rules only there, so the down
-- migration refuses once any rule version exists.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_rule_versions) THEN
        RAISE EXCEPTION 'cannot downgrade below 000008: rule versions exist and have no 000007 representation';
    END IF;
END $$;

CREATE OR REPLACE FUNCTION enforce_run_governance_pinned()
RETURNS trigger AS $$
BEGIN
    IF NEW.materiality_policy IS DISTINCT FROM OLD.materiality_policy
        OR NEW.exception_policy IS DISTINCT FROM OLD.exception_policy
        OR NEW.certification_profile IS DISTINCT FROM OLD.certification_profile
    THEN
        RAISE EXCEPTION 'reconciliation run % cannot change its pinned scope or rules', OLD.run_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE reconciliation_runs DROP CONSTRAINT IF EXISTS reconciliation_runs_rules_digest_check;
ALTER TABLE reconciliation_runs DROP COLUMN IF EXISTS rules_sha256;

CREATE OR REPLACE FUNCTION enforce_definition_versioning()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation definitions cannot be deleted';
    END IF;
    IF NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id THEN
        RAISE EXCEPTION 'reconciliation definition % cannot change legal_entity_id', OLD.definition_id;
    END IF;
    IF (NEW.match_key_fields, NEW.tolerance_amount_minor_units, NEW.materiality_amount_minor_units,
        NEW.amount_scale, NEW.populations, NEW.tolerance_policy, NEW.balance_dimensions,
        NEW.materiality_policy, NEW.exception_policy, NEW.certification_profile)
       IS DISTINCT FROM
       (OLD.match_key_fields, OLD.tolerance_amount_minor_units, OLD.materiality_amount_minor_units,
        OLD.amount_scale, OLD.populations, OLD.tolerance_policy, OLD.balance_dimensions,
        OLD.materiality_policy, OLD.exception_policy, OLD.certification_profile) THEN
        NEW.version := OLD.version + 1;
        NEW.updated_at := NOW();
    ELSE
        NEW.version := OLD.version;
        NEW.updated_at := OLD.updated_at;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE reconciliation_definitions DROP CONSTRAINT IF EXISTS reconciliation_definitions_identity_only_check;
-- With no rule version, no definition was created under 000008, so every row
-- still carries its rule columns.
ALTER TABLE reconciliation_definitions
    ALTER COLUMN match_key_fields SET NOT NULL,
    ALTER COLUMN balance_dimensions SET DEFAULT '{}',
    ALTER COLUMN balance_dimensions SET NOT NULL;
ALTER TABLE reconciliation_definitions
    ADD CONSTRAINT reconciliation_definitions_sourced_check CHECK (
        legal_entity_id IS NOT NULL
        AND amount_scale IS NOT NULL
        AND populations IS NOT NULL
        AND jsonb_typeof(populations) = 'array'
        AND jsonb_array_length(populations) = 2
    ) NOT VALID,
    ADD CONSTRAINT reconciliation_definitions_tolerance_policy_check CHECK (
        tolerance_policy IS NOT NULL AND jsonb_typeof(tolerance_policy) = 'object'
        AND tolerance_amount_minor_units IS NULL
    ) NOT VALID,
    ADD CONSTRAINT reconciliation_definitions_governance_check CHECK (
        jsonb_typeof(materiality_policy) = 'object' AND jsonb_typeof(exception_policy) = 'object'
        AND jsonb_typeof(certification_profile) = 'object' AND materiality_amount_minor_units IS NULL
    ) NOT VALID;

DROP TABLE IF EXISTS reconciliation_rule_versions;
DROP FUNCTION IF EXISTS guard_rule_version();
