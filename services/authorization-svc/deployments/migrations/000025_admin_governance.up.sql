-- 000025: maker-checker on privileged grants, delegation purpose, and who/why
-- on every configuration change.
--
-- 1. principal_role_assignments.approval_status. An assignment of a role that
--    carries IAM or platform-administration permissions, made by somebody who
--    does not hold iam.assignment.approve_privileged, is PENDING_APPROVAL and
--    grants nothing until an independent approver approves it (ZS-IAM-001 §9
--    "Assignment request ... security approval depending on risk", A20
--    "protected assignment enters required approval path", GOV-04 negative
--    path #2 "user cannot approve own access elevation", GOV-12 checker
--    states Pending / Approved / Rejected / Expired). Every existing row is
--    APPROVED: nothing that grants today stops granting.
--
-- 2. delegated_authorities.reason / approval_reference (ZS-IAM-001 §11:
--    "reason — absence, named operational cover or other controlled purpose";
--    "approval_reference — required where policy says").
--
-- 3. authz_config_history.changed_by / correlation_id / reason. The store sets
--    app.actor_id, app.correlation_id and app.reason with app.tenant_id on
--    every transaction, and the history trigger records them, so a change is
--    attributable and its purpose recorded (§16 "purpose / reason_code").

ALTER TABLE principal_role_assignments
    ADD COLUMN IF NOT EXISTS approval_status     VARCHAR(32) NOT NULL DEFAULT 'APPROVED',
    ADD COLUMN IF NOT EXISTS approved_by         TEXT,
    ADD COLUMN IF NOT EXISTS approval_reference  TEXT,
    ADD COLUMN IF NOT EXISTS approval_decided_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS approval_expires_at TIMESTAMPTZ;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'pra_approval_status_check') THEN
        ALTER TABLE principal_role_assignments
            ADD CONSTRAINT pra_approval_status_check
            CHECK (approval_status IN ('APPROVED', 'PENDING_APPROVAL', 'REJECTED', 'EXPIRED'));
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS idx_pra_pending_approval
    ON principal_role_assignments (approval_expires_at)
    WHERE approval_status = 'PENDING_APPROVAL';

ALTER TABLE delegated_authorities
    ADD COLUMN IF NOT EXISTS reason             TEXT,
    ADD COLUMN IF NOT EXISTS approval_reference TEXT;

ALTER TABLE authz_config_history
    ADD COLUMN IF NOT EXISTS changed_by     TEXT,
    ADD COLUMN IF NOT EXISTS correlation_id TEXT,
    ADD COLUMN IF NOT EXISTS reason         TEXT;

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
    INSERT INTO authz_config_history (object_type, object_id, version, tenant_id, snapshot, changed_by, correlation_id, reason)
    VALUES (TG_TABLE_NAME, (v_row->>TG_ARGV[0])::uuid, NEW.version, v_tenant, v_row,
            NULLIF(current_setting('app.actor_id', true), ''),
            NULLIF(current_setting('app.correlation_id', true), ''),
            NULLIF(current_setting('app.reason', true), ''));
    RETURN NULL;
END;
$$;
