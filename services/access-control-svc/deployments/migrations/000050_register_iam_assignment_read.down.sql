-- Migration 000050 down: withdraw the template version this migration
-- published, then the action. Refused while any bundle or any other template
-- version still names the action.
--
-- role_template_versions is append-only (trigger from 000009). Drop the trigger
-- temporarily so this migration can withdraw its own published version.

DROP TRIGGER IF EXISTS role_template_versions_no_update ON role_template_versions;

DELETE FROM role_template_versions
 WHERE published_by = 'migration:000050'
   AND (template_code, template_version) IN (('IAM_ACCESS_ADMINISTRATOR', 2));

CREATE TRIGGER role_template_versions_no_update
    BEFORE UPDATE OR DELETE ON role_template_versions
    FOR EACH ROW EXECUTE FUNCTION role_template_versions_immutable();

DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = 'iam.assignment.read';
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000050 down refused: still granted by a bundle: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a = 'iam.assignment.read';
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000050 down refused: still granted by a role template: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions WHERE action_name = 'iam.assignment.read';
