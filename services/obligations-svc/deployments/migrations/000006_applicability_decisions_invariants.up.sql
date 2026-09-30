-- 000006_applicability_decisions_invariants.up.sql
-- Obligations Service — applicability_decisions invariants
--
-- Fixes:
-- 1. "Exactly one" actor rule: change from OR to XOR
-- 2. Prevent UNASSESSED from being stored as a decision value
-- 3. Add tenant_id column and RLS policy

BEGIN;

-- Fix "exactly one" actor/system constraint
ALTER TABLE applicability_decisions
    DROP CONSTRAINT IF EXISTS chk_applicability_decisions_actor_or_system;

ALTER TABLE applicability_decisions
    ADD CONSTRAINT chk_applicability_decisions_exactly_one_actor
    CHECK (
        (decided_by_principal_id IS NOT NULL AND decided_by_system IS NULL)
        OR (decided_by_principal_id IS NULL AND decided_by_system IS NOT NULL)
    );

-- Prevent UNASSESSED from being stored (it's an application-level answer, not a stored decision)
ALTER TABLE applicability_decisions
    ADD CONSTRAINT chk_applicability_decisions_decision_valid
    CHECK (decision IN ('APPLICABLE', 'NOT_APPLICABLE', 'UNCERTAIN'));

-- Add tenant_id column for direct RLS
ALTER TABLE applicability_decisions ADD COLUMN IF NOT EXISTS tenant_id UUID;
UPDATE applicability_decisions ad
   SET tenant_id = o.tenant_id
  FROM obligations o
 WHERE ad.obligation_id = o.obligation_id AND ad.tenant_id IS NULL;
ALTER TABLE applicability_decisions ALTER COLUMN tenant_id SET NOT NULL;

-- RLS for applicability_decisions
ALTER TABLE applicability_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE applicability_decisions FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON applicability_decisions;
CREATE POLICY tenant_isolation_policy ON applicability_decisions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Index for tenant-scoped queries
CREATE INDEX IF NOT EXISTS idx_applicability_decisions_tenant
    ON applicability_decisions (tenant_id, obligation_id);

COMMIT;