-- 000007_add_bitemporal_replay.up.sql
-- Add bitemporal replay support (known_at, rule_version, status history).
-- Per V-001 §8.1 and ZS-JUR-001 §3: immutable released rule versions with
-- supersedes references, and historical "what did the platform know on date X" queries.

-- Add rule_version column for immutable version tracking
ALTER TABLE jurisdiction_rules
ADD COLUMN IF NOT EXISTS rule_version INTEGER NOT NULL DEFAULT 1;

-- Add supersedes_rule_id to track rule supersession chain
ALTER TABLE jurisdiction_rules
ADD COLUMN IF NOT EXISTS supersedes_rule_id UUID REFERENCES jurisdiction_rules(jurisdiction_rule_id);

-- Create rule_status_history table for bitemporal tracking
-- known_from / known_to (decision_known_at in V-001) tracks when the platform
-- became aware of a rule status, separate from when the rule was effective.
CREATE TABLE IF NOT EXISTS rule_status_history (
    history_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_rule_id UUID NOT NULL REFERENCES jurisdiction_rules(jurisdiction_rule_id),
    rule_status          VARCHAR(32) NOT NULL,
    effective_from       TIMESTAMPTZ NOT NULL,
    effective_to         TIMESTAMPTZ,
    known_from           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    known_to             TIMESTAMPTZ,
    changed_by_principal_id VARCHAR(255) NOT NULL,
    change_reason        TEXT,
    schema_version       VARCHAR(32) NOT NULL DEFAULT '1.0'
);

CREATE INDEX IF NOT EXISTS idx_rule_status_history_rule
    ON rule_status_history (jurisdiction_rule_id, known_from DESC);

CREATE INDEX IF NOT EXISTS idx_rule_status_history_status
    ON rule_status_history (rule_status, known_from DESC);

-- Trigger to automatically record status changes in history
CREATE OR REPLACE FUNCTION record_rule_status_change()
RETURNS TRIGGER AS $$
BEGIN
    -- Close the previous known_to for this rule
    UPDATE rule_status_history
    SET known_to = NEW.updated_at
    WHERE jurisdiction_rule_id = NEW.jurisdiction_rule_id
      AND known_to IS NULL
      AND history_id != (
          SELECT history_id FROM rule_status_history
          WHERE jurisdiction_rule_id = NEW.jurisdiction_rule_id
          ORDER BY known_from DESC LIMIT 1
      );

    -- Insert new status with current known_from
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

DROP TRIGGER IF EXISTS trg_record_rule_status_change ON jurisdiction_rules;
CREATE TRIGGER trg_record_rule_status_change
AFTER INSERT OR UPDATE OF rule_status, effective_from, effective_to, updated_at, updated_by_principal_id
ON jurisdiction_rules
FOR EACH ROW EXECUTE FUNCTION record_rule_status_change();

-- Grant permissions
GRANT SELECT, INSERT, UPDATE ON rule_status_history TO jurisdiction_rules_app;