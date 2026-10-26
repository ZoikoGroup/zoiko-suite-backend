-- 000008_add_escalation_target.up.sql
-- Add escalation_target for WFC-01 §4.2 escalate-to-workflow.
--
-- Recovered: this number was missing from the sequence and the column was
-- referenced by the store layer (instanceColumns, CreateWorkflow inserts,
-- EscalateToWorkflow updates) but never created by any migration, so every
-- workflow-instance insert failed with SQLSTATE 42703.

ALTER TABLE workflow_instances
    ADD COLUMN escalation_target TEXT;
