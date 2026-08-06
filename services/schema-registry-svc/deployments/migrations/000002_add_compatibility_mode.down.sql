-- 000002_add_compatibility_mode.down.sql

DROP INDEX IF EXISTS idx_event_schemas_compatibility_mode;

ALTER TABLE event_schemas DROP COLUMN IF EXISTS owning_service;
ALTER TABLE event_schemas DROP COLUMN IF EXISTS compatibility_mode;
