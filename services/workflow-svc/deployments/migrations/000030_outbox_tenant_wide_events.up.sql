-- Migration 000030: tenant-wide aggregates write their events with no legal entity.
--
-- outbox_events.legal_entity_id (000011) was NOT NULL, but several aggregates
-- here belong to the tenant, not to one legal entity: workflow definitions,
-- operator actions, emergency approvals. Their events were written with
-- legal_entity_id '' and the uuid cast failed, so validate / submit-approval /
-- approve / reject / publish of a definition all answered 503 (found by the
-- live test of 26 Nov 2026). The event envelope already leaves
-- legal_entity_id out when it is empty (internal/events: "omitempty, not
-- fabricated defaults"), so NULL is the honest value: an event of a
-- tenant-wide aggregate carries the tenant and no entity.

ALTER TABLE outbox_events ALTER COLUMN legal_entity_id DROP NOT NULL;

COMMENT ON COLUMN outbox_events.legal_entity_id IS
    'The legal entity the event concerns; NULL for a tenant-wide aggregate (definition, operator action).';
