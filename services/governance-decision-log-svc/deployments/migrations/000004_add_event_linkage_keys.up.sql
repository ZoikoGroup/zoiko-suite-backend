-- workflow_instance_id and causation_id were previously left inside the
-- EvaluationContext JSONB catch-all, per this service's own MVP-schema
-- doctrine comment ("belong inside EvaluationContext until there's a
-- concrete need to query on them directly"). That need now exists: this is
-- the platform's canonical governance evidence log, and "find every
-- decision made during workflow instance X" or "find the decision this
-- event caused" are real queries nothing can currently answer without a
-- JSONB scan. Both nullable — a decision may not be workflow-triggered, and
-- causation is not always known.
ALTER TABLE governance_decisions
    ADD COLUMN workflow_instance_id TEXT,
    ADD COLUMN causation_id         TEXT;

CREATE INDEX idx_governance_decisions_workflow_instance
    ON governance_decisions (workflow_instance_id)
    WHERE workflow_instance_id IS NOT NULL;
