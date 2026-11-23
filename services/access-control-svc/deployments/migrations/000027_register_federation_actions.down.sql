-- Migration 000027 down. Refused while a role was made from version 3 of any
-- of the templates, or a bundle still names one of the actions: either would
-- be left pointing at something that no longer exists, or holding an
-- unregistered action.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT template_code, ', ') INTO v_used
      FROM role_definitions
     WHERE template_code IN ('IDENTITY_SUPPORT_OPERATOR','IDENTITY_SUPPORT_APPROVER','IDENTITY_ADMINISTRATOR')
       AND template_version = 3;
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000027 down refused: roles were made from version 3 of: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (ARRAY[
         'FEDERATION_READ','FEDERATION_CONFIGURE','FEDERATION_APPROVE',
         'FEDERATION_SUSPEND','FEDERATION_LINK','FEDERATION_PROVISIONING_MANAGE']);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000027 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

-- role_template_versions is append-only by trigger (000009); a down is the one
-- sanctioned removal.
ALTER TABLE role_template_versions DISABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_template_versions
 WHERE template_code IN ('IDENTITY_SUPPORT_OPERATOR','IDENTITY_SUPPORT_APPROVER','IDENTITY_ADMINISTRATOR')
   AND template_version = 3
   AND published_by = 'migration:000027';
ALTER TABLE role_template_versions ENABLE TRIGGER role_template_versions_no_update;

DELETE FROM permission_definitions
 WHERE action_name = ANY (ARRAY[
     'FEDERATION_READ','FEDERATION_CONFIGURE','FEDERATION_APPROVE',
     'FEDERATION_SUSPEND','FEDERATION_LINK','FEDERATION_PROVISIONING_MANAGE'])
   AND description LIKE 'Enforced by identity-context-svc%';
