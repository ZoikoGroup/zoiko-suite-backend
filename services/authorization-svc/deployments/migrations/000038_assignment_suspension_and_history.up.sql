-- 000038: the ORG-06 role half on role assignments (IAM-13).
--
-- ORG-06 names four operations on business-role assignments. Two were already
-- served: AssignBusinessRole is POST /v1/admin/role-assignments and
-- ListPrincipalAssignments is GET /v1/admin/role-assignments?principal_id=.
-- The other two had nothing under them:
--
--   SuspendAssignment   an assignment could only be ended. A suspension stops
--                       it granting while it stays on record, and can be
--                       lifted; suspended_at is the state (NULL = not
--                       suspended), the grant queries require it NULL.
--   GetAssignmentsAsOf  an assignment's past states were overwritten in
--                       place — an approval, a revocation, a suspension left
--                       only the latest row — so "who held what on 1 March"
--                       could not be answered. Every insert and real change
--                       now writes the full row to an append-only history,
--                       the way 000022 does for configuration.
--
-- version (API-3): bumped by the 000022 function on every real change, so an
-- If-Match on an assignment mutation has something to match.
--
-- Events: iam.assignment.suspended / iam.assignment.resumed join the 000026
-- granted / revoked pair.

ALTER TABLE principal_role_assignments
    ADD COLUMN IF NOT EXISTS suspended_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS suspended_by      TEXT,
    ADD COLUMN IF NOT EXISTS suspension_reason TEXT,
    ADD COLUMN IF NOT EXISTS version           BIGINT NOT NULL DEFAULT 1;

DROP TRIGGER IF EXISTS principal_role_assignments_bump_version ON principal_role_assignments;
CREATE TRIGGER principal_role_assignments_bump_version
    BEFORE UPDATE ON principal_role_assignments
    FOR EACH ROW EXECUTE FUNCTION authz_config_bump_version();

-- ── history ───────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS principal_role_assignment_history (
    history_id                   BIGSERIAL   PRIMARY KEY,
    principal_role_assignment_id UUID        NOT NULL,
    version                      BIGINT      NOT NULL,
    tenant_id                    UUID,
    snapshot                     JSONB       NOT NULL,
    changed_by                   TEXT,
    correlation_id               TEXT,
    reason                       TEXT,
    recorded_at                  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS idx_pra_history_assignment
    ON principal_role_assignment_history (principal_role_assignment_id, recorded_at);

CREATE OR REPLACE FUNCTION authz_history_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only (% refused)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

DROP TRIGGER IF EXISTS pra_history_no_update ON principal_role_assignment_history;
CREATE TRIGGER pra_history_no_update
    BEFORE UPDATE OR DELETE ON principal_role_assignment_history
    FOR EACH ROW EXECUTE FUNCTION authz_history_immutable();

DROP TRIGGER IF EXISTS pra_history_no_truncate ON principal_role_assignment_history;
CREATE TRIGGER pra_history_no_truncate
    BEFORE TRUNCATE ON principal_role_assignment_history
    FOR EACH STATEMENT EXECUTE FUNCTION authz_history_immutable();

-- The tenant comes from the role, as the 000026 event does; the as-of read
-- joins through the role too, so a row recorded without one is still found.
CREATE OR REPLACE FUNCTION authz_assignment_record_history() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_tenant UUID;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.version = OLD.version THEN
        RETURN NULL; -- no-op update: nothing changed, nothing to record
    END IF;
    SELECT r.tenant_id INTO v_tenant FROM roles r WHERE r.role_id = NEW.role_id;
    INSERT INTO principal_role_assignment_history
        (principal_role_assignment_id, version, tenant_id, snapshot, changed_by, correlation_id, reason)
    VALUES (NEW.principal_role_assignment_id, NEW.version, v_tenant, to_jsonb(NEW),
        NULLIF(current_setting('app.actor_id', true), ''),
        NULLIF(current_setting('app.correlation_id', true), ''),
        NULLIF(current_setting('app.reason', true), ''));
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS pra_record_history ON principal_role_assignments;
CREATE TRIGGER pra_record_history
    AFTER INSERT OR UPDATE ON principal_role_assignments
    FOR EACH ROW EXECUTE FUNCTION authz_assignment_record_history();

-- Backfill: the state of every existing row from when it was made. A row
-- decided after it was made (approved, rejected or expired later) was
-- PENDING_APPROVAL until then, so it gets that state at created_at and its
-- decided state at approval_decided_at. Revocations and scheduled ends are
-- effective-dated on the row itself, so the current row answers them for any
-- earlier instant. A suspension cannot predate this migration.
INSERT INTO principal_role_assignment_history
    (principal_role_assignment_id, version, tenant_id, snapshot, recorded_at)
SELECT a.principal_role_assignment_id, a.version, r.tenant_id,
       to_jsonb(a) || jsonb_build_object('approval_status', 'PENDING_APPROVAL',
                                         'approved_by', NULL, 'approval_decided_at', NULL),
       a.created_at
  FROM principal_role_assignments a
  LEFT JOIN roles r ON r.role_id = a.role_id
 WHERE a.approval_decided_at > a.created_at;

INSERT INTO principal_role_assignment_history
    (principal_role_assignment_id, version, tenant_id, snapshot, recorded_at)
SELECT a.principal_role_assignment_id, a.version, r.tenant_id, to_jsonb(a),
       GREATEST(a.created_at, COALESCE(a.approval_decided_at, a.created_at))
  FROM principal_role_assignments a
  LEFT JOIN roles r ON r.role_id = a.role_id;

-- ── events ────────────────────────────────────────────────────────────────────

CREATE OR REPLACE FUNCTION authz_assignment_events() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_tenant UUID;
    v_payload JSONB;
BEGIN
    SELECT r.tenant_id INTO v_tenant FROM roles r WHERE r.role_id = NEW.role_id;
    v_payload := jsonb_build_object(
        'principal_role_assignment_id', NEW.principal_role_assignment_id,
        'principal_id', NEW.principal_id, 'role_id', NEW.role_id,
        'legal_entity_id', NEW.legal_entity_id, 'book_id', NEW.book_id, 'org_unit_id', NEW.org_unit_id,
        'effective_from', NEW.effective_from, 'effective_to', NEW.effective_to,
        'assigned_by', NEW.assigned_by, 'approved_by', NEW.approved_by,
        'approval_reference', NEW.approval_reference, 'approval_status', NEW.approval_status,
        'suspended_at', NEW.suspended_at, 'suspended_by', NEW.suspended_by,
        'suspension_reason', NEW.suspension_reason, 'version', NEW.version);
    IF TG_OP = 'INSERT' THEN
        IF NEW.approval_status = 'APPROVED' THEN
            PERFORM authz_emit_event('iam.assignment.granted', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
        END IF;
        RETURN NULL;
    END IF;
    IF NEW.approval_status = 'APPROVED' AND OLD.approval_status IS DISTINCT FROM 'APPROVED' THEN
        PERFORM authz_emit_event('iam.assignment.granted', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    END IF;
    IF NEW.approval_status = 'APPROVED' AND NEW.effective_to IS DISTINCT FROM OLD.effective_to AND NEW.effective_to IS NOT NULL THEN
        PERFORM authz_emit_event('iam.assignment.revoked', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    END IF;
    IF OLD.suspended_at IS NULL AND NEW.suspended_at IS NOT NULL THEN
        PERFORM authz_emit_event('iam.assignment.suspended', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    ELSIF OLD.suspended_at IS NOT NULL AND NEW.suspended_at IS NULL THEN
        PERFORM authz_emit_event('iam.assignment.resumed', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    END IF;
    RETURN NULL;
END;
$$;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT ON principal_role_assignment_history TO app_authorization;
        GRANT USAGE ON SEQUENCE principal_role_assignment_history_history_id_seq TO app_authorization;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_app') THEN
        GRANT SELECT, INSERT ON principal_role_assignment_history TO zoiko_app;
        GRANT USAGE ON SEQUENCE principal_role_assignment_history_history_id_seq TO zoiko_app;
    END IF;
END
$$;
