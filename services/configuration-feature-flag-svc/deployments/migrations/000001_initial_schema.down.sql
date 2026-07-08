-- 000001_initial_schema.down.sql
-- Drops all objects created by 000001_initial_schema.up.sql in reverse order.

DROP TABLE IF EXISTS feature_flags;
DROP TABLE IF EXISTS config_entries;
