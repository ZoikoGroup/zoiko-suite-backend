-- Migration 000021 down. Refused while any role was made from one of these
-- templates, or any bundle names one of these actions: a role would be left
-- pointing at a template that no longer exists, or holding an unregistered
-- action.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT template_code, ', ') INTO v_used
      FROM role_definitions
     WHERE template_code = ANY (ARRAY['WORKFLOW_APPROVER','WORKFLOW_REQUESTER','WORKFLOW_ADMINISTRATOR','OBLIGATION_MANAGER',
                                      'COMPLIANCE_CATALOGUE_ADMINISTRATOR','IDENTITY_SUPPORT_OPERATOR','IDENTITY_SUPPORT_APPROVER',
                                      'IDENTITY_ADMINISTRATOR'])
        OR (template_code = 'VIEWER_AUDITOR_READ_ONLY' AND template_version = 2);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000021 down refused: roles were made from: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (ARRAY['WORKFLOW_READ','WORKFLOW_CANCEL','WORKFLOW_ESCALATE','WORKFLOW_ESCALATE_TO','WORKFLOW_INVALIDATE',
                          'WORKFLOW_ADMINISTER','WORKFLOW_DEFINE','OBLIGATION_DEFINITION_READ','OBLIGATION_DEFINITION_MANAGE',
                          'OBLIGATION_ENTITY_MAPPING_MANAGE','FILING_REQUIREMENT_STATUS_UPDATE','EVIDENCE_EVALUATE',
                          'EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ','IDENTITY_SESSION_READ','IDENTITY_SESSION_INVALIDATE',
                          'IDENTITY_PRINCIPAL_READ','IDENTITY_PRINCIPAL_STATUS_SET']);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000021 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

-- role_template_versions is append-only by trigger (000009); a down is the one
-- sanctioned removal.
ALTER TABLE role_template_versions DISABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_template_versions WHERE published_by = 'migration:000021';
ALTER TABLE role_template_versions ENABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_templates
 WHERE template_code = ANY (ARRAY['WORKFLOW_APPROVER','WORKFLOW_REQUESTER','WORKFLOW_ADMINISTRATOR','OBLIGATION_MANAGER',
                                  'COMPLIANCE_CATALOGUE_ADMINISTRATOR','IDENTITY_SUPPORT_OPERATOR','IDENTITY_SUPPORT_APPROVER',
                                  'IDENTITY_ADMINISTRATOR']);

DELETE FROM permission_definitions
 WHERE action_name = ANY (ARRAY['WORKFLOW_READ','WORKFLOW_CANCEL','WORKFLOW_ESCALATE','WORKFLOW_ESCALATE_TO','WORKFLOW_INVALIDATE',
                                'WORKFLOW_ADMINISTER','WORKFLOW_DEFINE','OBLIGATION_DEFINITION_READ','OBLIGATION_DEFINITION_MANAGE',
                                'OBLIGATION_ENTITY_MAPPING_MANAGE','FILING_REQUIREMENT_STATUS_UPDATE','EVIDENCE_EVALUATE',
                                'EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ','IDENTITY_SESSION_READ','IDENTITY_SESSION_INVALIDATE',
                                'IDENTITY_PRINCIPAL_READ','IDENTITY_PRINCIPAL_STATUS_SET'])
   AND description LIKE 'Enforced by %';
