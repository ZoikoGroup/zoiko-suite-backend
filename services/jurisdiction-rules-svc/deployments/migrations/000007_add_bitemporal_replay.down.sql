-- 000007_add_bitemporal_replay.down.sql
DROP TRIGGER IF EXISTS trg_record_rule_status_change ON jurisdiction_rules;
DROP FUNCTION IF EXISTS record_rule_status_change();
DROP TABLE IF EXISTS rule_status_history;
ALTER TABLE jurisdiction_rules DROP COLUMN IF EXISTS supersedes_rule_id;
ALTER TABLE jurisdiction_rules DROP COLUMN IF EXISTS rule_version;