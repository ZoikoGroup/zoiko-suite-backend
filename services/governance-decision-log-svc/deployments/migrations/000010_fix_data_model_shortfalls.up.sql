-- Migration: 000010_fix_data_model_shortfalls.up.sql
--
-- Fixes data-model shortfalls from Doc 04 §7.1:
-- 1. Adds action_subject_type and action_subject_id columns
-- 2. Adds policy_version_id column (promoted from rule_basis string)
-- 3. Adds bounds/validation for decided_at (not in the past beyond retention window, not in future)
-- 4. Adds check constraints for data integrity

-- Add action_subject_type and action_subject_id columns
ALTER TABLE governance_decisions
    ADD COLUMN action_subject_type VARCHAR(64),
    ADD COLUMN action_subject_id   VARCHAR(64);

-- Add policy_version_id column
ALTER TABLE governance_decisions
    ADD COLUMN policy_version_id VARCHAR(64);

-- Create indexes for the new columns
CREATE INDEX IF NOT EXISTS idx_governance_decisions_action_subject
    ON governance_decisions (action_subject_type, action_subject_id)
    WHERE action_subject_type IS NOT NULL AND action_subject_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_governance_decisions_policy_version
    ON governance_decisions (policy_version_id)
    WHERE policy_version_id IS NOT NULL;

-- Add check constraint for decided_at bounds:
-- - not more than 1 day in the future (clock skew allowance)
-- - not before 2020-01-01 (arbitrary floor, adjust per retention policy)
ALTER TABLE governance_decisions
    ADD CONSTRAINT chk_decided_at_bounds
    CHECK (decided_at <= NOW() + INTERVAL '1 day'
           AND decided_at >= '2020-01-01'::TIMESTAMPTZ);