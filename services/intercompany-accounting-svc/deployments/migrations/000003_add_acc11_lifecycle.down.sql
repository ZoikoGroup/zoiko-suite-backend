ALTER TABLE intercompany_entries DROP CONSTRAINT IF EXISTS chk_match_status;
ALTER TABLE intercompany_entries
    DROP COLUMN IF EXISTS acknowledged_at,
    DROP COLUMN IF EXISTS acknowledged_by_principal_id,
    DROP COLUMN IF EXISTS disputed_at,
    DROP COLUMN IF EXISTS disputed_by_principal_id,
    DROP COLUMN IF EXISTS dispute_reason,
    DROP COLUMN IF EXISTS resolved_at,
    DROP COLUMN IF EXISTS resolved_by_principal_id,
    DROP COLUMN IF EXISTS resolution_note;
