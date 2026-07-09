-- 000001_initial_schema.up.sql
-- Workflow & Approvals Service — initial schema
--
-- Owns: workflow_instances, workflow_stages, workflow_transitions.
--
-- Design decisions:
--   - workflow_type, workflow_status, stage_status are VARCHAR — no enums.
--   - workflow_status IS a real (small) state machine: PENDING -> APPROVED
--     | REJECTED | ESCALATED | CANCELLED, enforced in application code
--     (store.transitionWorkflowStatus), not a DB CHECK constraint.
--   - No hard-delete anywhere. Cancelling a workflow is a status
--     transition, never a row removal. workflow_transitions is pure
--     append-only evidence — no UPDATE/DELETE should ever target it.
--   - Critical constraint: entity-bound (legal_entity_id NOT NULL), same
--     as every other service in this platform.
--   - The approval chain (workflow_stages) is supplied by the caller at
--     creation time — this service does not resolve "who approves what"
--     from any rule engine; no such rules are specified in the docs.

-- ── workflow_instances ────────────────────────────────────────────────────────

CREATE TABLE workflow_instances (
    workflow_instance_id     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    tenant_id                UUID        NOT NULL,
    legal_entity_id          UUID        NOT NULL,

    -- Data only (e.g. "PURCHASE_APPROVAL").
    workflow_type            VARCHAR(64) NOT NULL,

    -- PENDING | APPROVED | REJECTED | ESCALATED | CANCELLED.
    workflow_status          VARCHAR(32) NOT NULL DEFAULT 'PENDING',

    -- 1-based index into workflow_stages.stage_order currently awaiting
    -- action. 0 once the workflow reaches a terminal state.
    current_stage            INT         NOT NULL DEFAULT 1,

    initiated_by             TEXT        NOT NULL,
    correlation_id           TEXT,

    started_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at             TIMESTAMPTZ
);

CREATE INDEX idx_workflow_instances_entity ON workflow_instances (legal_entity_id);
CREATE INDEX idx_workflow_instances_status ON workflow_instances (workflow_status);

-- ── workflow_stages ──────────────────────────────────────────────────────────

CREATE TABLE workflow_stages (
    workflow_stage_id        UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_instance_id     UUID        NOT NULL REFERENCES workflow_instances(workflow_instance_id),

    stage_order              INT         NOT NULL,
    approver_principal_id    TEXT        NOT NULL,

    -- PENDING | APPROVED | REJECTED | SKIPPED.
    stage_status             VARCHAR(32) NOT NULL DEFAULT 'PENDING',

    acted_at                 TIMESTAMPTZ,
    rationale                TEXT
);

-- One stage per (instance, order) — the ordered chain is well-defined.
CREATE UNIQUE INDEX idx_workflow_stages_order_unique ON workflow_stages (workflow_instance_id, stage_order);

-- Primary lookup: "resolve next approver" / "get current stage".
CREATE INDEX idx_workflow_stages_instance ON workflow_stages (workflow_instance_id, stage_order);

-- ── workflow_transitions ─────────────────────────────────────────────────────
-- Append-only evidence. No UPDATE/DELETE statement should ever target this table.

CREATE TABLE workflow_transitions (
    workflow_transition_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_instance_id     UUID        NOT NULL REFERENCES workflow_instances(workflow_instance_id),

    from_state               VARCHAR(32) NOT NULL,
    to_state                 VARCHAR(32) NOT NULL,
    acted_by                 TEXT        NOT NULL,
    rationale                TEXT,

    acted_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_workflow_transitions_instance ON workflow_transitions (workflow_instance_id, acted_at);
