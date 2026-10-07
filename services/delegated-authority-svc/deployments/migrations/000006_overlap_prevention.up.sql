-- Overlap prevention: EXCLUDE constraint using GiST to prevent concurrent
-- delegations for the same (delegate, action_type) on the same entity
-- with overlapping time windows.
--
-- ORG-06 §4.4: "A delegate cannot hold two ACTIVE delegations for the same
-- action on the same entity at the same time."
--
-- This requires the btree_gist extension for the equality operators on
-- the text columns.
--
-- NOT NOT VALID. An earlier version marked this constraint NOT VALID "to avoid
-- rejecting existing data", but Postgres does not support NOT VALID on an
-- EXCLUDE constraint (SQLSTATE 0A000), so the migration could not apply on any
-- database. An EXCLUDE constraint is backed by an index and is validated when
-- it is created: if a database already holds overlapping ACTIVE grants this
-- migration fails, and those rows must be resolved first:
--
--   SELECT a.delegation_id, b.delegation_id
--   FROM delegation_grants a JOIN delegation_grants b
--     ON a.delegation_id < b.delegation_id
--    AND a.tenant_id = b.tenant_id AND a.legal_entity_id = b.legal_entity_id
--    AND a.delegate_principal_id = b.delegate_principal_id
--    AND a.action_type = b.action_type
--    AND a.status = 'ACTIVE' AND b.status = 'ACTIVE'
--    AND tstzrange(a.effective_from, a.effective_to, '[]')
--        && tstzrange(b.effective_from, b.effective_to, '[]');
--
-- tstzrange, not tsrange: the columns are TIMESTAMPTZ, and converting them to
-- a plain timestamp depends on the session TimeZone, which Postgres refuses in
-- an index expression.

CREATE EXTENSION IF NOT EXISTS btree_gist;

-- The EXCLUDE constraint: for the same tenant, legal entity, delegate,
-- and action_type, the effective_from/effective_to ranges must not overlap.
-- Only ACTIVE delegations are considered for the overlap check.
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_no_overlapping_active;
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_no_overlapping_active
    EXCLUDE USING GIST (
        tenant_id WITH =,
        legal_entity_id WITH =,
        delegate_principal_id WITH =,
        action_type WITH =,
        tstzrange(effective_from, effective_to, '[]') WITH &&
    ) WHERE (status = 'ACTIVE');
