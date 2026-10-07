-- Migration: 000016_add_scope_dimensions_and_authority_limits.down.sql
--
-- Drops authority_limits table and hierarchical scope columns from
-- delegated_authorities and principal_role_assignments.

BEGIN;

DROP TABLE IF EXISTS authority_limits;

DROP INDEX IF EXISTS idx_delegations_scope_lookup;
ALTER TABLE delegated_authorities DROP COLUMN IF EXISTS org_unit_id;
ALTER TABLE delegated_authorities DROP COLUMN IF EXISTS book_id;

DROP INDEX IF EXISTS idx_assignments_scope_lookup;
ALTER TABLE principal_role_assignments DROP COLUMN IF EXISTS org_unit_id;
ALTER TABLE principal_role_assignments DROP COLUMN IF EXISTS book_id;

COMMIT;
