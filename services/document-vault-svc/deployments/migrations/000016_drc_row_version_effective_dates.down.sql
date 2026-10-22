DROP TRIGGER IF EXISTS trg_reject_retention_rule_window_mutation ON retention_rule_versions;
DROP FUNCTION IF EXISTS reject_retention_rule_window_mutation();
ALTER TABLE retention_rule_versions DROP CONSTRAINT IF EXISTS retention_rule_effective_window;
ALTER TABLE retention_rule_versions DROP COLUMN IF EXISTS effective_to;
ALTER TABLE retention_rule_versions DROP COLUMN IF EXISTS effective_from;
DROP TRIGGER IF EXISTS trg_bump_document_row_version ON documents;
DROP FUNCTION IF EXISTS bump_document_row_version();
ALTER TABLE documents DROP COLUMN IF EXISTS row_version;
