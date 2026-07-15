-- workflow_history_events: append-only immutable transition history for workflow instances.
--
-- Design decisions (see docs/architecture/01-backend.md §8.2 and doctrine.md):
--
--   1. Single table for all five workflow event types (workflow.started,
--      approval.granted, approval.rejected, workflow.escalated, workflow.completed).
--      All share the same structural shape; a JSONB payload column accommodates
--      per-type fields without schema proliferation.
--
--   2. event_id is the dedup key (PRIMARY KEY → unique index). Consumers issue:
--        INSERT … ON CONFLICT (event_id) DO NOTHING
--      This is the only safe dedup pattern — a prior SELECT EXISTS() check
--      allows a duplicate insertion race under concurrent delivery.
--
--   3. No UPDATE or DELETE permitted. This table is append-only evidence.
--      No soft-delete, no status column. History is immutable.
--
--   4. Every row carries tenant_id and legal_entity_id per doctrine §3.2.
--      For workflow.started events these are present in the payload.
--      For subsequent events (approval.granted, etc.) they are inherited from
--      the workflow.started row for the same workflow_instance_id — the consumer
--      performs a single lookup per non-started event.
--
--   5. recorded_at is set by the database clock, not by the event payload,
--      to prevent clock skew from producer services distorting insertion order.
--      The emitted_at (from the event envelope) is preserved inside the payload JSONB.
--
--   6. correlation_id is promoted to a top-level column (rather than left only
--      inside the JSONB payload) to enable efficient correlation queries without
--      parsing JSONB.

CREATE TABLE IF NOT EXISTS workflow_history_events (
    -- event_id: globally unique identifier assigned by the publishing service.
    -- This is the dedup key; the consumer issues ON CONFLICT (event_id) DO NOTHING.
    event_id              TEXT        NOT NULL,

    -- workflow_instance_id: the workflow whose history this row belongs to.
    workflow_instance_id  TEXT        NOT NULL,

    -- event_type: one of workflow.started, approval.granted, approval.rejected,
    -- workflow.escalated, workflow.completed.
    event_type            TEXT        NOT NULL,

    -- correlation_id: propagated from the event envelope for cross-service tracing.
    correlation_id        TEXT        NOT NULL,

    -- Mandatory governance context per doctrine §3.2.
    -- For non-started events these are inherited from the workflow.started row.
    tenant_id             TEXT        NOT NULL,
    legal_entity_id       TEXT        NOT NULL,

    -- Full raw event payload preserved as JSONB for queryability.
    payload               JSONB       NOT NULL,

    -- recorded_at: set by the DB clock, not the producer, for tamper-evidence.
    recorded_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT workflow_history_events_pkey PRIMARY KEY (event_id)
);

-- Primary read pattern: fetch full chronological history for one workflow instance.
-- ASC ordering returns events in emission order (first to last transition).
CREATE INDEX IF NOT EXISTS whe_instance_recorded_at_idx
    ON workflow_history_events (workflow_instance_id, recorded_at ASC);

-- Cross-workflow query pattern: list all events for a tenant+entity in a time window.
-- Used by evidence-manifest-svc's aggregator (v1 gap: aggregator not yet wired to this
-- service — see docs/architecture/known-gaps.md).
CREATE INDEX IF NOT EXISTS whe_tenant_entity_recorded_at_idx
    ON workflow_history_events (tenant_id, legal_entity_id, recorded_at ASC);

-- Tenant-only index for audit queries that span all entities within a tenant.
CREATE INDEX IF NOT EXISTS whe_tenant_recorded_at_idx
    ON workflow_history_events (tenant_id, recorded_at ASC);
