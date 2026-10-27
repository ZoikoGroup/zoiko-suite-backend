-- 000005_event_schemas_immutability.down.sql

DROP TRIGGER IF EXISTS trg_event_schemas_reject_truncate ON event_schemas;
DROP TRIGGER IF EXISTS trg_event_schemas_reject_delete ON event_schemas;
DROP TRIGGER IF EXISTS trg_event_schemas_reject_update ON event_schemas;
DROP FUNCTION IF EXISTS reject_event_schema_mutation();
