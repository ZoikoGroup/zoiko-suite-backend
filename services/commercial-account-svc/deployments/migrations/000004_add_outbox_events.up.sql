-- 000004_add_outbox_events.up.sql
-- Commercial Account Service — transactional outbox (doc7 backlog item 32)
--
-- Problem this closes: every service in this platform (including this
-- one) currently publishes Kafka events AFTER its business-data
-- transaction commits, as a separate call. A crash between the commit
-- and the publish call silently drops the event — the database has the
-- new fact, but nothing downstream (billing reconciliation, analytics,
-- other services subscribed to commercial_subscription.created) ever
-- hears about it.
--
-- The fix: write the event to THIS table inside the SAME transaction as
-- the business write. Either both commit or neither does — there is no
-- window where the business fact exists but the event doesn't. A
-- separate background relay (internal/outbox) then polls unpublished
-- rows and publishes them to Kafka, marking them published only after a
-- successful publish.
--
-- This migration and internal/outbox are a PILOT on this service's
-- highest-value write path (CreateSubscription) — proving the pattern
-- end-to-end, not a platform-wide rollout. Every other write path in this
-- service, and every other service in the fleet, still publishes directly
-- and carries the same crash-window risk; closing that fully is
-- deliberately left as incremental follow-up work, tracked in
-- docs/architecture/doc7-implementation-backlog.md item 32.

CREATE TABLE outbox_events (
    outbox_event_id      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Free-text aggregate type/id — DATA ONLY, e.g. "commercial_subscription".
    aggregate_type         VARCHAR(128) NOT NULL,
    aggregate_id             VARCHAR(128) NOT NULL,

    event_type                 VARCHAR(128) NOT NULL,
    payload                       JSONB       NOT NULL,
    tenant_id                       UUID,

    created_at                         TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- NULL = not yet published. Set exactly once, on a successful publish.
    published_at                          TIMESTAMPTZ,
    publish_attempts                         INTEGER     NOT NULL DEFAULT 0,
    last_error                                  TEXT
);

-- Relay's primary query: unpublished rows, oldest first.
CREATE INDEX idx_outbox_events_unpublished
    ON outbox_events (created_at ASC)
    WHERE published_at IS NULL;
