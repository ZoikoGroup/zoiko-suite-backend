-- 000022: versions and an append-only history for authorization configuration.
--
-- Roles, permission bundles, SoD rules and ABAC rules were changed in place:
-- retire/reactivate flipped active_flag and a bundle repost overwrote
-- permitted_actions wholesale, leaving no record of what a role granted, or
-- which rules were in force, at the time of a past decision (Governance Control
-- Plane invariants #4 and #11; Doc 03 §8.2). And nothing could detect a
-- concurrent change, so there was no expected_version to offer (§16).
--
-- 1. Each table gets a version, bumped by trigger on every real change, so the
--    application cannot forget or forge it. The handlers accept an optional
--    expected_version and refuse a stale one with 409.
-- 2. Every insert and change appends the row's full state to
--    authz_config_history. The state in force at any instant is the latest
--    snapshot recorded at or before it, so a decision can be replayed against
--    the configuration it was actually made under.
--
-- Triggers, not application code, for the reason the version is: a write path
-- added later records history without anyone having to remember to.

ALTER TABLE roles              ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE permission_bundles ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE sod_rules          ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE abac_rules         ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS authz_config_history (
    history_id   BIGSERIAL   PRIMARY KEY,
    object_type  TEXT        NOT NULL,
    object_id    UUID        NOT NULL,
    version      BIGINT      NOT NULL,
    tenant_id    UUID,
    snapshot     JSONB       NOT NULL,
    recorded_at  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (object_type, object_id, version)
);

CREATE INDEX IF NOT EXISTS idx_authz_config_history_object
    ON authz_config_history (object_type, object_id, recorded_at);

-- History is evidence: append-only, enforced in the database.
CREATE OR REPLACE FUNCTION authz_config_history_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'authz_config_history is append-only (% refused)', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

DROP TRIGGER IF EXISTS authz_config_history_no_update ON authz_config_history;
CREATE TRIGGER authz_config_history_no_update
    BEFORE UPDATE OR DELETE ON authz_config_history
    FOR EACH ROW EXECUTE FUNCTION authz_config_history_immutable();

DROP TRIGGER IF EXISTS authz_config_history_no_truncate ON authz_config_history;
CREATE TRIGGER authz_config_history_no_truncate
    BEFORE TRUNCATE ON authz_config_history
    FOR EACH STATEMENT EXECUTE FUNCTION authz_config_history_immutable();

-- Version: a real change bumps it; a no-op update (ON CONFLICT DO UPDATE with
-- identical values) does not. The application never sets it.
CREATE OR REPLACE FUNCTION authz_config_bump_version() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.version := OLD.version;
    IF NEW IS DISTINCT FROM OLD THEN
        NEW.version := OLD.version + 1;
    END IF;
    RETURN NEW;
END;
$$;

-- History: TG_ARGV[0] names the table's id column.
CREATE OR REPLACE FUNCTION authz_config_record_history() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_row    JSONB := to_jsonb(NEW);
    v_tenant UUID  := NULLIF(v_row->>'tenant_id', '')::uuid;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.version = OLD.version THEN
        RETURN NULL; -- no-op update: nothing changed, nothing to record
    END IF;
    IF TG_TABLE_NAME = 'permission_bundles' THEN
        SELECT r.tenant_id INTO v_tenant FROM roles r WHERE r.role_id = (v_row->>'role_id')::uuid;
    END IF;
    INSERT INTO authz_config_history (object_type, object_id, version, tenant_id, snapshot)
    VALUES (TG_TABLE_NAME, (v_row->>TG_ARGV[0])::uuid, NEW.version, v_tenant, v_row);
    RETURN NULL;
END;
$$;

DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT * FROM (VALUES
        ('roles', 'role_id'),
        ('permission_bundles', 'permission_bundle_id'),
        ('sod_rules', 'sod_rule_id'),
        ('abac_rules', 'abac_rule_id')) AS v(tbl, idcol)
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I', t.tbl || '_bump_version', t.tbl);
        EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION authz_config_bump_version()',
                       t.tbl || '_bump_version', t.tbl);
        EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I', t.tbl || '_record_history', t.tbl);
        EXECUTE format('CREATE TRIGGER %I AFTER INSERT OR UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION authz_config_record_history(%L)',
                       t.tbl || '_record_history', t.tbl, t.idcol);
    END LOOP;
END
$$;

-- Backfill: every existing row's current state as its version 1, so history
-- starts complete from this migration onwards.
INSERT INTO authz_config_history (object_type, object_id, version, tenant_id, snapshot)
SELECT 'roles', r.role_id, r.version, r.tenant_id, to_jsonb(r) FROM roles r
ON CONFLICT DO NOTHING;
INSERT INTO authz_config_history (object_type, object_id, version, tenant_id, snapshot)
SELECT 'permission_bundles', pb.permission_bundle_id, pb.version, r.tenant_id, to_jsonb(pb)
  FROM permission_bundles pb LEFT JOIN roles r ON r.role_id = pb.role_id
ON CONFLICT DO NOTHING;
INSERT INTO authz_config_history (object_type, object_id, version, tenant_id, snapshot)
SELECT 'sod_rules', s.sod_rule_id, s.version, s.tenant_id, to_jsonb(s) FROM sod_rules s
ON CONFLICT DO NOTHING;
INSERT INTO authz_config_history (object_type, object_id, version, tenant_id, snapshot)
SELECT 'abac_rules', a.abac_rule_id, a.version, a.tenant_id, to_jsonb(a) FROM abac_rules a
ON CONFLICT DO NOTHING;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT ON authz_config_history TO app_authorization;
        GRANT USAGE ON SEQUENCE authz_config_history_history_id_seq TO app_authorization;
    END IF;
END
$$;
