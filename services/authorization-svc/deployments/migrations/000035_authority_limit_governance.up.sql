-- 000035: approval and signing authority limits become a governed register
-- (AU-03; ZS-IAM-001 §12, §21-23, §8 "no direct runtime mutation without
-- controlled publication").
--
-- Every approval decision evaluated these limits, and nothing could create or
-- change one: CreateAuthorityLimit had no caller and POST
-- /v1/admin/authority-limits answered 404, so a limit existed only by direct
-- INSERT and iam.authority_limit.changed could never fire.
--
-- A limit is now a sequence of governed versions:
--
--   create  -> a PENDING_APPROVAL row; it limits nothing until approved.
--   change  -> a NEW PENDING_APPROVAL row naming the one it supersedes; the
--              old one stays in force until the change is approved, then is
--              SUPERSEDED (its effective_to set) in the same transaction.
--   retire  -> pending_action = 'RETIRE' on the ACTIVE row, which keeps
--              limiting until a checker approves the retirement.
--   approve -> by someone other than the requester (CHECK + trigger + handler).
--   reject  -> a pending row becomes REJECTED; a pending retirement is cleared.
--
-- Rows that exist before this migration were inserted directly and have been
-- enforced all along; they are ACTIVE (the default), so evaluation does not
-- change for anybody until a change is requested and approved.
--
-- Version and history ride on the 000022 triggers; iam.authority_limit.changed
-- is emitted when a version takes or leaves effect.

ALTER TABLE authority_limits
    ADD COLUMN IF NOT EXISTS status         VARCHAR(20) NOT NULL DEFAULT 'ACTIVE',
    ADD COLUMN IF NOT EXISTS pending_action VARCHAR(16),
    ADD COLUMN IF NOT EXISTS supersedes_id  UUID REFERENCES authority_limits (authority_limit_id),
    ADD COLUMN IF NOT EXISTS requested_by   TEXT,
    ADD COLUMN IF NOT EXISTS requested_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS request_reason TEXT,
    ADD COLUMN IF NOT EXISTS decided_by     TEXT,
    ADD COLUMN IF NOT EXISTS decided_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_decision  VARCHAR(10),
    ADD COLUMN IF NOT EXISTS version        BIGINT      NOT NULL DEFAULT 1;

ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_last_decision_check;
ALTER TABLE authority_limits ADD CONSTRAINT authority_limits_last_decision_check
    CHECK (last_decision IS NULL OR last_decision IN ('APPROVED', 'REJECTED'));

ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_status_check;
ALTER TABLE authority_limits ADD CONSTRAINT authority_limits_status_check
    CHECK (status IN ('PENDING_APPROVAL', 'ACTIVE', 'REJECTED', 'RETIRED', 'SUPERSEDED'));
ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_pending_action_check;
ALTER TABLE authority_limits ADD CONSTRAINT authority_limits_pending_action_check
    CHECK (pending_action IS NULL OR (pending_action = 'RETIRE' AND status = 'ACTIVE'));
ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_bounds_check;
ALTER TABLE authority_limits ADD CONSTRAINT authority_limits_bounds_check
    CHECK (lower_limit >= 0 AND upper_limit >= lower_limit) NOT VALID;

-- One open change per limit at a time: two pending successors of the same row
-- would both claim to replace it.
CREATE UNIQUE INDEX IF NOT EXISTS uq_authority_limits_one_pending_change
    ON authority_limits (supersedes_id)
    WHERE status = 'PENDING_APPROVAL' AND supersedes_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_authority_limits_status
    ON authority_limits (tenant_id, status);

-- The checker is never the maker. Enforced when a decision is recorded
-- (decided_at changes), so the row's later history cannot trip it.
CREATE OR REPLACE FUNCTION authz_authority_limit_independent() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.decided_at IS DISTINCT FROM OLD.decided_at AND NEW.decided_by IS NOT NULL
       AND NEW.decided_by IS NOT DISTINCT FROM OLD.requested_by THEN
        RAISE EXCEPTION 'an authority limit change is decided by someone other than its requester'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS authority_limits_independent ON authority_limits;
CREATE TRIGGER authority_limits_independent
    BEFORE UPDATE ON authority_limits
    FOR EACH ROW EXECUTE FUNCTION authz_authority_limit_independent();

-- Version + history, exactly as for roles, bundles, SoD and ABAC rules (000022).
DROP TRIGGER IF EXISTS authority_limits_bump_version ON authority_limits;
CREATE TRIGGER authority_limits_bump_version
    BEFORE UPDATE ON authority_limits
    FOR EACH ROW EXECUTE FUNCTION authz_config_bump_version();
DROP TRIGGER IF EXISTS authority_limits_record_history ON authority_limits;
CREATE TRIGGER authority_limits_record_history
    AFTER INSERT OR UPDATE ON authority_limits
    FOR EACH ROW EXECUTE FUNCTION authz_config_record_history('authority_limit_id');

INSERT INTO authz_config_history (object_type, object_id, version, tenant_id, snapshot)
SELECT 'authority_limits', a.authority_limit_id, a.version, a.tenant_id, to_jsonb(a) FROM authority_limits a
ON CONFLICT DO NOTHING;

-- iam.authority_limit.changed (§23 "approval/signing authority changed"): when
-- a version takes effect (ACTIVE) or leaves it (RETIRED / SUPERSEDED).
CREATE OR REPLACE FUNCTION authz_authority_limit_events() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IN ('ACTIVE', 'RETIRED', 'SUPERSEDED')
       AND (TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM NEW.status) THEN
        PERFORM authz_emit_event('iam.authority_limit.changed', NEW.authority_limit_id::text, NEW.tenant_id,
            NEW.legal_entity_id::text,
            to_jsonb(NEW) || jsonb_build_object('change', lower(NEW.status)));
    END IF;
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS authority_limits_events ON authority_limits;
CREATE TRIGGER authority_limits_events
    AFTER INSERT OR UPDATE ON authority_limits
    FOR EACH ROW EXECUTE FUNCTION authz_authority_limit_events();

-- The cache watermark (000028) moves on a limit change like any policy change.
DROP TRIGGER IF EXISTS authority_limits_bump_watermark ON authority_limits;
CREATE TRIGGER authority_limits_bump_watermark
    AFTER INSERT OR UPDATE OR DELETE ON authority_limits
    FOR EACH STATEMENT EXECUTE FUNCTION authz_bump_version('policy');

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT, UPDATE ON authority_limits TO app_authorization;
    END IF;
END
$$;
