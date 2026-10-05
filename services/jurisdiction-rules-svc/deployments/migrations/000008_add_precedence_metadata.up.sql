-- 000008_add_precedence_metadata.up.sql
-- Add explicit precedence metadata and support for CONFLICTED/INSUFFICIENT outcomes.
-- Per V-001 §8.2: the rule resolution must detect and surface conflicts rather than
-- silently applying nearest-wins.

-- Add precedence_level to jurisdiction_rules for explicit ordering
-- Lower number = higher precedence (e.g., 1 = COUNTRY, 2 = STATE, 3 = TAX_AUTHORITY)
ALTER TABLE jurisdiction_rules
ADD COLUMN IF NOT EXISTS precedence_level INTEGER NOT NULL DEFAULT 0;

-- Add index for precedence ordering
CREATE INDEX IF NOT EXISTS idx_jurisdiction_rules_precedence
    ON jurisdiction_rules (jurisdiction_id, rule_domain, rule_code, precedence_level);

-- Grant permissions
-- The runtime role is app_jurisdiction_rules (create-app-roles.sh). This used to
-- name jurisdiction_rules_app, which nothing creates, so the migration failed
-- and a fresh volume could not initialise. On a fresh volume roles are created
-- after migrations and default privileges cover this table; hence the guard.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT, UPDATE ON jurisdiction_rules TO app_jurisdiction_rules;
    END IF;
END $$;