-- Migration: 000011_policy_family_retirement.up.sql
--
-- A policy family can be retired (Doc 04 §7.1 policy_status). Retirement is
-- evidenced on the row (who, when, why) and is final: the guard refuses any
-- change to a retired family's status or retirement evidence, so a retired
-- family can never quietly take new versions again.

ALTER TABLE policies
    ADD COLUMN retired_by_principal_id TEXT,
    ADD COLUMN retired_at              TIMESTAMPTZ,
    ADD COLUMN retirement_reason       TEXT;

ALTER TABLE policies
    ADD CONSTRAINT policies_retirement_evidenced CHECK (
        policy_status <> 'RETIRED'
        OR (retired_by_principal_id IS NOT NULL AND retired_at IS NOT NULL AND retirement_reason IS NOT NULL)
    );

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
    IF OLD.policy_status = 'RETIRED' AND (
           NEW.policy_status              IS DISTINCT FROM OLD.policy_status
        OR NEW.retired_by_principal_id IS DISTINCT FROM OLD.retired_by_principal_id
        OR NEW.retired_at              IS DISTINCT FROM OLD.retired_at
        OR NEW.retirement_reason       IS DISTINCT FROM OLD.retirement_reason) THEN
        RAISE EXCEPTION 'a retired policy family stays retired'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
