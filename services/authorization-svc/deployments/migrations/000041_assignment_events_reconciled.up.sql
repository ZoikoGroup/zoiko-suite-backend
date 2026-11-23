-- 000041: one authz_assignment_events() for both suspension migrations.
--
-- 000037_assignment_suspension and 000038_assignment_suspension_and_history
-- were written on two branches, and each replaces authz_assignment_events().
-- Which definition survived depended on the order they ran: on a database
-- built from scratch (filename order) 000038's won, and it keys suspension on
-- suspended_at, so the APPROVED <-> SUSPENDED transitions the store makes
-- emitted no iam.assignment.suspended / .resumed, and a resume emitted a
-- second iam.assignment.granted. On a database that had 000038 before 000037
-- arrived, 000037's won, without the version and suspension fields in the
-- payload.
--
-- This is the definition for both: the status transitions of 000037 (a resume
-- is not a second grant; a SUSPENDED assignment whose end is set is revoked),
-- with 000038's payload (the suspension columns, which the store now keeps in
-- step with the status, and the API-3 version). It runs after both, so every
-- database ends with the same function.

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
