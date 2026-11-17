-- Migration: 000013_canonical_decisions.down.sql
--
-- Refuses (NOT NULL fails) while any decision without policy provenance
-- exists: evidence is append-only and is never deleted to make a rollback fit.

DROP INDEX IF EXISTS idx_policy_evaluations_outcome;

ALTER TABLE policy_evaluations
    DROP CONSTRAINT IF EXISTS chk_policy_evaluations_envelope_signed,
    DROP CONSTRAINT IF EXISTS chk_policy_evaluations_decision_outcome,
    DROP CONSTRAINT IF EXISTS chk_policy_evaluations_provenance;

ALTER TABLE policy_evaluations
    DROP COLUMN IF EXISTS decision_envelope,
    DROP COLUMN IF EXISTS valid_until,
    DROP COLUMN IF EXISTS reason_codes,
    DROP COLUMN IF EXISTS decision_outcome,
    DROP COLUMN IF EXISTS contract_version,
    ALTER COLUMN artifact_digest SET NOT NULL,
    ALTER COLUMN policy_code SET NOT NULL,
    ALTER COLUMN policy_version_id SET NOT NULL,
    ALTER COLUMN policy_id SET NOT NULL;
