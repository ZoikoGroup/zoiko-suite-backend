-- Migration 000023 down. Refused while a role was made from the template, or a
-- bundle still names the action: either would be left pointing at something
-- that no longer exists, or holding an unregistered action.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT template_code, ', ') INTO v_used
      FROM role_definitions
     WHERE template_code = 'JURISDICTION_COMPLIANCE_OFFICER';
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000023 down refused: roles were made from: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = 'JURISDICTION_RULE_VALIDATE';
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000023 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

-- role_template_versions is append-only by trigger (000009); a down is the one
-- sanctioned removal.
ALTER TABLE role_template_versions DISABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_template_versions
 WHERE template_code = 'JURISDICTION_COMPLIANCE_OFFICER'
   AND published_by = 'migration:000023';
ALTER TABLE role_template_versions ENABLE TRIGGER role_template_versions_no_update;

DELETE FROM role_templates WHERE template_code = 'JURISDICTION_COMPLIANCE_OFFICER';

DELETE FROM permission_definitions
 WHERE action_name = 'JURISDICTION_RULE_VALIDATE'
   AND description LIKE 'Enforced by %';
