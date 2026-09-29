-- 000011_add_com04_usage_metering.down.sql

DROP TABLE IF EXISTS usage_adjustments;
DROP TABLE IF EXISTS usage_event_records;
DROP TABLE IF EXISTS usage_statements;
DROP TABLE IF EXISTS meter_definitions;

DROP FUNCTION IF EXISTS enforce_usage_event_correct_only();
DROP FUNCTION IF EXISTS enforce_usage_statement_lifecycle();
DROP FUNCTION IF EXISTS enforce_meter_definition_retire_only();
-- reject_immutable_row() is owned by migration 000010 and stays.
