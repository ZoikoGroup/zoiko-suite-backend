-- Reverts 000034. Refused while a break-glass session is pending or was
-- rejected: the 000015 status CHECK has no such states, and quietly turning a
-- pending request into an ACTIVE one would grant what nobody approved.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM break_glass_sessions WHERE status IN ('PENDING_APPROVAL', 'REJECTED')) THEN
        RAISE EXCEPTION '000034 down refused: pending or rejected break-glass sessions exist';
    END IF;
END
$$;

DROP INDEX IF EXISTS idx_access_decision_log_session;
ALTER TABLE access_decision_log DROP CONSTRAINT IF EXISTS access_decision_log_session_kind_check;
ALTER TABLE access_decision_log
    DROP COLUMN IF EXISTS session_kind,
    DROP COLUMN IF EXISTS session_id;

DROP TRIGGER IF EXISTS break_glass_approvals_events ON break_glass_approvals;
DROP FUNCTION IF EXISTS authz_break_glass_approval_events();
DROP TRIGGER IF EXISTS break_glass_approvals_no_change ON break_glass_approvals;
DROP FUNCTION IF EXISTS authz_break_glass_approvals_immutable();
DROP TRIGGER IF EXISTS break_glass_approvals_independent ON break_glass_approvals;
DROP FUNCTION IF EXISTS authz_break_glass_approval_independent();
DROP TABLE IF EXISTS break_glass_approvals;

-- The 000030 event function, without requested / rejected.
CREATE OR REPLACE FUNCTION authz_session_events() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_prefix  TEXT;
    v_subject TEXT;
    v_payload JSONB;
BEGIN
    IF TG_TABLE_NAME = 'privileged_sessions' THEN
        v_prefix := 'security.privileged_session';
        v_subject := NEW.principal_id;
    ELSIF TG_TABLE_NAME = 'break_glass_sessions' THEN
        v_prefix := 'security.break_glass';
        v_subject := NEW.principal_id;
    ELSE
        v_prefix := 'support.session';
        v_subject := NEW.support_operator_id;
    END IF;
    v_payload := to_jsonb(NEW) || jsonb_build_object('session_kind', TG_TABLE_NAME, 'subject_id', v_subject);

    IF NEW.status = 'ACTIVE' AND (TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM 'ACTIVE') THEN
        PERFORM authz_emit_event(v_prefix || '.started', NEW.session_id::text, NEW.tenant_id, NULL, v_payload);
    ELSIF TG_OP = 'UPDATE' AND OLD.status = 'ACTIVE' AND NEW.status IN ('REVOKED', 'EXPIRED') THEN
        PERFORM authz_emit_event(v_prefix || '.ended', NEW.session_id::text, NEW.tenant_id, NULL, v_payload);
    END IF;
    RETURN NULL;
END;
$$;

ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_independent_rejection;
ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_independent_approval;
ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_approvals_required_check;
ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_status_check;
ALTER TABLE break_glass_sessions ADD CONSTRAINT break_glass_sessions_status_check
    CHECK (status IN ('ACTIVE', 'REVOKED', 'EXPIRED'));
ALTER TABLE break_glass_sessions
    DROP COLUMN IF EXISTS request_expires_at,
    DROP COLUMN IF EXISTS rejected_at,
    DROP COLUMN IF EXISTS rejected_by,
    DROP COLUMN IF EXISTS approved_at,
    DROP COLUMN IF EXISTS approved_by,
    DROP COLUMN IF EXISTS approvals_required;
