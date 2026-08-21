DROP INDEX IF EXISTS idx_event_schemas_owning_service;

ALTER TABLE event_schemas DROP CONSTRAINT IF EXISTS event_schemas_event_name_wellformed;
ALTER TABLE event_schemas DROP CONSTRAINT IF EXISTS event_schemas_compatibility_mode_known;
ALTER TABLE event_schemas DROP CONSTRAINT IF EXISTS event_schemas_version_positive;
ALTER TABLE event_schemas DROP CONSTRAINT IF EXISTS event_schemas_json_schema_not_empty;
ALTER TABLE event_schemas DROP CONSTRAINT IF EXISTS event_schemas_json_schema_is_object;
