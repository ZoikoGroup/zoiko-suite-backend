-- Restore 000022's history function (no actor / correlation / reason).
CREATE OR REPLACE FUNCTION authz_config_record_history() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_row    JSONB := to_jsonb(NEW);
    v_tenant UUID  := NULLIF(v_row->>'tenant_id', '')::uuid;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.version = OLD.version THEN
        RETURN NULL;
    END IF;
    IF TG_TABLE_NAME = 'permission_bundles' THEN
        SELECT r.tenant_id INTO v_tenant FROM roles r WHERE r.role_id = (v_row->>'role_id')::uuid;
    END IF;
    INSERT INTO authz_config_history (object_type, object_id, version, tenant_id, snapshot)
    VALUES (TG_TABLE_NAME, (v_row->>TG_ARGV[0])::uuid, NEW.version, v_tenant, v_row);
    RETURN NULL;
END;
$$;

ALTER TABLE authz_config_history
    DROP COLUMN IF EXISTS changed_by,
    DROP COLUMN IF EXISTS correlation_id,
    DROP COLUMN IF EXISTS reason;

ALTER TABLE delegated_authorities
    DROP COLUMN IF EXISTS reason,
    DROP COLUMN IF EXISTS approval_reference;

DROP INDEX IF EXISTS idx_pra_pending_approval;
ALTER TABLE principal_role_assignments
    DROP CONSTRAINT IF EXISTS pra_approval_status_check,
    DROP COLUMN IF EXISTS approval_status,
    DROP COLUMN IF EXISTS approved_by,
    DROP COLUMN IF EXISTS approval_reference,
    DROP COLUMN IF EXISTS approval_decided_at,
    DROP COLUMN IF EXISTS approval_expires_at;
