-- 000008_gov01_gap_closure.down.sql
--
-- Reverses 000008. Note what this costs: dropping idempotency_keys discards
-- every recorded command response, so a retry that arrives after a down
-- migration is indistinguishable from a first attempt and will execute again.
-- That is inherent to removing replay protection, not a defect in the rollback.

BEGIN;

DROP POLICY IF EXISTS support_reconciler_read ON support_contexts;

DROP POLICY IF EXISTS retention_sweep_purge ON idempotency_keys;
DROP POLICY IF EXISTS tenant_isolation_policy ON idempotency_keys;
DROP INDEX IF EXISTS idx_idempotency_keys_created_at;
DROP TABLE IF EXISTS idempotency_keys;

ALTER TABLE session_contexts
    DROP COLUMN IF EXISTS ingress_binding_version,
    DROP COLUMN IF EXISTS entitlement_context_ref,
    DROP COLUMN IF EXISTS causation_id,
    DROP COLUMN IF EXISTS workload_id,
    DROP COLUMN IF EXISTS source_channel;

COMMIT;
