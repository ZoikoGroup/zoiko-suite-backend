-- audit_events: append-only immutable store for all platform domain events.
--
-- Design decisions (see docs/architecture/01-backend.md §8.2 and doctrine.md):
--
--   1. Single table for all event types.  Both entity.status.changed and
--      identity.context.resolved share the same structural shape (event_id,
--      event_type, tenant/entity context, payload).  A JSONB payload column
--      accommodates per-type fields without schema proliferation.
--
--   2. event_id is the dedup key (PRIMARY KEY → unique index).  Consumers
--      issue:  INSERT … ON CONFLICT (event_id) DO NOTHING
--      This is the only safe dedup pattern — a prior SELECT EXISTS() check
--      allows a duplicate insertion race under concurrent delivery.
--
--   3. No UPDATE or DELETE permitted.  The table is append-only by design.
--      No soft-delete, no status column.  Evidence is immutable.
--
--   4. Every row carries tenant_id and legal_entity_id per doctrine §3.2
--      (data-model §3.2: every material record carries these fields).
--      principal_id is nullable — some platform events originate from a
--      system actor with no human principal.
--
--   5. stored_at is set by the database clock, not by the event payload,
--      to prevent clock skew from producer services distorting insertion order.
--      emitted_at (from the event envelope) is preserved in the payload JSONB.

CREATE TABLE IF NOT EXISTS audit_events (
    -- event_id is the globally unique identifier assigned by the publishing
    -- service for this specific event occurrence.  It is the dedup key.
    event_id            TEXT        NOT NULL,

    -- event_type mirrors the event name (e.g. "identity.context.resolved").
    event_type          TEXT        NOT NULL,

    -- Mandatory governance context per doctrine.
    tenant_id           TEXT        NOT NULL,
    legal_entity_id     TEXT        NOT NULL,

    -- principal_id: nullable — system-level events may not have a human actor.
    principal_id        TEXT,

    -- Provenance fields copied from the event envelope.
    source_service      TEXT        NOT NULL,
    schema_version      TEXT        NOT NULL,

    -- Full event payload preserved as JSONB for queryability.
    payload             JSONB       NOT NULL,

    -- stored_at: set by the DB, not the producer, for tamper-evidence.
    stored_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT audit_events_pkey PRIMARY KEY (event_id)
);

-- Covering index for the most common audit query pattern: retrieve all events
-- for a tenant ordered by insertion time.
CREATE INDEX IF NOT EXISTS audit_events_tenant_stored_at_idx
    ON audit_events (tenant_id, stored_at DESC);

-- Index for event_type-based filtering (e.g. "give me all ContextResolved events").
CREATE INDEX IF NOT EXISTS audit_events_event_type_idx
    ON audit_events (event_type);
