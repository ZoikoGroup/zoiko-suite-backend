-- 000010 down: removes calendars, obligation rules and their pack links.
ALTER TABLE rule_decision_evidence DROP CONSTRAINT IF EXISTS ck_decision_due_has_basis, DROP CONSTRAINT IF EXISTS ck_decision_kind, DROP COLUMN IF EXISTS decision_kind;
DROP TABLE IF EXISTS pack_version_obligations;
DROP TABLE IF EXISTS pack_version_calendars;
DROP TABLE IF EXISTS obligation_rule_sources;
DROP TABLE IF EXISTS obligation_rules;
DROP TABLE IF EXISTS calendar_version_sources;
DROP TABLE IF EXISTS regulatory_holidays;
DROP TABLE IF EXISTS regulatory_calendar_versions;
DROP TABLE IF EXISTS regulatory_calendars;
DROP FUNCTION IF EXISTS jur_obligation_child_guard();
DROP FUNCTION IF EXISTS jur_obligation_guard();
DROP FUNCTION IF EXISTS jur_calendar_child_guard();
DROP FUNCTION IF EXISTS jur_calendar_version_guard();
