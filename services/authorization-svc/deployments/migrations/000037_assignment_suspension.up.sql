-- 000037: SuspendAssignment / resume for business role assignments
-- (ZS-SVC-I-001 s4.6 ORG-06: lifecycle Proposed -> Active -> Suspended/Expired/Revoked).
--
-- WHY A STATUS VALUE AND NOT A NEW COLUMN. Every query that confers access from
-- an assignment (the decision path, the delegated-authority ceilings, the
-- delegation source, the limit lookups) already requires
-- approval_status = 'APPROVED'. A suspended assignment is therefore excluded
-- from every one of them the moment its status changes, with no query to
-- forget to update. A separate suspended_at column would have needed each of
-- those queries edited, and the one missed would have kept granting.
--
-- Only APPROVED -> SUSPENDED -> APPROVED is allowed. A suspended assignment is
-- still a held assignment: its own effective window keeps running, it can still
-- be revoked, and it can still expire.
--
-- assignment_suspensions is the append-only evidence of WHEN. principal_role_
-- assignments is updated in place, so without it "was this assignment
-- suspended last Tuesday" has no answer, and GetAssignmentsAsOf could not
-- reconstruct a past authorization.

ALTER TABLE principal_role_assignments DROP CONSTRAINT IF EXISTS pra_approval_status_check;
ALTER TABLE principal_role_assignments
    ADD CONSTRAINT pra_approval_status_check
    CHECK (approval_status IN ('APPROVED', 'PENDING_APPROVAL', 'REJECTED', 'EXPIRED', 'SUSPENDED'));

CREATE TABLE IF NOT EXISTS assignment_suspensions (
    suspension_id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_role_assignment_id UUID        NOT NULL REFERENCES principal_role_assignments (principal_role_assignment_id),
    tenant_id                    UUID        NOT NULL,

    suspended_by                 TEXT        NOT NULL,
    suspended_at                 TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    suspension_reason            TEXT        NOT NULL,

    resumed_by                   TEXT,
    resumed_at                   TIMESTAMPTZ,
    resume_reason                TEXT,

    CONSTRAINT as_reason_present CHECK (btrim(suspension_reason) <> ''),
    -- A resumption names who and when, or neither.
    CONSTRAINT as_resume_complete CHECK ((resumed_at IS NULL) = (resumed_by IS NULL)),
    CONSTRAINT as_resume_after_suspend CHECK (resumed_at IS NULL OR resumed_at >= suspended_at)
);

-- At most one open suspension per assignment: it is either suspended or it is not.
CREATE UNIQUE INDEX IF NOT EXISTS uq_assignment_one_open_suspension
    ON assignment_suspensions (principal_role_assignment_id) WHERE resumed_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_assignment_suspensions_lookup
    ON assignment_suspensions (tenant_id, principal_role_assignment_id, suspended_at);

ALTER TABLE assignment_suspensions ENABLE ROW LEVEL SECURITY;
ALTER TABLE assignment_suspensions FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON assignment_suspensions;
CREATE POLICY tenant_isolation_policy ON assignment_suspensions
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Evidence is not edited. The only change a row ever sees is being closed: the
-- three resume columns going from empty to filled, once. Nothing else is
-- updatable and nothing is deletable.
CREATE OR REPLACE FUNCTION authz_assignment_suspensions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'assignment_suspensions is append-only (% refused)', TG_OP
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.resumed_at IS NOT NULL
       OR NEW.principal_role_assignment_id IS DISTINCT FROM OLD.principal_role_assignment_id
       OR NEW.tenant_id            IS DISTINCT FROM OLD.tenant_id
       OR NEW.suspended_by         IS DISTINCT FROM OLD.suspended_by
       OR NEW.suspended_at         IS DISTINCT FROM OLD.suspended_at
       OR NEW.suspension_reason    IS DISTINCT FROM OLD.suspension_reason THEN
        RAISE EXCEPTION 'assignment_suspensions is append-only: a suspension can only be closed, once'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS assignment_suspensions_guard ON assignment_suspensions;
CREATE TRIGGER assignment_suspensions_guard
    BEFORE UPDATE OR DELETE ON assignment_suspensions
    FOR EACH ROW EXECUTE FUNCTION authz_assignment_suspensions_guard();

DROP TRIGGER IF EXISTS assignment_suspensions_no_truncate ON assignment_suspensions;
CREATE TRIGGER assignment_suspensions_no_truncate
    BEFORE TRUNCATE ON assignment_suspensions
    FOR EACH STATEMENT EXECUTE FUNCTION authz_assignment_suspensions_guard();

-- Only an APPROVED assignment can be suspended, and only a SUSPENDED one resumed.
-- Enforced here so a write path that skips the store cannot, for example,
-- "resume" a REJECTED assignment into a granting one.
CREATE OR REPLACE FUNCTION authz_assignment_status_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.approval_status = 'SUSPENDED' AND OLD.approval_status <> 'APPROVED' THEN
        RAISE EXCEPTION 'only an APPROVED assignment can be suspended (it is %)', OLD.approval_status
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.approval_status = 'SUSPENDED' AND NEW.approval_status <> 'SUSPENDED'
       AND NEW.approval_status <> 'APPROVED' THEN
        RAISE EXCEPTION 'a SUSPENDED assignment can only be resumed to APPROVED (not %)', NEW.approval_status
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS pra_status_transition ON principal_role_assignments;
CREATE TRIGGER pra_status_transition
    BEFORE UPDATE OF approval_status ON principal_role_assignments
    FOR EACH ROW WHEN (OLD.approval_status IS DISTINCT FROM NEW.approval_status)
    EXECUTE FUNCTION authz_assignment_status_transition();

-- Events. 000026's trigger, extended:
--   iam.assignment.suspended   APPROVED -> SUSPENDED
--   iam.assignment.resumed     SUSPENDED -> APPROVED   (not a second "granted":
--                              nothing new was granted, a withheld grant resumed)
--   iam.assignment.revoked     now also fires for a SUSPENDED assignment whose
--                              end date is set or brought forward
-- iam.assignment.granted is unchanged for every other path.
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
        'approval_reference', NEW.approval_reference, 'approval_status', NEW.approval_status);
    IF TG_OP = 'INSERT' THEN
        IF NEW.approval_status = 'APPROVED' THEN
            PERFORM authz_emit_event('iam.assignment.granted', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
        END IF;
        RETURN NULL;
    END IF;

    IF NEW.approval_status = 'SUSPENDED' AND OLD.approval_status = 'APPROVED' THEN
        PERFORM authz_emit_event('iam.assignment.suspended', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    ELSIF NEW.approval_status = 'APPROVED' AND OLD.approval_status = 'SUSPENDED' THEN
        PERFORM authz_emit_event('iam.assignment.resumed', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    ELSIF NEW.approval_status = 'APPROVED' AND OLD.approval_status IS DISTINCT FROM 'APPROVED' THEN
        PERFORM authz_emit_event('iam.assignment.granted', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    END IF;
    IF NEW.approval_status IN ('APPROVED', 'SUSPENDED')
       AND NEW.effective_to IS DISTINCT FROM OLD.effective_to AND NEW.effective_to IS NOT NULL THEN
        PERFORM authz_emit_event('iam.assignment.revoked', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    END IF;
    RETURN NULL;
END;
$$;

-- The application role reads, files and closes suspensions; it never deletes
-- one (the guard trigger refuses it as well).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT, UPDATE ON assignment_suspensions TO app_authorization;
    END IF;
END
$$;
