-- Migration 000027 down.
ALTER TABLE workflow_instances DROP COLUMN IF EXISTS causation_decision_id;
