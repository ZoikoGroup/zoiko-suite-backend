-- Migration: 000006_operations.down.sql

DROP POLICY IF EXISTS tenant_isolation_policy ON emergency_changes;
DROP POLICY IF EXISTS tenant_isolation_policy ON config_changes;
DROP POLICY IF EXISTS tenant_isolation_policy ON kill_switches;

DROP TABLE IF EXISTS emergency_changes;
DROP TABLE IF EXISTS config_changes;
DROP TABLE IF EXISTS kill_switches;