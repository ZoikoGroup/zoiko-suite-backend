-- 000014_record_version.up.sql
--
-- Add optimistic-concurrency version (record_version) to mutable aggregates
-- in configuration-feature-flag-svc. API-3 requires ETag/If-Match on every
-- aggregate mutation; GET must return ETag, and mutations require If-Match.

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

ALTER TABLE attestation_results
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

ALTER TABLE attestation_results
    ADD CONSTRAINT attest_res_version_positive CHECK (record_version > 0);