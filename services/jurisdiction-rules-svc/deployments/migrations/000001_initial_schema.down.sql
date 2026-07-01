-- 000001_initial_schema.down.sql
-- Drops all objects created by 000001_initial_schema.up.sql in reverse order.

DROP TABLE IF EXISTS jurisdiction_rule_drift_events;
DROP TABLE IF EXISTS jurisdiction_rules;
DROP TABLE IF EXISTS jurisdictions;
