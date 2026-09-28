-- 000015_add_com05_dunning_reconciliation.down.sql
DROP TABLE IF EXISTS commercial_reconciliations;
DROP TABLE IF EXISTS dunning_cases;
DROP FUNCTION IF EXISTS enforce_dunning_case_lifecycle();
DROP TABLE IF EXISTS dunning_policy_versions;
