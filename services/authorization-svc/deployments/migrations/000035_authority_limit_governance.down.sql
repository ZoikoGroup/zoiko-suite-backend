-- Reverts 000035. Refused while any limit is pending, rejected, or awaiting a
-- retirement: without the status column those rows would read as in force.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM authority_limits
                WHERE status IN ('PENDING_APPROVAL', 'REJECTED') OR pending_action IS NOT NULL) THEN
        RAISE EXCEPTION '000035 down refused: authority limits with undecided or rejected changes exist';
    END IF;
END
$$;

-- Retired and superseded versions stop limiting by their end date, which the
-- governed path already set; anything without one is ended now.
UPDATE authority_limits SET effective_to = NOW()
 WHERE status IN ('RETIRED', 'SUPERSEDED') AND (effective_to IS NULL OR effective_to > NOW());

DROP TRIGGER IF EXISTS authority_limits_bump_watermark ON authority_limits;
DROP TRIGGER IF EXISTS authority_limits_events ON authority_limits;
DROP FUNCTION IF EXISTS authz_authority_limit_events();
DROP TRIGGER IF EXISTS authority_limits_record_history ON authority_limits;
DROP TRIGGER IF EXISTS authority_limits_bump_version ON authority_limits;
DROP TRIGGER IF EXISTS authority_limits_independent ON authority_limits;
DROP FUNCTION IF EXISTS authz_authority_limit_independent();
DROP INDEX IF EXISTS idx_authority_limits_status;
DROP INDEX IF EXISTS uq_authority_limits_one_pending_change;
ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_bounds_check;
ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_pending_action_check;
ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_status_check;
ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_last_decision_check;
ALTER TABLE authority_limits
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS last_decision,
    DROP COLUMN IF EXISTS decided_at,
    DROP COLUMN IF EXISTS decided_by,
    DROP COLUMN IF EXISTS request_reason,
    DROP COLUMN IF EXISTS requested_at,
    DROP COLUMN IF EXISTS requested_by,
    DROP COLUMN IF EXISTS supersedes_id,
    DROP COLUMN IF EXISTS pending_action,
    DROP COLUMN IF EXISTS status;
