-- 000012_add_row_version.up.sql
-- Add optimistic concurrency control (row_version) to workflow tables
-- per R-001 §9.1 common columns and to fix check-then-act races.

-- workflow_instances
ALTER TABLE workflow_instances
    ADD COLUMN row_version INT NOT NULL DEFAULT 1,
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN created_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN plane VARCHAR(32) NOT NULL DEFAULT 'governance',
    ADD COLUMN data_class VARCHAR(32) NOT NULL DEFAULT 'INTERNAL',
    ADD COLUMN residency_region VARCHAR(64) NOT NULL DEFAULT 'default';

CREATE INDEX idx_workflow_instances_row_version ON workflow_instances (row_version);

-- workflow_stages
ALTER TABLE workflow_stages
    ADD COLUMN row_version INT NOT NULL DEFAULT 1,
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN created_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN plane VARCHAR(32) NOT NULL DEFAULT 'governance',
    ADD COLUMN data_class VARCHAR(32) NOT NULL DEFAULT 'INTERNAL',
    ADD COLUMN residency_region VARCHAR(64) NOT NULL DEFAULT 'default';

CREATE INDEX idx_workflow_stages_row_version ON workflow_stages (row_version);

-- workflow_transitions (append-only, but add columns for consistency)
ALTER TABLE workflow_transitions
    ADD COLUMN row_version INT NOT NULL DEFAULT 1,
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN created_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN plane VARCHAR(32) NOT NULL DEFAULT 'governance',
    ADD COLUMN data_class VARCHAR(32) NOT NULL DEFAULT 'INTERNAL',
    ADD COLUMN residency_region VARCHAR(64) NOT NULL DEFAULT 'default';