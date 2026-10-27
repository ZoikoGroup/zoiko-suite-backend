-- Migration 000028: the assignment and policy version watermark
-- (ZS-IAM-001 §19 "cache key: subject, tenant, entity, action, resource,
-- assignment version, policy version"; Governance Platform audit row "Cache
-- key includes assignment and policy versions").
--
-- The decision cache keyed on in-process generation counters only. A write
-- through THIS replica invalidated at once; a write through another replica,
-- or straight to the database, stayed invisible until the entry's TTL ran out.
-- Every replica now reads this one row about once a second and puts both
-- numbers in every cache key, so any write anywhere makes the old entries
-- unreachable within that interval.
--
--   policy_version      bumped by every authz_config_history row (roles,
--                       bundles, SoD and ABAC rules, 000022) and every SoD
--                       exception change (000027)
--   assignment_version  bumped by assignment, delegation, principal-status
--                       and entity-status writes
--
-- Statement-level triggers: one bump per statement, not per row. The trigger
-- function is SECURITY DEFINER so the service role needs SELECT only.

CREATE TABLE IF NOT EXISTS authz_versions (
    singleton          INTEGER PRIMARY KEY DEFAULT 1 CHECK (singleton = 1),
    policy_version     BIGINT  NOT NULL DEFAULT 0,
    assignment_version BIGINT  NOT NULL DEFAULT 0
);
INSERT INTO authz_versions (singleton) VALUES (1) ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION authz_bump_version() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    IF TG_ARGV[0] = 'policy' THEN
        UPDATE authz_versions SET policy_version = policy_version + 1 WHERE singleton = 1;
    ELSE
        UPDATE authz_versions SET assignment_version = assignment_version + 1 WHERE singleton = 1;
    END IF;
    RETURN NULL;
END;
$$;

DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT * FROM (VALUES
        ('authz_config_history', 'policy'),
        ('sod_exceptions', 'policy'),
        ('principal_role_assignments', 'assignment'),
        ('delegated_authorities', 'assignment'),
        ('principal_status_projection', 'assignment'),
        ('entity_status_projection', 'assignment')) AS v(tbl, kind)
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I', t.tbl || '_bump_watermark', t.tbl);
        EXECUTE format('CREATE TRIGGER %I AFTER INSERT OR UPDATE OR DELETE ON %I FOR EACH STATEMENT EXECUTE FUNCTION authz_bump_version(%L)',
                       t.tbl || '_bump_watermark', t.tbl, t.kind);
    END LOOP;
END
$$;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT ON authz_versions TO app_authorization;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_app') THEN
        GRANT SELECT ON authz_versions TO zoiko_app;
    END IF;
END
$$;
