-- 000015_ownership_supersede_end.up.sql
--
-- ORG-04 wave 2: SupersedeRelationship and EndRelationship (ZS-SVC-I-001 s4.4).
--
-- SUPERSEDE is a proposal like any other: a new PROPOSED row that names the
-- relationship it replaces. When a different principal approves it, the new row
-- becomes ACTIVE and the old one SUPERSEDED, in one transaction, with its
-- valid_to cut to the new row's valid_from. Nothing is edited in place and the
-- old fact stays retrievable (s4.4 "Historical versions remain retrievable").
--
-- END records that an ACTIVE relationship stopped. It names who, when, why and
-- on what evidence, because an end is as material as a start.

ALTER TABLE ownership_relationships
    ADD COLUMN supersedes_relationship_id UUID,
    ADD COLUMN ended_by_principal_id      VARCHAR(255),
    ADD COLUMN ended_at                   TIMESTAMP WITH TIME ZONE,
    ADD COLUMN end_reason                 TEXT,
    ADD COLUMN end_evidence_ref           TEXT;

-- A tenant-qualified key, so a supersession can only name a relationship of the
-- same tenant (the same reason the entity keys include tenant_id).
ALTER TABLE ownership_relationships
    ADD CONSTRAINT or_tenant_relationship_unique UNIQUE (tenant_id, relationship_id);

ALTER TABLE ownership_relationships
    ADD CONSTRAINT or_supersedes_in_tenant FOREIGN KEY (tenant_id, supersedes_relationship_id)
        REFERENCES ownership_relationships (tenant_id, relationship_id),
    ADD CONSTRAINT or_supersedes_not_self CHECK (supersedes_relationship_id IS DISTINCT FROM relationship_id),
    -- An ENDED row says who ended it, when, why and on what evidence; no other
    -- row carries any of it.
    ADD CONSTRAINT or_end_recorded CHECK (
        (status = 'ENDED'
            AND ended_by_principal_id IS NOT NULL AND ended_at IS NOT NULL
            AND btrim(COALESCE(end_reason, '')) <> '' AND btrim(COALESCE(end_evidence_ref, '')) <> '')
        OR (status <> 'ENDED'
            AND ended_by_principal_id IS NULL AND ended_at IS NULL
            AND end_reason IS NULL AND end_evidence_ref IS NULL));

-- One open replacement per relationship: a second maker cannot queue a competing
-- supersession behind the first.
CREATE UNIQUE INDEX or_one_pending_supersession
    ON ownership_relationships (tenant_id, supersedes_relationship_id)
    WHERE status = 'PROPOSED' AND supersedes_relationship_id IS NOT NULL;
