-- Migration 000023 down. Refused while any role was made from one of these templates (or the read-only archetype version 4), or any
-- bundle names one of these actions.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT template_code, ', ') INTO v_used
      FROM role_definitions
     WHERE template_code IN ('GL_MIGRATION_PREPARER','GL_MIGRATION_AUTHORITY','GL_MIGRATION_POSTER')
        OR (template_code = 'VIEWER_AUDITOR_READ_ONLY' AND template_version = 4);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000023 down refused: roles were made from: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a IN ('GL_MIGRATION_STAGE','GL_MIGRATION_READ','GL_MIGRATION_APPROVE','GL_MIGRATION_POST','GL_MIGRATION_RECONCILE');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000023 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

ALTER TABLE role_template_versions DISABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_template_versions WHERE published_by = 'migration:000023';
ALTER TABLE role_template_versions ENABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_templates WHERE template_code IN ('GL_MIGRATION_PREPARER','GL_MIGRATION_AUTHORITY','GL_MIGRATION_POSTER');
DELETE FROM permission_definitions
 WHERE description LIKE 'Enforced by general-ledger-svc:%'
   AND action_name IN ('GL_MIGRATION_STAGE','GL_MIGRATION_READ','GL_MIGRATION_APPROVE','GL_MIGRATION_POST','GL_MIGRATION_RECONCILE');
