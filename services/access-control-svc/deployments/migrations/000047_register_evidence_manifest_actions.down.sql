-- Migration 000047 down: withdraw the template versions this migration
-- published, then the two actions. Refused while any bundle, or any template
-- version this migration did not publish, still names one of the actions. A
-- role already instantiated from one of the withdrawn versions keeps its own
-- copy of the actions, which is why a bundle naming one refuses the down.
--
-- role_template_versions is append-only (trigger from 000009). Drop the trigger
-- temporarily so this migration can withdraw its own published versions.

DROP TRIGGER IF EXISTS role_template_versions_no_update ON role_template_versions;

DELETE FROM role_template_versions
 WHERE published_by = 'migration:000047'
   AND (template_code, template_version) IN (('EVIDENCE_MANIFEST_MANAGER', 1), ('EVIDENCE_REQUEST_MANAGER', 2), ('OBLIGATION_MANAGER', 3));
DELETE FROM role_templates WHERE template_code = 'EVIDENCE_MANIFEST_MANAGER'
   AND NOT EXISTS (SELECT 1 FROM role_template_versions WHERE template_code = 'EVIDENCE_MANIFEST_MANAGER');

CREATE TRIGGER role_template_versions_no_update
    BEFORE UPDATE OR DELETE ON role_template_versions
    FOR EACH ROW EXECUTE FUNCTION role_template_versions_immutable();

DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a IN ('EVIDENCE_MANIFEST_GENERATE', 'EVIDENCE_MANIFEST_READ');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000047 down refused: still granted by a bundle: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE a IN ('EVIDENCE_MANIFEST_GENERATE', 'EVIDENCE_MANIFEST_READ');
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000047 down refused: still granted by a role template: %', v_used;
    END IF;
END
$$;

DELETE FROM permission_definitions WHERE action_name IN ('EVIDENCE_MANIFEST_GENERATE', 'EVIDENCE_MANIFEST_READ');
