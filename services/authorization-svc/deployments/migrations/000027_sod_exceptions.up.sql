-- 000027: GOV-04 compensating-control exceptions.
--
-- GOV-04 owns "SoDPolicy, SoDConflictDecision, compensating-control
-- reference", with the exception lifecycle Requested / Approved / Active /
-- Expired / Revoked; "exceptions cannot be self-approved and always expire";
-- "compensating control approval restricted and independently reviewed";
-- negative path #3 "expired exception immediately stops authorizing
-- conflict". Doc 03 §8.3 places "SoD evaluation results" in this service, and
-- nothing in the estate implemented an exception at all — a static conflict
-- could only ever be resolved by removing a grant.
--
-- status: REQUESTED -> APPROVED | REJECTED; APPROVED -> REVOKED | EXPIRED.
-- "Active" is APPROVED with effective_from <= now < expires_at, evaluated at
-- decision time, so expiry takes effect the instant it passes, before the
-- sweeper marks the row.
CREATE TABLE IF NOT EXISTS sod_exceptions (
    sod_exception_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID         NOT NULL,
    sod_rule_id          UUID         NOT NULL REFERENCES sod_rules (sod_rule_id),
    principal_id         TEXT         NOT NULL,
    compensating_control TEXT         NOT NULL,
    reason               TEXT         NOT NULL,
    status               VARCHAR(16)  NOT NULL DEFAULT 'REQUESTED'
                         CHECK (status IN ('REQUESTED', 'APPROVED', 'REJECTED', 'REVOKED', 'EXPIRED')),
    requested_by         TEXT         NOT NULL,
    approved_by          TEXT,
    decided_at           TIMESTAMPTZ,
    effective_from       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at           TIMESTAMPTZ  NOT NULL,
    revoked_by           TEXT,
    revoked_at           TIMESTAMPTZ,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (expires_at > effective_from),
    -- Never self-approved: not by the subject, not by the requester.
    CHECK (approved_by IS NULL OR (approved_by <> principal_id AND approved_by <> requested_by))
);

CREATE INDEX IF NOT EXISTS idx_sod_exceptions_active
    ON sod_exceptions (tenant_id, principal_id, sod_rule_id)
    WHERE status = 'APPROVED';

ALTER TABLE sod_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sod_exceptions FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON sod_exceptions;
CREATE POLICY tenant_isolation_policy ON sod_exceptions
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- GOV-04 events: CompensatingControlApproved, SoDExceptionExpired, and the
-- revocation, through the outbox with the change.
CREATE OR REPLACE FUNCTION authz_sod_exception_events() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_type TEXT;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.status IS DISTINCT FROM OLD.status THEN
        v_type := CASE NEW.status
            WHEN 'APPROVED' THEN 'sod.compensating_control.approved'
            WHEN 'EXPIRED'  THEN 'sod.exception.expired'
            WHEN 'REVOKED'  THEN 'sod.compensating_control.revoked'
        END;
    END IF;
    IF v_type IS NOT NULL THEN
        PERFORM authz_emit_event(v_type, NEW.sod_exception_id::text, NEW.tenant_id, NULL, jsonb_build_object(
            'sod_exception_id', NEW.sod_exception_id, 'sod_rule_id', NEW.sod_rule_id,
            'principal_id', NEW.principal_id, 'compensating_control', NEW.compensating_control,
            'reason', NEW.reason, 'status', NEW.status, 'requested_by', NEW.requested_by,
            'approved_by', NEW.approved_by, 'effective_from', NEW.effective_from, 'expires_at', NEW.expires_at,
            'revoked_by', NEW.revoked_by));
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS sod_exception_events ON sod_exceptions;
CREATE TRIGGER sod_exception_events
    AFTER UPDATE ON sod_exceptions
    FOR EACH ROW EXECUTE FUNCTION authz_sod_exception_events();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT, UPDATE ON sod_exceptions TO app_authorization;
    END IF;
END
$$;
