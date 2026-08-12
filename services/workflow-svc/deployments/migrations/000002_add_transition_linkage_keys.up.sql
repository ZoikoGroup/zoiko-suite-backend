-- workflow_transitions is this service's append-only audit trail — the
-- evidence of every state change a workflow instance goes through — but had
-- zero correlation/causation columns, unlike workflow_instances (which
-- already carries correlation_id). Both nullable: correlation_id is
-- populated from the owning instance's own correlation_id (real data
-- already collected at workflow creation, not invented), and causation_id
-- is populated only when the caller submitting the action supplies one.
ALTER TABLE workflow_transitions
    ADD COLUMN correlation_id TEXT,
    ADD COLUMN causation_id   TEXT;
