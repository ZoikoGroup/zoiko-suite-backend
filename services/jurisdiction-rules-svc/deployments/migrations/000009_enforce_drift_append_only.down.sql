-- 000009_enforce_drift_append_only.down.sql
DROP TRIGGER IF EXISTS trg_prevent_duplicate_deactivation ON jurisdictions;
DROP FUNCTION IF EXISTS prevent_duplicate_deactivation();

DROP TRIGGER IF EXISTS trg_enforce_drift_append_only ON jurisdiction_rule_drift_events;
DROP FUNCTION IF EXISTS enforce_drift_append_only();