-- Migration 000024 down. Refused while a role was made from either template,
-- or a bundle still names one of the actions: either would be left pointing at
-- something that no longer exists, or holding an unregistered action.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT template_code, ', ') INTO v_used
      FROM role_definitions
     WHERE template_code IN ('SECURITY_OPERATIONS_ANALYST', 'SECURITY_SIEM_ADMINISTRATOR');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000024 down refused: roles were made from: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (ARRAY['SIEM_EXPORTER_CREATE', 'SIEM_EXPORTER_READ', 'SIEM_EVENT_READ']);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000024 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

-- role_template_versions is append-only by trigger (000009); a down is the one
-- sanctioned removal.
ALTER TABLE role_template_versions DISABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_template_versions
 WHERE template_code IN ('SECURITY_OPERATIONS_ANALYST', 'SECURITY_SIEM_ADMINISTRATOR')
   AND published_by = 'migration:000024';
ALTER TABLE role_template_versions ENABLE TRIGGER role_template_versions_no_update;

DELETE FROM role_templates WHERE template_code IN ('SECURITY_OPERATIONS_ANALYST', 'SECURITY_SIEM_ADMINISTRATOR');

DELETE FROM permission_definitions
 WHERE action_name = ANY (ARRAY['SIEM_EXPORTER_CREATE', 'SIEM_EXPORTER_READ', 'SIEM_EVENT_READ'])
   AND description LIKE 'Enforced by %';
