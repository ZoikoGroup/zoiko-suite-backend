-- Migration 000027: the governance decision a workflow resolves (tracker
-- "Assignment 25 Nov 2026: automatic decision capture", step DL-2).
--
-- A workflow started for an escalated governance decision (a policy
-- APPROVAL_REQUIRED or SIGNATURE_REQUIRED, an authorization REQUIRE_APPROVAL)
-- stores that decision's id. Every governance-ledger record the workflow
-- writes then carries it as causation_id, so the ledger links the escalation
-- to the approval that resolved it (Doc 01 s8.2 "linked to source action
-- through full correlation chain"). Before this, none of the 64 workflow
-- decisions in the ledger carried a causation id (25 Nov 2026).
--
-- Set once, when the workflow starts: from X-Causation-Id or the body's
-- causation_id, or else from the signatory decision the workflow is bound to.
-- Nullable: a workflow started for no governance decision has none.
ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS causation_decision_id VARCHAR(128);

COMMENT ON COLUMN workflow_instances.causation_decision_id IS
    'Governance decision (decision-log decision_id) this workflow resolves; causation_id of its ledger records (000027).';
