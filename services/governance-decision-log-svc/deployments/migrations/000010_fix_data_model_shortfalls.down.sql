-- Migration: 000010_fix_data_model_shortfalls.down.sql
-- Drops all objects created by 000010_fix_data_model_shortfalls.up.sql.

ALTER TABLE governance_decisions DROP CONSTRAINT IF EXISTS chk_decided_at_bounds;
DROP INDEX IF EXISTS idx_governance_decisions_policy_version;
DROP INDEX IF EXISTS idx_governance_decisions_action_subject;
ALTER TABLE governance_decisions DROP COLUMN IF EXISTS policy_version_id;
ALTER TABLE governance_decisions DROP COLUMN IF EXISTS action_subject_id;
ALTER TABLE governance_decisions DROP COLUMN IF EXISTS action_subject_type;