-- 000037 down. Refused while any assignment is SUSPENDED or any suspension is on
-- record: dropping the state and its evidence would silently turn suspended
-- (non-granting) assignments into rows the old constraint rejects, and erase
-- the record of who suspended what.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM principal_role_assignments WHERE approval_status = 'SUSPENDED')
       OR EXISTS (SELECT 1 FROM assignment_suspensions) THEN
        RAISE EXCEPTION '000037 down refused: suspended assignments or suspension history exist';
    END IF;
END
$$;

DROP TRIGGER IF EXISTS pra_status_transition ON principal_role_assignments;
DROP FUNCTION IF EXISTS authz_assignment_status_transition();

DROP TABLE IF EXISTS assignment_suspensions;
DROP FUNCTION IF EXISTS authz_assignment_suspensions_guard();

ALTER TABLE principal_role_assignments DROP CONSTRAINT IF EXISTS pra_approval_status_check;
ALTER TABLE principal_role_assignments
    ADD CONSTRAINT pra_approval_status_check
    CHECK (approval_status IN ('APPROVED', 'PENDING_APPROVAL', 'REJECTED', 'EXPIRED'));

-- Restore 000026's trigger body.
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
    IF NEW.approval_status = 'APPROVED' AND OLD.approval_status IS DISTINCT FROM 'APPROVED' THEN
        PERFORM authz_emit_event('iam.assignment.granted', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    END IF;
    IF NEW.approval_status = 'APPROVED' AND NEW.effective_to IS DISTINCT FROM OLD.effective_to AND NEW.effective_to IS NOT NULL THEN
        PERFORM authz_emit_event('iam.assignment.revoked', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    END IF;
    RETURN NULL;
END;
$$;
