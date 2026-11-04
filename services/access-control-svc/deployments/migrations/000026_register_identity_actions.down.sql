-- Migration 000026 down. Refused while a role was made from either template
-- version, or a bundle still names one of the actions: either would be left
-- pointing at something that no longer exists, or holding an unregistered action.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT template_code, ', ') INTO v_used
      FROM role_definitions
     WHERE template_code IN ('IDENTITY_SUPPORT_OPERATOR','IDENTITY_SUPPORT_APPROVER','IDENTITY_ADMINISTRATOR')
       AND template_version = 2;
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000026 down refused: roles were made from version 2 of: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (ARRAY[
         'IDENTITY_PRINCIPAL_CREATE','IDENTITY_PASSWORD_RESET_REQUEST',
         'IDENTITY_PASSWORD_RESET_APPROVE','IDENTITY_AUTH_EVENTS_READ','IDENTITY_SUBJECT_MERGE']);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000026 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

-- role_template_versions is append-only by trigger (000009); a down is the one
-- sanctioned removal.
ALTER TABLE role_template_versions DISABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_template_versions
 WHERE template_code IN ('IDENTITY_SUPPORT_OPERATOR','IDENTITY_SUPPORT_APPROVER','IDENTITY_ADMINISTRATOR')
   AND template_version = 2
   AND published_by = 'migration:000026';
ALTER TABLE role_template_versions ENABLE TRIGGER role_template_versions_no_update;

DELETE FROM permission_definitions
 WHERE action_name = ANY (ARRAY[
     'IDENTITY_PRINCIPAL_CREATE','IDENTITY_PASSWORD_RESET_REQUEST',
     'IDENTITY_PASSWORD_RESET_APPROVE','IDENTITY_AUTH_EVENTS_READ','IDENTITY_SUBJECT_MERGE'])
   AND description LIKE 'Enforced by identity-context-svc%';
