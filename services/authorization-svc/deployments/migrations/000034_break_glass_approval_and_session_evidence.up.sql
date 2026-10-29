-- 000034: break-glass needs independent approval before it is active (AU-02),
-- and every decision made inside a JIT, break-glass or support session names
-- that session (AU-13).
--
-- AU-02. A break-glass session was issued ACTIVE with approved_by empty: the
-- requester's own step-up was the only control between "I declare an incident"
-- and emergency powers. GOV-12's state model is Requested -> Approved -> Active
-- -> Expired/Revoked -> ReviewPending -> Closed and its first negative path is
-- "requester cannot self-approve break glass"; Doc 05 §5.5 asks for "elevated
-- approval". Decision D-3 (14 Oct 2026): an independent, stepped-up approver
-- before ACTIVE, with a quorum knob that defaults to one.
--
--   break_glass_sessions   PENDING_APPROVAL and REJECTED states;
--                          approvals_required (the quorum in force when the
--                          request was made); approved_by / approved_at (the
--                          approval that activated it); rejected_by /
--                          rejected_at; request_expires_at (an unanswered
--                          request lapses).
--   break_glass_approvals  one row per approver, with the step-up they proved.
--                          The primary key makes a second approval by the same
--                          person impossible, so a quorum of two is two people.
--
-- The duration still starts at activation: expires_at is recomputed when the
-- quorum is met, exactly as a JIT approval does.
--
-- AU-13. Decision evidence carried no structured session reference — a
-- break-glass grant was findable only by parsing decision_basis — so "the
-- commands executed under this session" could not be listed for its review
-- (ZS-IAM-001 §13-15, §20; GOV-12 evidence "session ID, commands executed").
-- access_decision_log gains session_kind / session_id, indexed per tenant.

-- ── break_glass_sessions ────────────────────────────────────────────────────
ALTER TABLE break_glass_sessions
    ADD COLUMN IF NOT EXISTS approvals_required SMALLINT    NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS approved_by        TEXT,
    ADD COLUMN IF NOT EXISTS approved_at        TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS rejected_by        TEXT,
    ADD COLUMN IF NOT EXISTS rejected_at        TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS request_expires_at TIMESTAMPTZ;

ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_status_check;
ALTER TABLE break_glass_sessions ADD CONSTRAINT break_glass_sessions_status_check
    CHECK (status IN ('PENDING_APPROVAL', 'ACTIVE', 'REJECTED', 'REVOKED', 'EXPIRED'));
ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_approvals_required_check;
ALTER TABLE break_glass_sessions ADD CONSTRAINT break_glass_sessions_approvals_required_check
    CHECK (approvals_required BETWEEN 1 AND 5);
-- GOV-12 #1: never the subject, never the requester.
ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_independent_approval;
ALTER TABLE break_glass_sessions ADD CONSTRAINT break_glass_sessions_independent_approval
    CHECK (approved_by IS NULL OR (approved_by <> principal_id AND approved_by IS DISTINCT FROM requested_by));
ALTER TABLE break_glass_sessions DROP CONSTRAINT IF EXISTS break_glass_sessions_independent_rejection;
ALTER TABLE break_glass_sessions ADD CONSTRAINT break_glass_sessions_independent_rejection
    CHECK (rejected_by IS NULL OR (rejected_by <> principal_id AND rejected_by IS DISTINCT FROM requested_by));

COMMENT ON COLUMN break_glass_sessions.approvals_required IS
    'Independent approvals needed before ACTIVE (AUTHZ_BREAK_GLASS_APPROVALS when requested; GOV-12, D-3).';

CREATE TABLE IF NOT EXISTS break_glass_approvals (
    session_id                UUID        NOT NULL REFERENCES break_glass_sessions (session_id),
    tenant_id                 UUID        NOT NULL,
    approver_id               TEXT        NOT NULL,
    approved_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    approver_assurance        TEXT,
    approver_authn_at         TIMESTAMPTZ,
    approver_identity_session TEXT,
    PRIMARY KEY (session_id, approver_id)
);

COMMENT ON TABLE break_glass_approvals IS
    'One row per independent approver of a break-glass request, with the step-up they proved (GOV-12 CheckerApprovalRecorded).';

ALTER TABLE break_glass_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE break_glass_approvals FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON break_glass_approvals;
CREATE POLICY tenant_isolation_policy ON break_glass_approvals
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- The approver's independence is held here as well as in the handler and the
-- store: an approval row naming the session's subject or requester is refused
-- whichever path wrote it.
CREATE OR REPLACE FUNCTION authz_break_glass_approval_independent() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_subject   TEXT;
    v_requester TEXT;
BEGIN
    SELECT principal_id, requested_by INTO v_subject, v_requester
      FROM break_glass_sessions WHERE session_id = NEW.session_id;
    IF NEW.approver_id = v_subject OR NEW.approver_id IS NOT DISTINCT FROM v_requester THEN
        RAISE EXCEPTION 'break-glass approver must be independent of the subject and the requester'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS break_glass_approvals_independent ON break_glass_approvals;
CREATE TRIGGER break_glass_approvals_independent
    BEFORE INSERT ON break_glass_approvals
    FOR EACH ROW EXECUTE FUNCTION authz_break_glass_approval_independent();

-- Approvals are evidence: append-only.
CREATE OR REPLACE FUNCTION authz_break_glass_approvals_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'break_glass_approvals is append-only (% refused)', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
DROP TRIGGER IF EXISTS break_glass_approvals_no_change ON break_glass_approvals;
CREATE TRIGGER break_glass_approvals_no_change
    BEFORE UPDATE OR DELETE ON break_glass_approvals
    FOR EACH ROW EXECUTE FUNCTION authz_break_glass_approvals_immutable();

-- ── §23 / GOV-12 events: requested and rejected join started / ended ────────
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
    ELSIF TG_TABLE_NAME = 'break_glass_sessions' AND NEW.status = 'PENDING_APPROVAL' AND TG_OP = 'INSERT' THEN
        PERFORM authz_emit_event(v_prefix || '.requested', NEW.session_id::text, NEW.tenant_id, NULL, v_payload);
    ELSIF TG_TABLE_NAME = 'break_glass_sessions' AND TG_OP = 'UPDATE' AND OLD.status = 'PENDING_APPROVAL'
          AND NEW.status IN ('REJECTED', 'EXPIRED') THEN
        PERFORM authz_emit_event(v_prefix || '.' || lower(NEW.status), NEW.session_id::text, NEW.tenant_id, NULL, v_payload);
    END IF;
    RETURN NULL;
END;
$$;

-- GOV-12 CheckerApprovalRecorded, one per approver.
CREATE OR REPLACE FUNCTION authz_break_glass_approval_events() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM authz_emit_event('security.break_glass.approval_recorded', NEW.session_id::text, NEW.tenant_id, NULL,
        jsonb_build_object('session_id', NEW.session_id, 'approver_id', NEW.approver_id,
                           'approved_at', NEW.approved_at, 'approver_assurance', NEW.approver_assurance));
    RETURN NULL;
END;
$$;
DROP TRIGGER IF EXISTS break_glass_approvals_events ON break_glass_approvals;
CREATE TRIGGER break_glass_approvals_events
    AFTER INSERT ON break_glass_approvals
    FOR EACH ROW EXECUTE FUNCTION authz_break_glass_approval_events();

-- ── access_decision_log: the session a decision was made in ─────────────────
ALTER TABLE access_decision_log
    ADD COLUMN IF NOT EXISTS session_kind VARCHAR(16),
    ADD COLUMN IF NOT EXISTS session_id   UUID;

ALTER TABLE access_decision_log DROP CONSTRAINT IF EXISTS access_decision_log_session_kind_check;
ALTER TABLE access_decision_log ADD CONSTRAINT access_decision_log_session_kind_check
    CHECK (session_kind IS NULL OR session_kind IN ('PRIVILEGED', 'BREAK_GLASS', 'SUPPORT'));

CREATE INDEX IF NOT EXISTS idx_access_decision_log_session
    ON access_decision_log (tenant_id, session_id, decided_at DESC)
    WHERE session_id IS NOT NULL;

COMMENT ON COLUMN access_decision_log.session_id IS
    'The JIT, break-glass or support session the decision was made in (AU-13; ZS-IAM-001 §13-15, §20).';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT ON break_glass_approvals TO app_authorization;
    END IF;
END
$$;
