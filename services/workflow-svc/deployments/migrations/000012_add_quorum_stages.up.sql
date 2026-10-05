-- 000012_add_quorum_stages.up.sql
-- ZS-SVC-R-001 §5.1/§8.3 — WFC-03's quorum / N-of-M dual-control
-- approval, additive to workflow_stages (000001).
--
-- Confirmed via repo-wide search before this migration was written:
-- quorum/N-of-M approval has zero implementation anywhere in this
-- platform. Every existing WorkflowStage is a single named approver,
-- strictly sequential — this migration adds a SECOND stage shape
-- (QUORUM) alongside the existing one (now named SINGLE), without
-- changing SINGLE's existing columns, constraints or behavior at all.
--
-- A QUORUM stage has no single approver_principal_id — instead it has
-- an eligible pool (workflow_stage_quorum_approvers) and a threshold
-- (required_approvals). Each eligible approver may cast exactly one
-- decision (workflow_stage_quorum_decisions, UNIQUE per approver) —
-- this is the "concurrency-safe double-count protection" the doc's
-- NP-09 requires: a UNIQUE constraint makes a double-vote a conflict
-- the database itself refuses, not a race the application has to
-- detect.

ALTER TABLE workflow_stages ADD COLUMN stage_type VARCHAR(16) NOT NULL DEFAULT 'SINGLE'
    CHECK (stage_type IN ('SINGLE', 'QUORUM'));

-- approver_principal_id was NOT NULL for every existing (SINGLE) row;
-- a QUORUM stage has no single approver, so the column becomes
-- nullable, with a CHECK pinning exactly which shape each stage_type
-- must have.
ALTER TABLE workflow_stages ALTER COLUMN approver_principal_id DROP NOT NULL;
ALTER TABLE workflow_stages ADD COLUMN required_approvals INT;
ALTER TABLE workflow_stages ADD CONSTRAINT workflow_stages_shape_matches_type CHECK (
    (stage_type = 'SINGLE' AND approver_principal_id IS NOT NULL AND required_approvals IS NULL)
    OR
    (stage_type = 'QUORUM' AND approver_principal_id IS NULL AND required_approvals IS NOT NULL AND required_approvals > 0)
);

-- ── Quorum eligible pool ─────────────────────────────────────────────────────

CREATE TABLE workflow_stage_quorum_approvers (
    workflow_stage_id    UUID NOT NULL REFERENCES workflow_stages(workflow_stage_id),
    approver_principal_id TEXT NOT NULL,
    PRIMARY KEY (workflow_stage_id, approver_principal_id)
);

-- ── Quorum decisions ─────────────────────────────────────────────────────────

-- One decision per eligible approver per stage, ever — the composite
-- FK below ensures a decision can only be cast by a principal who is
-- actually in the stage's eligible pool, and the PRIMARY KEY on the
-- pool table it references means a non-eligible principal's INSERT
-- fails at the database, not just the application's own pre-check.
CREATE TABLE workflow_stage_quorum_decisions (
    workflow_stage_quorum_decision_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_stage_id                 UUID NOT NULL,
    approver_principal_id             TEXT NOT NULL,

    -- APPROVE | REJECT.
    decision                          VARCHAR(16) NOT NULL CHECK (decision IN ('APPROVE', 'REJECT')),
    rationale                         TEXT,
    acted_at                          TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT workflow_stage_quorum_decisions_one_per_approver UNIQUE (workflow_stage_id, approver_principal_id),
    CONSTRAINT workflow_stage_quorum_decisions_eligible_approver
        FOREIGN KEY (workflow_stage_id, approver_principal_id)
        REFERENCES workflow_stage_quorum_approvers (workflow_stage_id, approver_principal_id)
);

CREATE INDEX idx_workflow_stage_quorum_decisions_stage ON workflow_stage_quorum_decisions (workflow_stage_id);
