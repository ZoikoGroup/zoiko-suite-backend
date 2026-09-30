-- 000006_add_exclusion_constraint.up.sql
-- Exclusion constraint to prevent overlapping live rules for the same rule_code.
-- This enforces the check at the database level, eliminating the check-then-act race
-- in hasOverlappingRule (pg_store.go:1125). The constraint applies only to live
-- rules (status NOT IN ('DRAFT', 'RETIRED')).

-- First, create the exclusion constraint using a partial index with the
-- range operator on the effective period (effective_from, effective_to).

-- We use a custom operator class for the tsrange type to enable the
-- exclusion constraint on the half-open interval.

-- Note: PostgreSQL doesn't have a built-in tsrange operator class for
-- EXCLUDE USING GIST, so we use a workaround with a GENERATED column
-- that creates a tsrange from effective_from and effective_to.

ALTER TABLE jurisdiction_rules
ADD COLUMN IF NOT EXISTS effective_period tsrange
GENERATED ALWAYS AS (
    tsrange(effective_from, effective_to, '[)')
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
GRANT SELECT, INSERT, UPDATE ON jurisdiction_rules TO jurisdiction_rules_app;