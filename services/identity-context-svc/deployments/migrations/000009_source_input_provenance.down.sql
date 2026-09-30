-- 000009_source_input_provenance.down.sql
--
-- Reverses 000009. Dropping the basis columns discards how each recorded
-- channel and workload was established; the values themselves (000008) stay.

BEGIN;

ALTER TABLE session_contexts
    DROP CONSTRAINT IF EXISTS session_contexts_discarded_workload_not_recorded,
    DROP CONSTRAINT IF EXISTS session_contexts_rejected_channel_not_recorded,
    DROP CONSTRAINT IF EXISTS session_contexts_entitlement_status_known,
    DROP CONSTRAINT IF EXISTS session_contexts_ingress_cache_state_known,
    DROP CONSTRAINT IF EXISTS session_contexts_workload_id_basis_known,
    DROP CONSTRAINT IF EXISTS session_contexts_source_channel_basis_known;

ALTER TABLE session_contexts
    DROP COLUMN IF EXISTS entitlement_context_status,
    DROP COLUMN IF EXISTS ingress_cache_state,
    DROP COLUMN IF EXISTS workload_id_basis,
    DROP COLUMN IF EXISTS source_channel_basis;

COMMIT;
