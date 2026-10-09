-- 000008_add_precedence_metadata.down.sql
ALTER TABLE jurisdiction_rules DROP COLUMN IF EXISTS precedence_level;
DROP INDEX IF EXISTS idx_jurisdiction_rules_precedence;