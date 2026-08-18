-- The 000001 down migration was missing entirely, so there was no way to roll
-- the schema back to nothing — every other service in this platform ships a
-- matching pair, and 000002 already had one.
DROP INDEX IF EXISTS idx_event_schemas_event_name;
DROP TABLE IF EXISTS event_schemas;
