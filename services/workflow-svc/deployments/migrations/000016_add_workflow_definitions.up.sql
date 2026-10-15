-- 000016_add_workflow_definitions.up.sql
-- Add versioned workflow definitions per R-001 WFC-02 / GOV-06

-- Workflow definitions are immutable, versioned entities that define the approval chain.
-- Each workflow instance is pinned to a specific definition version.
CREATE TABLE workflow_definitions (
    workflow_definition_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    
    -- Definition identity
    workflow_type          VARCHAR(64) NOT NULL,      -- e.g. "PURCHASE_APPROVAL"
    version                INT NOT NULL DEFAULT 1,    -- Monotonically increasing
    
    -- Approval chain (ordered stages)
    stages_json            JSONB NOT NULL,            -- Array of {order, approver_role, due_at_hours}
    
    -- Metadata
    name                   VARCHAR(255),
    description            TEXT,
    created_by             TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    superseded_by          UUID REFERENCES workflow_definitions(workflow_definition_id),
    
    -- Status
    is_active              BOOLEAN NOT NULL DEFAULT true,
    
    -- R-001 §9.1 common columns
    row_version            INT NOT NULL DEFAULT 1,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    plane                  VARCHAR(32) NOT NULL DEFAULT 'governance',
    data_class             VARCHAR(32) NOT NULL DEFAULT 'INTERNAL',
    residency_region       VARCHAR(64) NOT NULL DEFAULT 'default'
);

-- Unique constraint: one active version per (tenant, workflow_type)
CREATE UNIQUE INDEX idx_workflow_definitions_active_version 
    ON workflow_definitions (tenant_id, workflow_type, version) 
    WHERE is_active = true;

-- Index for finding definitions by type
CREATE INDEX idx_workflow_definitions_type ON workflow_definitions (tenant_id, workflow_type);

-- Add workflow_definition_id and workflow_definition_version to workflow_instances
ALTER TABLE workflow_instances
    ADD COLUMN workflow_definition_id UUID REFERENCES workflow_definitions(workflow_definition_id),
    ADD COLUMN workflow_definition_version INT;

-- Index for finding instances by definition
CREATE INDEX idx_workflow_instances_definition ON workflow_instances (workflow_definition_id);