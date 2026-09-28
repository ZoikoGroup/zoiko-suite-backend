-- Down migration for 000006_overlap_prevention.up.sql

ALTER TABLE delegation_grants
    DROP CONSTRAINT IF EXISTS delegation_grants_no_overlapping_active;

-- Note: we don't drop the btree_gist extension as other tables might use it.