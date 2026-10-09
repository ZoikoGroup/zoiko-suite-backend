-- 000006_add_exclusion_constraint.up.sql
-- Exclusion constraint to prevent overlapping live rules for the same rule_code.
-- This enforces the check at the database level, eliminating the check-then-act race
-- in hasOverlappingRule (pg_store.go:1125). The constraint applies only to live
-- rules (status NOT IN ('DRAFT', 'RETIRED')).

-- First, create the exclusion constraint using a partial index with the
-- range operator on the effective period (effective_from, effective_to).

-- The effective period as a half-open range, stored so the exclusion
-- constraint can index it. effective_from/effective_to are TIMESTAMPTZ, so the
-- range is tstzrange — this file used tsrange, which has no (timestamptz,
-- timestamptz) form, so it failed and a fresh volume could not initialise.
-- btree_gist supplies the GiST "=" operator classes for jurisdiction_id and
-- rule_code; it was never installed in this database either.
CREATE EXTENSION IF NOT EXISTS btree_gist;

ALTER TABLE jurisdiction_rules
ADD COLUMN IF NOT EXISTS effective_period tstzrange
GENERATED ALWAYS AS (
    tstzrange(effective_from, effective_to, '[)')
) STORED;

-- Create the exclusion constraint
ALTER TABLE jurisdiction_rules
ADD CONSTRAINT excl_no_overlapping_live_rules
EXCLUDE USING GIST (
    jurisdiction_id WITH =,
    rule_code WITH =,
    effective_period WITH &&
)
WHERE (rule_status NOT IN ('DRAFT', 'RETIRED'));

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