-- 000030: JIT, break-glass and support session controls (ZS-IAM-001 §13-15, §23).
--
-- The final re-audit (12 Oct 2026) found these sessions grant actions at
-- decision time with none of the standard's controls: a holder of
-- iam.pam.manage could issue themselves a ["*"] session and act on it at once,
-- nothing alerted on break-glass, nothing was reviewed afterwards, and no
-- lifecycle event was published. This migration holds the state the handler
-- now enforces:
--
--   privileged_sessions  requested_by, resource_scope, target_environment
--                        (§13 "mandatory"), high_impact, approval
--                        (PENDING_APPROVAL -> ACTIVE | REJECTED, approved_by
--                        never the subject or the requester — CHECK), and
--                        post-session review.
--   break_glass_sessions requested_by and post-session review (always
--                        required, §14 "mandatory post-event review").
--   support_sessions     requested_by.
--
-- Review becomes PENDING when a high-impact JIT session or any break-glass
-- session ends (trigger), and the reviewer is never the subject (CHECK).
-- The §23 events security.privileged_session.started/.ended,
-- security.break_glass.started/.ended and support.session.started/.ended are
-- emitted from triggers into the outbox, like the 000026 IAM events.

-- ── privileged_sessions ─────────────────────────────────────────────────────
ALTER TABLE privileged_sessions
    ADD COLUMN IF NOT EXISTS requested_by        TEXT,
    ADD COLUMN IF NOT EXISTS resource_scope      TEXT,
    ADD COLUMN IF NOT EXISTS target_environment  TEXT,
    ADD COLUMN IF NOT EXISTS high_impact         BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS approved_by         TEXT,
    ADD COLUMN IF NOT EXISTS approved_at         TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS review_status       VARCHAR(16) NOT NULL DEFAULT 'NOT_REQUIRED',
    ADD COLUMN IF NOT EXISTS reviewed_by         TEXT,
    ADD COLUMN IF NOT EXISTS reviewed_at         TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS review_notes        TEXT;

ALTER TABLE privileged_sessions DROP CONSTRAINT IF EXISTS privileged_sessions_status_check;
ALTER TABLE privileged_sessions ADD CONSTRAINT privileged_sessions_status_check
    CHECK (status IN ('PENDING_APPROVAL', 'ACTIVE', 'REJECTED', 'REVOKED', 'EXPIRED'));
ALTER TABLE privileged_sessions DROP CONSTRAINT IF EXISTS privileged_sessions_review_check;
ALTER TABLE privileged_sessions ADD CONSTRAINT privileged_sessions_review_check
    CHECK (review_status IN ('NOT_REQUIRED', 'PENDING', 'ATTESTED', 'FLAGGED'));
-- §13 "independent approval": never the subject, never the requester.
ALTER TABLE privileged_sessions DROP CONSTRAINT IF EXISTS privileged_sessions_independent_approval;
ALTER TABLE privileged_sessions ADD CONSTRAINT privileged_sessions_independent_approval
    CHECK (approved_by IS NULL OR (approved_by <> principal_id AND approved_by IS DISTINCT FROM requested_by));
ALTER TABLE privileged_sessions DROP CONSTRAINT IF EXISTS privileged_sessions_independent_review;
ALTER TABLE privileged_sessions ADD CONSTRAINT privileged_sessions_independent_review
    CHECK (reviewed_by IS NULL OR reviewed_by <> principal_id);

-- ── break_glass_sessions ────────────────────────────────────────────────────
ALTER TABLE break_glass_sessions
    ADD COLUMN IF NOT EXISTS requested_by   TEXT,
    ADD COLUMN IF NOT EXISTS review_status  VARCHAR(16) NOT NULL DEFAULT 'NOT_REQUIRED',
    ADD COLUMN IF NOT EXISTS reviewed_by    TEXT,
    ADD COLUMN IF NOT EXISTS reviewed_at    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS review_notes   TEXT;
ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_review_check;
ALTER TABLE break_glass_sessions ADD CONSTRAINT break_glass_sessions_review_check
    CHECK (review_status IN ('NOT_REQUIRED', 'PENDING', 'ATTESTED', 'FLAGGED'));
ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_independent_review;
ALTER TABLE break_glass_sessions ADD CONSTRAINT break_glass_sessions_independent_review
    CHECK (reviewed_by IS NULL OR reviewed_by <> principal_id);

-- ── support_sessions ────────────────────────────────────────────────────────
ALTER TABLE support_sessions
    ADD COLUMN IF NOT EXISTS requested_by TEXT;

-- ── review opens when the session ends ──────────────────────────────────────
CREATE OR REPLACE FUNCTION authz_session_review_on_end() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'ACTIVE' AND NEW.status IN ('REVOKED', 'EXPIRED') AND NEW.review_status = 'NOT_REQUIRED' THEN
        IF TG_TABLE_NAME = 'break_glass_sessions' THEN
            NEW.review_status := 'PENDING';
        ELSIF TG_TABLE_NAME = 'privileged_sessions' AND NEW.high_impact THEN
            NEW.review_status := 'PENDING';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS privileged_sessions_review_on_end ON privileged_sessions;
CREATE TRIGGER privileged_sessions_review_on_end
    BEFORE UPDATE ON privileged_sessions
    FOR EACH ROW EXECUTE FUNCTION authz_session_review_on_end();
DROP TRIGGER IF EXISTS break_glass_sessions_review_on_end ON break_glass_sessions;
CREATE TRIGGER break_glass_sessions_review_on_end
    BEFORE UPDATE ON break_glass_sessions
    FOR EACH ROW EXECUTE FUNCTION authz_session_review_on_end();

-- ── §23 lifecycle events ────────────────────────────────────────────────────
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

DROP TRIGGER IF EXISTS privileged_sessions_events ON privileged_sessions;
CREATE TRIGGER privileged_sessions_events
    AFTER INSERT OR UPDATE ON privileged_sessions
    FOR EACH ROW EXECUTE FUNCTION authz_session_events();
DROP TRIGGER IF EXISTS break_glass_sessions_events ON break_glass_sessions;
CREATE TRIGGER break_glass_sessions_events
    AFTER INSERT OR UPDATE ON break_glass_sessions
    FOR EACH ROW EXECUTE FUNCTION authz_session_events();
DROP TRIGGER IF EXISTS support_sessions_events ON support_sessions;
CREATE TRIGGER support_sessions_events
    AFTER INSERT OR UPDATE ON support_sessions
    FOR EACH ROW EXECUTE FUNCTION authz_session_events();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT, UPDATE ON privileged_sessions, break_glass_sessions, support_sessions TO app_authorization;
    END IF;
END
$$;
