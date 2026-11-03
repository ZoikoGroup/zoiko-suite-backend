-- 000015 down. Refused while any relationship was superseded or ended: dropping
-- the columns would erase who ended it and on what evidence.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM ownership_relationships
                WHERE supersedes_relationship_id IS NOT NULL OR status IN ('SUPERSEDED', 'ENDED')) THEN
        RAISE EXCEPTION '000015 down refused: superseded or ended relationships exist and their history would be lost';
    END IF;
END
$$;

DROP INDEX IF EXISTS or_one_pending_supersession;
ALTER TABLE ownership_relationships
    DROP CONSTRAINT IF EXISTS or_end_recorded,
    DROP CONSTRAINT IF EXISTS or_supersedes_not_self,
    DROP CONSTRAINT IF EXISTS or_supersedes_in_tenant,
    DROP CONSTRAINT IF EXISTS or_tenant_relationship_unique,
    DROP COLUMN IF EXISTS end_evidence_ref,
    DROP COLUMN IF EXISTS end_reason,
    DROP COLUMN IF EXISTS ended_at,
    DROP COLUMN IF EXISTS ended_by_principal_id,
    DROP COLUMN IF EXISTS supersedes_relationship_id;
