DROP TRIGGER IF EXISTS trg_reject_template_version_mutation ON template_versions;
DROP FUNCTION IF EXISTS reject_template_version_mutation();
DROP TABLE IF EXISTS template_versions;
DROP TABLE IF EXISTS template_definitions;
