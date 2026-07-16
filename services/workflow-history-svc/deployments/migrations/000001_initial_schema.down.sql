-- Reverses 000001_initial_schema.up.sql.
-- Drops indexes first (implicit via DROP TABLE, but explicit for clarity),
-- then the table.
-- NOTE: this migration should never be run in production — workflow history
-- is immutable evidence and must not be destroyed.
DROP TABLE IF EXISTS workflow_history_events;
