-- Migration: 000011_policy_family_retirement.down.sql
--
-- Refused while any family is retired: dropping the columns would erase who
-- retired it and why.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM policies WHERE policy_status = 'RETIRED') THEN
        RAISE EXCEPTION '000011 down refused: retired policy families carry retirement evidence';
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION policies_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'policies is append-only: a policy family is never deleted'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.policy_id               IS DISTINCT FROM OLD.policy_id
       OR NEW.policy_code          IS DISTINCT FROM OLD.policy_code
       OR NEW.policy_name          IS DISTINCT FROM OLD.policy_name
       OR NEW.policy_type          IS DISTINCT FROM OLD.policy_type
       OR NEW.tenant_id            IS DISTINCT FROM OLD.tenant_id
       OR NEW.created_at           IS DISTINCT FROM OLD.created_at
       OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'policy identity is immutable'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

ALTER TABLE policies DROP CONSTRAINT IF EXISTS policies_retirement_evidenced;
ALTER TABLE policies
    DROP COLUMN IF EXISTS retirement_reason,
    DROP COLUMN IF EXISTS retired_at,
    DROP COLUMN IF EXISTS retired_by_principal_id;
