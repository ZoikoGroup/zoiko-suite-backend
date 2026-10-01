-- Migration: 000007_ledger_integrity.down.sql
-- Revert ledger integrity changes

DROP TRIGGER IF EXISTS evidence_evaluations_immutable ON evidence_evaluations;
DROP FUNCTION IF EXISTS enforce_evaluation_immutability();

ALTER TABLE evidence_evaluations
    DROP COLUMN IF EXISTS requirement_ids;

ALTER TABLE evidence_requirements
    DROP CONSTRAINT IF EXISTS chk_effective_dates_order;

ALTER TABLE evidence_requirements
    DROP CONSTRAINT IF EXISTS chk_requirement_payload_is_object;