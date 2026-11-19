-- Migration 000023: WFC-04 Cases
-- Creates the cases table with lifecycle states and workflow linking

-- Helper function to update updated_at column
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$;

CREATE TABLE cases (
    case_id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    legal_entity_id         uuid NOT NULL,
    workflow_instance_id    uuid REFERENCES workflow_instances(workflow_instance_id) ON DELETE SET NULL,
    case_type               text NOT NULL,  -- INVESTIGATION, DISPUTE, EXCEPTION, etc.
    case_state              text NOT NULL DEFAULT 'OPEN',  -- OPEN, IN_REVIEW, RESOLVED, CLOSED, ESCALATED
    title                   text NOT NULL,
    description             text,
    assigned_to_principal_id text,
    opened_at               timestamptz NOT NULL DEFAULT NOW(),
    resolved_at             timestamptz,
    closed_at               timestamptz,
    created_at              timestamptz NOT NULL DEFAULT NOW(),
    updated_at              timestamptz NOT NULL DEFAULT NOW(),
    version                 int NOT NULL DEFAULT 1,
    row_version             int NOT NULL DEFAULT 1
);

-- Indexes
CREATE INDEX cases_tenant_id_idx ON cases(tenant_id);
CREATE INDEX cases_legal_entity_id_idx ON cases(legal_entity_id);
CREATE INDEX cases_workflow_instance_id_idx ON cases(workflow_instance_id);
CREATE INDEX cases_case_state_idx ON cases(case_state);
CREATE INDEX cases_case_type_idx ON cases(case_type);

-- Row-level security
ALTER TABLE cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE cases FORCE ROW LEVEL SECURITY;
CREATE POLICY cases_tenant_isolation ON cases
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Trigger
CREATE TRIGGER cases_updated_at
    BEFORE UPDATE ON cases
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

COMMENT ON TABLE cases IS 'WFC-04: Cases — distinct business objects from workflows. States: OPEN→IN_REVIEW→RESOLVED/CLOSED/ESCALATED.';
COMMENT ON COLUMN cases.case_type IS 'Case classification: INVESTIGATION, DISPUTE, EXCEPTION, etc.';
COMMENT ON COLUMN cases.case_state IS 'Case state machine: OPEN, IN_REVIEW, RESOLVED, CLOSED, ESCALATED.';
COMMENT ON COLUMN cases.workflow_instance_id IS 'Optional link to a workflow instance for approval steps.';
COMMENT ON COLUMN cases.version IS 'Optimistic lock for case state transitions.';