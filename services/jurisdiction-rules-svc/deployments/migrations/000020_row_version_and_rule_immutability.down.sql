-- 000020_row_version_and_rule_immutability.down.sql
DROP INDEX IF EXISTS uq_jurisdiction_rules_single_successor;
DROP TRIGGER IF EXISTS trg_jurisdiction_rules_content_guard ON jurisdiction_rules;
DROP FUNCTION IF EXISTS jur_rule_content_guard();
DROP TRIGGER IF EXISTS trg_jurisdiction_rules_row_version ON jurisdiction_rules;
DROP TRIGGER IF EXISTS trg_jurisdictions_row_version ON jurisdictions;
DROP FUNCTION IF EXISTS jur_bump_row_version();
ALTER TABLE jurisdiction_rules DROP COLUMN IF EXISTS row_version;
ALTER TABLE jurisdictions      DROP COLUMN IF EXISTS row_version;
