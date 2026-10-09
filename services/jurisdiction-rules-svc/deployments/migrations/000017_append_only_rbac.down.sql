-- 000017_append_only_rbac.down.sql
DROP TRIGGER IF EXISTS trg_rule_status_history_no_delete ON rule_status_history;
DROP FUNCTION IF EXISTS prevent_rule_status_history_delete();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT UPDATE, DELETE ON jurisdiction_rule_drift_events TO app_jurisdiction_rules;
        GRANT DELETE ON rule_status_history TO app_jurisdiction_rules;
    END IF;
END
$$;