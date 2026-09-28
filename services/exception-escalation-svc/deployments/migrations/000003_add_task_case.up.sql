-- +migrate Up
BEGIN;

-- BIZ-05 Task / Case Management lives beside ExceptionCase/EscalationRecord
-- and AuditFinding in this same service — the user's own decision after an
-- audit found no existing service a clean home for a generic Task/Case
-- engine, and exception-escalation-svc already hosts the closest analog
-- (case/escalation/linked-object/SoD patterns) as a second bolted-on
-- domain. TEXT ids ("task-<uuid>", "case-<uuid>") to match this service's
-- own existing convention, not native UUID columns.
--
-- Case is a lightweight container; Task is the real governed entity with
-- its own lifecycle. task_transitions is the append-only audit trail —
-- same split as workflow-svc's WorkflowInstance/WorkflowTransition.
--
-- CreateCase is not named in the doc's own command list (only CloseCase
-- is), but the doc's canonical-entity table lists Case as its own
-- write-owned entity with nothing else that creates one — same class of
-- gap as BIZ-04's missing RetireForm, filled here rather than left
-- unreachable.

CREATE TABLE cases (
    case_id         TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL,
    legal_entity_id TEXT        NOT NULL,
    case_type       TEXT        NOT NULL,
    purpose         TEXT        NOT NULL DEFAULT '',
    status          TEXT        NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'CLOSED')),
    created_by      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_by       TEXT        NOT NULL DEFAULT '',
    closed_at       TIMESTAMPTZ,
    closure_reason  TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (case_id, tenant_id)
);
CREATE INDEX idx_cases_tenant_entity ON cases (tenant_id, legal_entity_id);
CREATE INDEX idx_cases_status ON cases (tenant_id, status);

CREATE TABLE tasks (
    task_id             TEXT        NOT NULL,
    tenant_id           TEXT        NOT NULL,
    legal_entity_id     TEXT        NOT NULL,
    -- No hard FK to cases — same opaque-reference posture this service
    -- already uses for linked_object_type/linked_object_id, and NULL
    -- means "standalone task, not part of any case".
    case_id             TEXT,
    task_type           TEXT        NOT NULL,
    priority            TEXT        NOT NULL DEFAULT 'MEDIUM' CHECK (priority IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL')),
    business_trigger    TEXT        NOT NULL DEFAULT '',
    linked_object_type  TEXT        NOT NULL,
    linked_object_id    TEXT        NOT NULL,
    required_evidence   TEXT        NOT NULL DEFAULT '',
    status              TEXT        NOT NULL DEFAULT 'NEW'
        CHECK (status IN ('NEW', 'ASSIGNED', 'IN_PROGRESS', 'BLOCKED', 'ESCALATED', 'COMPLETED', 'CLOSED', 'CANCELLED')),
    assigned_to_role    TEXT        NOT NULL DEFAULT '',
    assigned_to_user    TEXT        NOT NULL DEFAULT '',
    -- Read-time only — see internal/store/task_store.go's own doc comment
    -- on GetSLAState for why there is no background clock service that
    -- could silently auto-escalate/close a task on failure (the doc's own
    -- named failure mode).
    sla_deadline        TIMESTAMPTZ,
    blocked_reason       TEXT        NOT NULL DEFAULT '',
    escalated_to_role     TEXT        NOT NULL DEFAULT '',
    escalated_at           TIMESTAMPTZ,
    completion_notes         TEXT        NOT NULL DEFAULT '',
    completed_at               TIMESTAMPTZ,
    closed_by                    TEXT        NOT NULL DEFAULT '',
    closed_at                      TIMESTAMPTZ,
    cancel_reason                    TEXT        NOT NULL DEFAULT '',
    cancelled_at                       TIMESTAMPTZ,
    reopened_count                       INTEGER     NOT NULL DEFAULT 0,
    created_by                             TEXT        NOT NULL,
    created_at                               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, tenant_id)
);
CREATE INDEX idx_tasks_tenant_entity ON tasks (tenant_id, legal_entity_id);
CREATE INDEX idx_tasks_case ON tasks (tenant_id, case_id) WHERE case_id IS NOT NULL;
CREATE INDEX idx_tasks_status ON tasks (tenant_id, status);
CREATE INDEX idx_tasks_assignee_user ON tasks (tenant_id, assigned_to_user) WHERE assigned_to_user <> '';
CREATE INDEX idx_tasks_assignee_role ON tasks (tenant_id, assigned_to_role) WHERE assigned_to_role <> '';
CREATE INDEX idx_tasks_linked_object ON tasks (linked_object_type, linked_object_id);

CREATE TABLE task_transitions (
    transition_id      TEXT        NOT NULL,
    task_id             TEXT        NOT NULL,
    tenant_id             TEXT        NOT NULL,
    from_status             TEXT        NOT NULL,
    to_status                 TEXT        NOT NULL,
    actor_principal_id          TEXT        NOT NULL,
    reason                         TEXT        NOT NULL DEFAULT '',
    occurred_at                       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (transition_id, tenant_id)
);
CREATE INDEX idx_task_transitions_task ON task_transitions (tenant_id, task_id, occurred_at);

ALTER TABLE cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE cases FORCE ROW LEVEL SECURITY;
CREATE POLICY cases_tenant_isolation ON cases
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE tasks ENABLE ROW LEVEL SECURITY;
ALTER TABLE tasks FORCE ROW LEVEL SECURITY;
CREATE POLICY tasks_tenant_isolation ON tasks
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE task_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE task_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY task_transitions_tenant_isolation ON task_transitions
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Append-only, same doctrine as this service's existing evidentiary child
-- tables (migration 000002).
CREATE OR REPLACE FUNCTION reject_task_transition_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'task transitions are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_task_transition_mutation
    BEFORE UPDATE OR DELETE ON task_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_task_transition_mutation();

COMMIT;
