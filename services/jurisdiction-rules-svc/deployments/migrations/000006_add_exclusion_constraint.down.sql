-- 000006_add_exclusion_constraint.down.sql
ALTER TABLE jurisdiction_rules DROP CONSTRAINT IF EXISTS excl_no_overlapping_live_rules;
ALTER TABLE jurisdiction_rules DROP COLUMN IF EXISTS effective_period;