-- 000014_record_version.down.sql
--
-- Revert record_version additions for mutable aggregates.

DROP TRIGGER IF EXISTS flag_retirements_bump_version ON flag_retirements;
DROP TRIGGER IF EXISTS release_plans_bump_version ON release_plans;
DROP TRIGGER IF EXISTS kill_switches_bump_version ON kill_switches;
DROP TRIGGER IF EXISTS emergency_changes_bump_version ON emergency_changes;
DROP TRIGGER IF EXISTS config_changes_bump_version ON config_changes;
DROP TRIGGER IF EXISTS config_definitions_bump_version ON config_definitions;
DROP FUNCTION IF EXISTS bump_record_version();

ALTER TABLE flag_retirements
    DROP CONSTRAINT IF EXISTS flag_ret_version_positive;
ALTER TABLE flag_retirements
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE release_plans
    DROP CONSTRAINT IF EXISTS rel_plan_version_positive;
ALTER TABLE release_plans
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE kill_switches
    DROP CONSTRAINT IF EXISTS kill_sw_version_positive;
ALTER TABLE kill_switches
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE emergency_changes
    DROP CONSTRAINT IF EXISTS emerg_chg_version_positive;
ALTER TABLE emergency_changes
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE config_changes
    DROP CONSTRAINT IF EXISTS config_chg_version_positive;
ALTER TABLE config_changes
    DROP COLUMN IF EXISTS record_version;

ALTER TABLE config_definitions
    DROP CONSTRAINT IF EXISTS config_def_version_positive;
ALTER TABLE config_definitions
    DROP COLUMN IF EXISTS record_version;
