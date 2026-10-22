-- Migration: 000006_add_consolidation_adjustment_transitions.up.sql
--
-- ACC-12 GetAdjustmentHistory. Migration 000003 kept only the latest
-- approve/post/reverse columns on the adjustment row, so the lifecycle before
-- the current state was unrecoverable. This table is the append-only log: one
-- row per transition, written by the command that makes the change in the
-- same transaction, never updated or deleted. It answers the spec's
-- "approval state" and "approvers" evidence without reconstructing history
-- from mutable columns.
--
-- A rejected transition writes no row: the guarded UPDATE and the INSERT share
-- a transaction, so a wrong-status command rolls both back.
CREATE TABLE consolidation_adjustment_transitions (
    transition_id              UUID PRIMARY KEY,
    tenant_id                  VARCHAR(255) NOT NULL,
    consolidation_adjustment_id UUID NOT NULL REFERENCES consolidation_adjustments(consolidation_adjustment_id),
    sequence                   BIGSERIAL NOT NULL UNIQUE,
    from_status                VARCHAR(20), -- NULL for the creation step
    to_status                  VARCHAR(20) NOT NULL,
    actor_principal_id         VARCHAR(255) NOT NULL,
    occurred_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    correlation_id             TEXT,
    reason                     TEXT,        -- the reversal reason, when there is one
    details                    JSONB NOT NULL DEFAULT '{}'::jsonb,

    CONSTRAINT chk_adjustment_transition_from_status CHECK (
        from_status IS NULL OR from_status IN ('PENDING_APPROVAL', 'APPROVED', 'POSTED', 'REVERSED')),
    CONSTRAINT chk_adjustment_transition_to_status CHECK (
        to_status IN ('PENDING_APPROVAL', 'APPROVED', 'POSTED', 'REVERSED'))
);

ALTER TABLE consolidation_adjustment_transitions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON consolidation_adjustment_transitions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_consolidation_adjustment_transitions_adjustment
    ON consolidation_adjustment_transitions (tenant_id, consolidation_adjustment_id, sequence);

-- Backfill one chain per existing adjustment from the lifecycle columns the
-- old commands wrote. Statements run in lifecycle order, so the BIGSERIAL
-- sequence orders each chain created -> approved -> posted -> reversed.
INSERT INTO consolidation_adjustment_transitions (
    transition_id, tenant_id, consolidation_adjustment_id, from_status, to_status,
    actor_principal_id, occurred_at, details
)
SELECT gen_random_uuid(), tenant_id, consolidation_adjustment_id, NULL, 'PENDING_APPROVAL',
       created_by_principal_id, created_at,
       jsonb_build_object('adjustment_type', adjustment_type)
FROM consolidation_adjustments
ORDER BY created_at;

INSERT INTO consolidation_adjustment_transitions (
    transition_id, tenant_id, consolidation_adjustment_id, from_status, to_status,
    actor_principal_id, occurred_at, details
)
SELECT gen_random_uuid(), tenant_id, consolidation_adjustment_id, 'PENDING_APPROVAL', 'APPROVED',
       COALESCE(approved_by_principal_id, ''), approved_at, '{}'::jsonb
FROM consolidation_adjustments
WHERE approved_at IS NOT NULL
ORDER BY approved_at;

INSERT INTO consolidation_adjustment_transitions (
    transition_id, tenant_id, consolidation_adjustment_id, from_status, to_status,
    actor_principal_id, occurred_at, details
)
SELECT gen_random_uuid(), tenant_id, consolidation_adjustment_id, 'APPROVED', 'POSTED',
       COALESCE(posted_by_principal_id, ''), posted_at,
       jsonb_strip_nulls(jsonb_build_object(
           'consolidation_book_journal_id', consolidation_book_journal_id))
FROM consolidation_adjustments
WHERE posted_at IS NOT NULL
ORDER BY posted_at;

INSERT INTO consolidation_adjustment_transitions (
    transition_id, tenant_id, consolidation_adjustment_id, from_status, to_status,
    actor_principal_id, occurred_at, reason, details
)
SELECT gen_random_uuid(), tenant_id, consolidation_adjustment_id, 'POSTED', 'REVERSED',
       COALESCE(reversed_by_principal_id, ''), reversed_at, reversal_reason,
       jsonb_strip_nulls(jsonb_build_object(
           'superseded_by_adjustment_id', superseded_by_adjustment_id))
FROM consolidation_adjustments
WHERE reversed_at IS NOT NULL
ORDER BY reversed_at;
