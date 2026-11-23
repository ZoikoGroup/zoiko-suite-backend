-- Reverts 000038. Dropping principal_role_assignment_history discards the
-- recorded assignment history; take a copy first if it may be needed as
-- evidence. A suspended assignment grants again once suspended_at is gone.

DROP TRIGGER IF EXISTS pra_record_history ON principal_role_assignments;
DROP TRIGGER IF EXISTS principal_role_assignments_bump_version ON principal_role_assignments;
DROP TABLE IF EXISTS principal_role_assignment_history;
DROP FUNCTION IF EXISTS authz_assignment_record_history();
DROP FUNCTION IF EXISTS authz_history_immutable();

-- 000026's event function: granted / revoked only.
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

ALTER TABLE principal_role_assignments
    DROP COLUMN IF EXISTS suspended_at,
    DROP COLUMN IF EXISTS suspended_by,
    DROP COLUMN IF EXISTS suspension_reason,
    DROP COLUMN IF EXISTS version;
