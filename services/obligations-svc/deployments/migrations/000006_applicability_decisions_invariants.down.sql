-- 000006_applicability_decisions_invariants.down.sql

BEGIN;

DROP POLICY IF EXISTS tenant_isolation_policy ON applicability_decisions;
ALTER TABLE applicability_decisions DISABLE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS idx_applicability_decisions_tenant;

ALTER TABLE applicability_decisions DROP CONSTRAINT IF EXISTS chk_applicability_decisions_exactly_one_actor;
ALTER TABLE applicability_decisions DROP CONSTRAINT IF EXISTS chk_applicability_decisions_decision_valid;

ALTER TABLE applicability_decisions
    ADD CONSTRAINT chk_applicability_decisions_actor_or_system CHECK (
        decided_by_principal_id IS NOT NULL OR decided_by_system IS NOT NULL
    );

ALTER TABLE applicability_decisions DROP COLUMN IF EXISTS tenant_id;

COMMIT;