-- 000039: a version on workload bindings (API-3).
--
-- Deactivate / reactivate changed a binding with nothing to make the change
-- conditional on. version is bumped by the 000022 function on every real
-- change — a re-save with identical values is not one — so a deactivation can
-- carry If-Match and be refused when the binding moved under it.

ALTER TABLE workload_bindings
    ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

DROP TRIGGER IF EXISTS workload_bindings_bump_version ON workload_bindings;
CREATE TRIGGER workload_bindings_bump_version
    BEFORE UPDATE ON workload_bindings
    FOR EACH ROW EXECUTE FUNCTION authz_config_bump_version();
