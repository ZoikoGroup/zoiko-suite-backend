-- Migration: 000007_add_tenant_to_replay_manifests.up.sql
--
-- Adds tenant_id to replay_manifests and enforces tenant isolation via RLS
-- with FORCE, matching the pattern on governance_decisions (migration 000006).
-- This closes the cross-tenant disclosure gap where any caller could read
-- another tenant's replay manifests by decision_id.
--
-- The backfill is an UPDATE on an append-only table, and the
-- replay_manifests_immutable trigger (000005) refuses every UPDATE. The first
-- version of this file ran the UPDATE with the trigger live, so it failed on
-- any database that already held a replay manifest -- and, run statement by
-- statement, it left the ADD COLUMN behind. The trigger is now disabled for the
-- backfill only, inside one transaction, and every step is re-runnable so a
-- database that took the partial apply can run this file again.

BEGIN;

ALTER TABLE replay_manifests
    ADD COLUMN IF NOT EXISTS tenant_id VARCHAR(64) NOT NULL DEFAULT '';

-- Backfill tenant_id from the parent governance_decisions row. The FK on
-- decision_id guarantees every manifest has a parent.
--
-- governance_decisions is FORCE row-level security (000006), which binds the
-- owner this migration runs as: with no tenant set, the join would match no
-- row and the backfill would silently leave every tenant_id ''. FORCE is lifted
-- for this one statement only, inside this transaction.
ALTER TABLE governance_decisions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE replay_manifests DISABLE TRIGGER replay_manifests_immutable;
UPDATE replay_manifests rm
SET tenant_id = gd.tenant_id
FROM governance_decisions gd
WHERE rm.decision_id = gd.decision_id
  AND rm.tenant_id = '';
ALTER TABLE replay_manifests ENABLE TRIGGER replay_manifests_immutable;
ALTER TABLE governance_decisions FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM replay_manifests WHERE tenant_id = '') THEN
        RAISE EXCEPTION 'replay_manifests backfill left rows without a tenant';
    END IF;
END $$;

ALTER TABLE replay_manifests
    ALTER COLUMN tenant_id DROP DEFAULT;

-- Add index for tenant-scoped queries
CREATE INDEX IF NOT EXISTS idx_replay_manifests_tenant_decision
    ON replay_manifests (tenant_id, decision_id, replayed_at DESC);

-- Enable RLS and create FORCE policy
ALTER TABLE replay_manifests ENABLE ROW LEVEL SECURITY;

ALTER TABLE replay_manifests FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON replay_manifests;
CREATE POLICY tenant_isolation_policy ON replay_manifests
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

COMMIT;
