-- 000009_source_input_provenance.up.sql
--
-- Closes the schema half of four GOV-01 §4 items the 2026-09-28 re-audit left
-- open after 000008. The application half is internal/context/source_inputs.go,
-- the resolver, and the ingress checker.
--
--   1. source_channel_basis / workload_id_basis
--      §4 lists source channel and workload identity as SERVER-resolved. 000008
--      recorded both verbatim from client headers the edge does not strip, so
--      the evidence row recorded whatever the caller said. The service now
--      resolves each against the verified principal's type and records HOW:
--      server-derived, verified principal, asserted-and-consistent, or
--      rejected/discarded (value NOT recorded). §4's negative-path column
--      permits "preserve UNKNOWN state" -- an unknown is honest, a
--      contradicted assertion is not.
--
--   2. ingress_cache_state
--      §4's cache model is FRESH / STALE / INVALIDATED. The ingress check
--      computed the state and dropped it, so no decision said which applied.
--
--   3. entitlement_context_status
--      entitlement_context_ref (000008) is NULL in every deployment because its
--      owner, COM-03 Entitlement, does not exist. The status keeps "never
--      resolved: no upstream" distinct from "upstream failed" and from
--      "resolved".
--
-- All nullable, no defaults: NULL means the row predates this migration. Rows
-- written before it are NOT backfilled -- guessing a basis for a value that was
-- recorded without one would fabricate provenance.

BEGIN;

ALTER TABLE session_contexts
    ADD COLUMN IF NOT EXISTS source_channel_basis       VARCHAR(32),
    ADD COLUMN IF NOT EXISTS workload_id_basis          VARCHAR(32),
    ADD COLUMN IF NOT EXISTS ingress_cache_state        VARCHAR(16),
    ADD COLUMN IF NOT EXISTS entitlement_context_status VARCHAR(32);

-- CHECKs, because a basis is only evidence if it is one of the known answers.
-- A typo in Go would otherwise land a new, undocumented state in the audit
-- record, and every report grouping by basis would silently miss it.
ALTER TABLE session_contexts
    ADD CONSTRAINT session_contexts_source_channel_basis_known CHECK (
        source_channel_basis IS NULL OR source_channel_basis IN (
            'SERVER_DERIVED', 'CLIENT_ASSERTED_CONSISTENT', 'REJECTED_INCONSISTENT', 'NOT_PRESENTED')),
    ADD CONSTRAINT session_contexts_workload_id_basis_known CHECK (
        workload_id_basis IS NULL OR workload_id_basis IN (
            'VERIFIED_PRINCIPAL', 'REJECTED_INCONSISTENT', 'UNVERIFIABLE_DISCARDED', 'NOT_PRESENTED')),
    ADD CONSTRAINT session_contexts_ingress_cache_state_known CHECK (
        ingress_cache_state IS NULL OR ingress_cache_state IN ('FRESH', 'STALE', 'INVALIDATED')),
    ADD CONSTRAINT session_contexts_entitlement_status_known CHECK (
        entitlement_context_status IS NULL OR entitlement_context_status IN (
            'RESOLVED', 'UPSTREAM_NOT_CONFIGURED', 'UPSTREAM_UNAVAILABLE')),
    -- A value that was rejected or discarded must not be on the row. This is
    -- the invariant the whole change rests on, so it is enforced where a
    -- future code path cannot forget it.
    ADD CONSTRAINT session_contexts_rejected_channel_not_recorded CHECK (
        source_channel_basis IS DISTINCT FROM 'REJECTED_INCONSISTENT' OR source_channel IS NULL),
    ADD CONSTRAINT session_contexts_discarded_workload_not_recorded CHECK (
        workload_id_basis IS DISTINCT FROM 'UNVERIFIABLE_DISCARDED' OR workload_id IS NULL);

COMMENT ON COLUMN session_contexts.source_channel_basis IS
    'How source_channel was established (ZS-SVC-A-001 §4 server-resolved). REJECTED_INCONSISTENT means the client asserted a channel the verified principal type cannot arrive on; the value is not recorded.';
COMMENT ON COLUMN session_contexts.workload_id_basis IS
    'How workload_id was established. VERIFIED_PRINCIPAL: a service account / API client authenticated as itself. UNVERIFIABLE_DISCARDED: a workload id asserted on a human session, which nothing here can attest.';
COMMENT ON COLUMN session_contexts.ingress_cache_state IS
    'ZS-SVC-A-001 §4 cache state (FRESH / STALE / INVALIDATED) of the ingress binding this decision was checked against. NULL when no binding was consulted.';
COMMENT ON COLUMN session_contexts.entitlement_context_status IS
    'Outcome of resolving entitlement_context_ref. UPSTREAM_NOT_CONFIGURED until COM-03 Entitlement exists in the estate.';

COMMIT;
