-- Migration 000051 down: withdraw the template this migration published.
--
-- role_template_versions is append-only (trigger from 000009). Drop the trigger
-- temporarily so this migration can withdraw its own published version.

DROP TRIGGER IF EXISTS role_template_versions_no_update ON role_template_versions;

DELETE FROM role_template_versions
 WHERE published_by = 'migration:000051'
   AND (template_code, template_version) IN (('GOVERNANCE_LEDGER_WRITER', 1));
DELETE FROM role_templates WHERE template_code = 'GOVERNANCE_LEDGER_WRITER'
   AND NOT EXISTS (SELECT 1 FROM role_template_versions WHERE template_code = 'GOVERNANCE_LEDGER_WRITER');

CREATE TRIGGER role_template_versions_no_update
    BEFORE UPDATE OR DELETE ON role_template_versions
    FOR EACH ROW EXECUTE FUNCTION role_template_versions_immutable();
