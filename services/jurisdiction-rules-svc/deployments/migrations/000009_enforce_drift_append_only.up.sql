-- 000009_enforce_drift_append_only.up.sql
-- Make deactivate idempotent and enforce drift history append-only in DB.
-- Per audit: deactivate not idempotent, drift history not append-only in DB.

-- Make deactivate idempotent: add a unique constraint on jurisdiction_id + active_flag=false
-- so a second deactivate of the same jurisdiction is a no-op (ON CONFLICT DO NOTHING).
-- Since active_flag is boolean, we can't do a partial unique index on false.
-- Instead, we use a trigger to prevent multiple deactivations.

-- For drift history append-only: prevent UPDATE and DELETE on drift_events table.
-- Only INSERT is allowed (enforced by trigger).

-- Trigger to prevent multiple deactivations of the same jurisdiction
CREATE OR REPLACE FUNCTION prevent_duplicate_deactivation()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.active_flag = FALSE AND NEW.active_flag = FALSE THEN
        -- Already deactivated, make this a no-op
        RETURN NULL; -- Skip the update
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_prevent_duplicate_deactivation ON jurisdictions;
CREATE TRIGGER trg_prevent_duplicate_deactivation
BEFORE UPDATE ON jurisdictions
FOR EACH ROW EXECUTE FUNCTION prevent_duplicate_deactivation();

-- Trigger to enforce drift_events append-only (no UPDATE, no DELETE)
CREATE OR REPLACE FUNCTION enforce_drift_append_only()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'drift_events is append-only: updates are not permitted';
    ELSIF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'drift_events is append-only: deletions are not permitted';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_enforce_drift_append_only ON jurisdiction_rule_drift_events;
CREATE TRIGGER trg_enforce_drift_append_only
BEFORE UPDATE OR DELETE ON jurisdiction_rule_drift_events
FOR EACH ROW EXECUTE FUNCTION enforce_drift_append_only();

-- Grant permissions
-- The runtime role is app_jurisdiction_rules (create-app-roles.sh). This used to
-- name jurisdiction_rules_app, which nothing creates, so the migration failed
-- and a fresh volume could not initialise. On a fresh volume roles are created
-- after migrations and default privileges cover this table; hence the guard.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT, UPDATE ON jurisdictions TO app_jurisdiction_rules;
    END IF;
END $$;
-- The runtime role is app_jurisdiction_rules (create-app-roles.sh). This used to
-- name jurisdiction_rules_app, which nothing creates, so the migration failed
-- and a fresh volume could not initialise. On a fresh volume roles are created
-- after migrations and default privileges cover this table; hence the guard.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT ON jurisdiction_rule_drift_events TO app_jurisdiction_rules;
    END IF;
END $$;