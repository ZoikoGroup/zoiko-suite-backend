-- Overlap prevention: EXCLUDE constraint using GiST to prevent concurrent
-- delegations for the same (delegate, action_type) on the same entity
-- with overlapping time windows.
--
-- ORG-06 §4.4: "A delegate cannot hold two ACTIVE delegations for the same
-- action on the same entity at the same time."
--
-- This requires the btree_gist extension for the equality operators on
-- the text/uuid columns. The constraint is added NOT VALID to avoid
-- rejecting existing data; VALIDATE CONSTRAINT can be run after cleanup.

CREATE EXTENSION IF NOT EXISTS btree_gist;

-- The EXCLUDE constraint: for the same tenant, legal entity, delegate,
-- and action_type, the effective_from/effective_to ranges must not overlap.
-- Only ACTIVE delegations are considered for the overlap check.
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_no_overlapping_active
    EXCLUDE USING GIST (
        tenant_id WITH =,
        legal_entity_id WITH =,
        delegate_principal_id WITH =,
        action_type WITH =,
        tsrange(effective_from, effective_to, '[]') WITH &&
    ) WHERE (status = 'ACTIVE') NOT VALID;