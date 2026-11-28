-- Migration 000026: WF-S24 operator repair + WF-S25 emergency approval
-- This migration adds the remaining tables and columns needed

-- Enum for operator action kinds
CREATE TYPE operator_action_kind AS ENUM (
    'RETRY_TIMER',
    'RECOMPUTE_NEXT_ACTION',
    'FORCE_ESCALATE',
    'FORCE_EXPIRE',
    'REQUEUE_PARKED_EVENT',
    'RELEASE_CLAIM'
);

-- Enum for operator action status
CREATE TYPE operator_action_status AS ENUM (
    'PENDING',
    'APPROVED',
    'REJECTED',
    'EXECUTED',
    'EXPIRED'
);

-- Enum for emergency approval conditions
CREATE TYPE emergency_condition AS ENUM (
    'QUORUM',
    'TIMING'
);

-- Add emergency_conditions to workflow_definitions if not exists
ALTER TABLE workflow_definitions 
    ADD COLUMN IF NOT EXISTS emergency_conditions emergency_condition[] DEFAULT '{}';

-- Indexes for WF-S22 (already in 000025)
CREATE INDEX IF NOT EXISTS idx_workflow_definitions_status ON workflow_definitions (tenant_id, definition_status);
CREATE INDEX IF NOT EXISTS idx_workflow_definitions_effective ON workflow_definitions (effective_from, effective_to);

-- Operator actions table (WF-S24)
CREATE TABLE IF NOT EXISTS operator_actions (
    operator_action_id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    workflow_instance_id    uuid NOT NULL REFERENCES workflow_instances(workflow_instance_id) ON DELETE CASCADE,
    kind                    operator_action_kind NOT NULL,
    status                  operator_action_status NOT NULL DEFAULT 'PENDING',
    reason                  text NOT NULL,
    scope                   text,
    evidence_refs           text[] DEFAULT '{}',
    requested_by            text NOT NULL,
    requested_at            timestamptz NOT NULL DEFAULT NOW(),
    approved_by             text,
    approved_at             timestamptz,
    executed_at             timestamptz,
    execution_result        text,
    created_at              timestamptz NOT NULL DEFAULT NOW(),
    updated_at              timestamptz NOT NULL DEFAULT NOW(),
    row_version             int NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_operator_actions_tenant ON operator_actions (tenant_id);
CREATE INDEX IF NOT EXISTS idx_operator_actions_workflow ON operator_actions (workflow_instance_id);
CREATE INDEX IF NOT EXISTS idx_operator_actions_status ON operator_actions (status);
CREATE INDEX IF NOT EXISTS idx_operator_actions_kind ON operator_actions (kind);

ALTER TABLE operator_actions ENABLE ROW LEVEL SECURITY;
ALTER TABLE operator_actions FORCE ROW LEVEL SECURITY;
CREATE POLICY operator_actions_tenant_isolation ON operator_actions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TRIGGER operator_actions_updated_at
    BEFORE UPDATE ON operator_actions
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Emergency approvals table (WF-S25)
CREATE TABLE IF NOT EXISTS emergency_approvals (
    emergency_approval_id   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    workflow_instance_id    uuid NOT NULL REFERENCES workflow_instances(workflow_instance_id) ON DELETE CASCADE,
    break_glass_session_id  uuid NOT NULL,
    window_start            timestamptz NOT NULL,
    window_end              timestamptz NOT NULL,
    reason_code             text NOT NULL,
    incident_ref            text,
    scope                   text,
    max_impact              text,
    conditions              emergency_condition[] NOT NULL DEFAULT '{}',
    approved_by             text NOT NULL,
    approved_at             timestamptz NOT NULL DEFAULT NOW(),
    review_work_item_id     uuid REFERENCES work_items(work_item_id) ON DELETE SET NULL,
    created_at              timestamptz NOT NULL DEFAULT NOW(),
    updated_at              timestamptz NOT NULL DEFAULT NOW(),
    row_version             int NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_emergency_approvals_tenant ON emergency_approvals (tenant_id);
CREATE INDEX IF NOT EXISTS idx_emergency_approvals_workflow ON emergency_approvals (workflow_instance_id);
CREATE INDEX IF NOT EXISTS idx_emergency_approvals_session ON emergency_approvals (break_glass_session_id);

ALTER TABLE emergency_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE emergency_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY emergency_approvals_tenant_isolation ON emergency_approvals
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TRIGGER emergency_approvals_updated_at
    BEFORE UPDATE ON emergency_approvals
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Add next_action_at to workflow_instances if not exists (for stuck detection)
ALTER TABLE workflow_instances 
    ADD COLUMN IF NOT EXISTS next_action_at TIMESTAMPTZ;

-- Add index for stuck instance detection
CREATE INDEX IF NOT EXISTS idx_workflow_instances_next_action ON workflow_instances (next_action_at) WHERE next_action_at IS NOT NULL;

COMMENT ON TABLE operator_actions IS 'WF-S24: Operator repair actions for stuck workflow instances';
COMMENT ON TABLE emergency_approvals IS 'WF-S25: Emergency approvals with break-glass sessions and mandatory review work items';