-- Migration: 000007_ledger_integrity.up.sql
-- Add immutability trigger for evidence_evaluations
-- Add requirement_ids column to track which requirements were evaluated
-- Add check constraints for effective date validity

-- 1. Immutability trigger for evidence_evaluations (prevents UPDATE/DELETE)
CREATE OR REPLACE FUNCTION enforce_evaluation_immutability()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'evidence_evaluations is append-only: UPDATE not allowed';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evidence_evaluations is append-only: DELETE not allowed';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER;

DROP TRIGGER IF EXISTS evidence_evaluations_immutable ON evidence_evaluations;
CREATE TRIGGER evidence_evaluations_immutable
    BEFORE UPDATE OR DELETE ON evidence_evaluations
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_immutability();

-- 2. Add requirement_ids column to track which requirement IDs were evaluated
ALTER TABLE evidence_evaluations
    ADD COLUMN IF NOT EXISTS requirement_ids UUID[] NOT NULL DEFAULT '{}';

-- 3. Check constraint: effective_to must be > effective_from (when both set)
ALTER TABLE evidence_requirements
    ADD CONSTRAINT chk_effective_dates_order
    CHECK (effective_to IS NULL OR effective_to > effective_from);

-- 4. Check constraint: effective_from cannot be retroactively set before created_at
-- (This is enforced at application level since created_at is set on insert)

-- 5. Check constraint: requirement_payload must be a JSON object (not array, scalar, or null)
ALTER TABLE evidence_requirements
    ADD CONSTRAINT chk_requirement_payload_is_object
    CHECK (jsonb_typeof(requirement_payload) = 'object');