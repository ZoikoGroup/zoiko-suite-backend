-- 000016_fix_rule_status_history_trigger.up.sql
-- 000007's trigger recorded known_from = NEW.updated_at and
-- changed_by_principal_id = NEW.updated_by_principal_id. Both are NULL on a
-- freshly inserted rule, and both history columns are NOT NULL, so every rule
-- insert failed. It also closed every open history row except the newest,
-- leaving the previous status open forever. A new rule's first row now uses
-- created_at / created_by, and every open row is closed before the new one.

CREATE OR REPLACE FUNCTION record_rule_status_change()
RETURNS TRIGGER AS $$
DECLARE
    known_at TIMESTAMPTZ := COALESCE(NEW.updated_at, NEW.created_at, NOW());
BEGIN
    UPDATE rule_status_history
    SET known_to = known_at
    WHERE jurisdiction_rule_id = NEW.jurisdiction_rule_id
      AND known_to IS NULL;

    INSERT INTO rule_status_history (
        jurisdiction_rule_id, rule_status, effective_from, effective_to,
        known_from, changed_by_principal_id, change_reason, schema_version
    ) VALUES (
        NEW.jurisdiction_rule_id, NEW.rule_status, NEW.effective_from, NEW.effective_to,
        known_at, COALESCE(NEW.updated_by_principal_id, NEW.created_by_principal_id),
        NULL, NEW.schema_version
    );

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
