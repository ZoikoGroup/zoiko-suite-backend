-- 000004_add_control_tests.down.sql
-- Drops all objects created by 000004_add_control_tests.up.sql in reverse order.

DROP TABLE IF EXISTS attestations;
DROP TABLE IF EXISTS control_test_executions;
DROP TABLE IF EXISTS control_test_definitions;
