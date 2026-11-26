-- Reverts 000030. Refused while an event of a tenant-wide aggregate (NULL
-- legal entity) is in the outbox: it has no entity to give it.

COMMENT ON COLUMN outbox_events.legal_entity_id IS NULL;
ALTER TABLE outbox_events ALTER COLUMN legal_entity_id SET NOT NULL;
