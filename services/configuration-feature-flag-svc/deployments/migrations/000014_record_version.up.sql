-- 000014_record_version.up.sql
--
-- Add optimistic-concurrency version (record_version) to mutable aggregates
-- in configuration-feature-flag-svc. API-3 requires ETag/If-Match on every
-- aggregate mutation; GET must return ETag, and mutations require If-Match.
--
-- runtime_attestations is append-only (an observation is never updated), so
-- it carries no version.
--
-- The version is bumped by a trigger on every UPDATE rather than by each
-- store statement: approve, activate, the expiry sweeps and the retrospective
-- all update these rows, and a path that forgot to bump would leave the ETag
-- unchanged, so a stale If-Match would pass.

ALTER TABLE config_definitions
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE config_changes
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE emergency_changes
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE kill_switches
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE release_plans
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE flag_retirements
    ADD COLUMN record_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE config_definitions
    ADD CONSTRAINT config_def_version_positive CHECK (record_version > 0);

ALTER TABLE config_changes
    ADD CONSTRAINT config_chg_version_positive CHECK (record_version > 0);

ALTER TABLE emergency_changes
    ADD CONSTRAINT emerg_chg_version_positive CHECK (record_version > 0);

ALTER TABLE kill_switches
    ADD CONSTRAINT kill_sw_version_positive CHECK (record_version > 0);

ALTER TABLE release_plans
    ADD CONSTRAINT rel_plan_version_positive CHECK (record_version > 0);

ALTER TABLE flag_retirements
    ADD CONSTRAINT flag_ret_version_positive CHECK (record_version > 0);

CREATE OR REPLACE FUNCTION bump_record_version() RETURNS trigger AS $$
BEGIN
    NEW.record_version := OLD.record_version + 1;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER config_definitions_bump_version
    BEFORE UPDATE ON config_definitions
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();

CREATE TRIGGER config_changes_bump_version
    BEFORE UPDATE ON config_changes
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();

CREATE TRIGGER emergency_changes_bump_version
    BEFORE UPDATE ON emergency_changes
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();

CREATE TRIGGER kill_switches_bump_version
    BEFORE UPDATE ON kill_switches
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();

CREATE TRIGGER release_plans_bump_version
    BEFORE UPDATE ON release_plans
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();

CREATE TRIGGER flag_retirements_bump_version
    BEFORE UPDATE ON flag_retirements
    FOR EACH ROW EXECUTE FUNCTION bump_record_version();
