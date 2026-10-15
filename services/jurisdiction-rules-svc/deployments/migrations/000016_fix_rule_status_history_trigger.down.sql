-- 000016_fix_rule_status_history_trigger.down.sql
-- Restores 000007's function body (which fails on every rule insert).

CREATE OR REPLACE FUNCTION record_rule_status_change()
RETURNS TRIGGER AS $$
BEGIN
    UPDATE rule_status_history
    SET known_to = NEW.updated_at
    WHERE jurisdiction_rule_id = NEW.jurisdiction_rule_id
      AND known_to IS NULL
      AND history_id != (
          SELECT history_id FROM rule_status_history
          WHERE jurisdiction_rule_id = NEW.jurisdiction_rule_id
          ORDER BY known_from DESC LIMIT 1
      );

    INSERT INTO rule_status_history (
        jurisdiction_rule_id, rule_status, effective_from, effective_to,
        known_from, changed_by_principal_id, change_reason, schema_version
    ) VALUES (
        NEW.jurisdiction_rule_id, NEW.rule_status, NEW.effective_from, NEW.effective_to,
        NEW.updated_at, NEW.updated_by_principal_id, NULL, NEW.schema_version
    );

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
