-- Migration: 000007_add_tenant_to_replay_manifests.up.sql
--
-- Adds tenant_id to replay_manifests and enforces tenant isolation via RLS
-- with FORCE, matching the pattern on governance_decisions (migration 000006).
-- This closes the cross-tenant disclosure gap where any caller could read
-- another tenant's replay manifests by decision_id.

-- Add tenant_id column, populated from the parent decision's tenant_id
ALTER TABLE replay_manifests
    ADD COLUMN tenant_id VARCHAR(64) NOT NULL DEFAULT '';

-- Backfill tenant_id from the parent governance_decisions row
UPDATE replay_manifests rm
SET tenant_id = gd.tenant_id
FROM governance_decisions gd
WHERE rm.decision_id = gd.decision_id;

-- Make tenant_id NOT NULL (backfill above ensures no NULLs remain)
ALTER TABLE replay_manifests
    ALTER COLUMN tenant_id SET NOT NULL,
    ALTER COLUMN tenant_id DROP DEFAULT;

-- Add index for tenant-scoped queries
CREATE INDEX IF NOT EXISTS idx_replay_manifests_tenant_decision
    ON replay_manifests (tenant_id, decision_id, replayed_at DESC);

-- Enable RLS and create FORCE policy
ALTER TABLE replay_manifests ENABLE ROW LEVEL SECURITY;

ALTER TABLE replay_manifests FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON replay_manifests
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));